import type { ApiClient } from './client'
import type { SessionEvent } from './types'
import { parseEvent } from './contracts'
import { AppError, isAbort, normalizeError } from '../lib/errors'

export type StreamTransport = 'sse' | 'poll'

export interface EventStreamOptions {
  client: ApiClient
  sessionId: string
  after: number
  signal: AbortSignal
  onEvent: (event: SessionEvent) => void
  onGap: () => void
  onTransport?: (transport: StreamTransport) => void
  onReconnect?: () => void
  onError?: (error: AppError, willRetry: boolean) => void
}

export interface EventStreamHandle {
  close: () => void
  readonly transport: StreamTransport | null
  readonly done: Promise<void>
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(new DOMException('Aborted', 'AbortError'))
      return
    }
    const timer = setTimeout(() => {
      signal.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    function onAbort() {
      clearTimeout(timer)
      reject(new DOMException('Aborted', 'AbortError'))
    }
    signal.addEventListener('abort', onAbort, { once: true })
  })
}

function backoff(attempt: number): number {
  const base = Math.min(15_000, 500 * 2 ** Math.min(attempt, 5))
  return base + Math.random() * 250
}

function eventsPath(sessionId: string, after: number): string {
  return `/web/events?session_id=${encodeURIComponent(sessionId)}&after=${after}`
}

function errorCode(body: unknown): string {
  if (typeof body === 'object' && body !== null && 'error' in body) {
    const error = (body as { error?: { code?: unknown } }).error
    if (error && typeof error.code === 'string') return error.code
  }
  if (typeof body === 'object' && body !== null && 'code' in body) {
    const code = (body as { code?: unknown }).code
    if (typeof code === 'string') return code
  }
  return ''
}

async function readJson(response: Response): Promise<unknown> {
  try {
    return await response.json()
  } catch {
    return null
  }
}

export function startEventStream(options: EventStreamOptions): EventStreamHandle {
  const controller = new AbortController()
  const signal = controller.signal
  const onOuterAbort = () => controller.abort()
  if (options.signal.aborted) controller.abort()
  else options.signal.addEventListener('abort', onOuterAbort, { once: true })
  let transport: StreamTransport | null = null
  let cursor = options.after
  let closed = false

  const done = (async () => {
    let attempt = 0
    while (!signal.aborted) {
      try {
        const result = await runSse()
        if (result === 'fallback') {
          await runPoll()
          return
        }
        // A clean SSE close still requires reconnecting from the last cursor.
        // A daemon restart resets event_seq, so let the owner re-open the
        // session and hand us a fresh cursor before we subscribe again.
        attempt = 0
        if (!signal.aborted) options.onReconnect?.()
        await sleep(backoff(attempt), signal)
        continue
      } catch (error) {
        if (signal.aborted || isAbort(error)) return
        const appError = normalizeError(error)
        if (appError.kind === 'unauthorized' || appError.kind === 'forbidden') {
          options.onError?.(appError, false)
          return
        }
        if (appError.kind === 'not_found' || appError.kind === 'unsupported') {
          options.onError?.(appError, false)
          return
        }
        attempt += 1
        options.onError?.(appError, true)
        if (!signal.aborted) options.onReconnect?.()
        try {
          await sleep(backoff(attempt), signal)
        } catch {
          return
        }
      }
    }
  })().finally(() => {
    options.signal.removeEventListener('abort', onOuterAbort)
  })

  async function runSse(): Promise<'closed' | 'fallback'> {
    const response = await options.client.authorizedFetch(
      eventsPath(options.sessionId, cursor),
      { headers: { Accept: 'text/event-stream' }, signal },
      { signal, timeoutMs: 0 },
    )
    if (response.status === 401 || response.status === 403) {
      throw new AppError('Event stream unauthorized', {
        kind: response.status === 401 ? 'unauthorized' : 'forbidden',
        status: response.status,
      })
    }
    if (response.status === 409) {
      const body = await readJson(response)
      if (errorCode(body) === 'event_gap' || response.status === 409) {
        options.onGap()
        return 'closed'
      }
    }
    if (!response.ok) {
      const body = await readJson(response)
      throw new AppError(`Event stream failed (${response.status})`, {
        kind: response.status === 404 ? 'not_found' : 'server',
        status: response.status,
        code: errorCode(body),
      })
    }
    const contentType = response.headers.get('Content-Type') || ''
    if (!contentType.includes('text/event-stream')) {
      // A proxy may buffer or rewrite SSE. Fall back to JSON long polling.
      return 'fallback'
    }
    transport = 'sse'
    options.onTransport?.('sse')
    if (!response.body) return 'fallback'
    const reader = response.body.getReader()
    const cancelReader = () => {
      void reader.cancel().catch(() => {})
    }
    if (signal.aborted) cancelReader()
    else signal.addEventListener('abort', cancelReader, { once: true })
    const decoder = new TextDecoder()
    let buffer = ''
    try {
      for (;;) {
        const { value, done: ended } = await reader.read()
        if (ended || signal.aborted) break
        buffer += decoder.decode(value, { stream: true })
        let boundary = buffer.search(/\r?\n\r?\n/)
        while (boundary !== -1 && !signal.aborted) {
          const frame = buffer.slice(0, boundary)
          const consumed = buffer.slice(boundary).match(/^\r?\n\r?\n/)![0].length
          buffer = buffer.slice(boundary + consumed)
          handleFrame(frame)
          boundary = buffer.search(/\r?\n\r?\n/)
        }
      }
    } finally {
      signal.removeEventListener('abort', cancelReader)
      reader.releaseLock()
    }
    return 'closed'

    function handleFrame(frame: string) {
      let eventName = 'message'
      const data: string[] = []
      for (const rawLine of frame.split(/\r?\n/)) {
        if (!rawLine || rawLine.startsWith(':')) continue
        const colon = rawLine.indexOf(':')
        const field = colon === -1 ? rawLine : rawLine.slice(0, colon)
        const value = colon === -1 ? '' : rawLine.slice(colon + 1).replace(/^ /, '')
        if (field === 'event') eventName = value
        else if (field === 'data') data.push(value)
      }
      if (data.length === 0) return
      let parsed: unknown
      try {
        parsed = JSON.parse(data.join('\n'))
      } catch {
        return
      }
      if (eventName === 'error') {
        const code = errorCode(parsed)
        if (code === 'event_gap') options.onGap()
        else options.onError?.(new AppError('Event stream error', { kind: 'server', code }), false)
        return
      }
      const event = parseEvent(parsed)
      if (!event) return
      if (event.seq <= cursor) return
      cursor = event.seq
      if (event.kind === 'event_gap') {
        options.onGap()
        return
      }
      options.onEvent(event)
    }
  }

  async function runPoll(): Promise<void> {
    transport = 'poll'
    options.onTransport?.('poll')
    let attempt = 0
    while (!signal.aborted) {
      try {
        const response = await options.client.authorizedFetch(
          eventsPath(options.sessionId, cursor),
          { headers: { Accept: 'application/json' }, signal },
          { signal, timeoutMs: 30_000 },
        )
        if (response.status === 401 || response.status === 403) {
          throw new AppError('Event poll unauthorized', {
            kind: response.status === 401 ? 'unauthorized' : 'forbidden',
            status: response.status,
          })
        }
        if (response.status === 409) {
          const body = await readJson(response)
          if (errorCode(body) === 'event_gap') {
            options.onGap()
            return
          }
        }
        if (!response.ok) {
          const body = await readJson(response)
          throw new AppError(`Event poll failed (${response.status})`, {
            kind: response.status === 404 ? 'not_found' : 'server',
            status: response.status,
            code: errorCode(body),
          })
        }
        const body = await readJson(response)
        const events =
          typeof body === 'object' && body !== null && 'events' in body && Array.isArray(body.events)
            ? body.events
            : []
        attempt = 0
        for (const raw of events) {
          const event = parseEvent(raw)
          if (!event || event.seq <= cursor) continue
          cursor = event.seq
          if (event.kind === 'event_gap') {
            options.onGap()
            return
          }
          options.onEvent(event)
        }
        if (events.length === 0) await sleep(250, signal)
      } catch (error) {
        if (signal.aborted || isAbort(error)) return
        const appError = normalizeError(error)
        if (appError.kind === 'unauthorized' || appError.kind === 'forbidden') {
          options.onError?.(appError, false)
          return
        }
        if (appError.kind === 'not_found') {
          options.onError?.(appError, false)
          return
        }
        attempt += 1
        options.onError?.(appError, true)
        try {
          await sleep(backoff(attempt), signal)
        } catch {
          return
        }
      }
    }
  }

  return {
    close() {
      closed = true
      controller.abort()
      options.signal.removeEventListener('abort', onOuterAbort)
    },
    get transport() {
      return transport
    },
    get done() {
      return done
    },
    get closed() {
      return closed
    },
  } as EventStreamHandle & { readonly closed: boolean }
}
