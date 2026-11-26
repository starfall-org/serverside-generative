import type { ListPage, PublicCapabilities, ProviderCatalog, SessionView } from './types'
import {
  parseCapabilities,
  parseListPage,
  parseProviderCatalog,
  parseSessionView,
  isRecord,
} from './contracts'
import { AppError, errorFromStatus, normalizeError } from '../lib/errors'

export interface WebSessionStatus {
  configured: boolean
  authenticated: boolean
  sessionVersion: string
}

export interface RequestOptions {
  signal?: AbortSignal
  timeoutMs?: number
}

const DEFAULT_TIMEOUT = 30_000

function combineSignals(signals: (AbortSignal | undefined)[]): { signal: AbortSignal; cleanup: () => void } {
  const active = signals.filter((s): s is AbortSignal => Boolean(s))
  if (active.length === 0) return { signal: new AbortController().signal, cleanup: () => {} }
  if (active.length === 1) return { signal: active[0]!, cleanup: () => {} }
  const controller = new AbortController()
  const listeners: (() => void)[] = []
  for (const signal of active) {
    if (signal.aborted) {
      controller.abort(signal.reason)
      break
    }
    const listener = () => controller.abort(signal.reason)
    signal.addEventListener('abort', listener, { once: true })
    listeners.push(() => signal.removeEventListener('abort', listener))
  }
  return { signal: controller.signal, cleanup: () => listeners.forEach((remove) => remove()) }
}

function withTimeout(external: AbortSignal | undefined, timeoutMs: number) {
  if (timeoutMs <= 0) {
    const combined = combineSignals([external])
    return { signal: combined.signal, cleanup: combined.cleanup }
  }
  const timeout = new AbortController()
  const timer = setTimeout(() => timeout.abort(new DOMException('Timeout', 'TimeoutError')), timeoutMs)
  const combined = combineSignals([external, timeout.signal])
  return {
    signal: combined.signal,
    cleanup: () => {
      clearTimeout(timer)
      combined.cleanup()
    },
  }
}

async function parseJson(response: Response): Promise<unknown> {
  try {
    return await response.json()
  } catch {
    return null
  }
}

function errorFromBody(body: unknown, status: number): AppError {
  if (isRecord(body) && isRecord(body.error) && typeof body.error.message === 'string') {
    const error = errorFromStatus(
      status,
      typeof body.error.code === 'string' ? body.error.code : '',
      body.error.message,
    )
    return new AppError(error.message, {
      kind: error.kind,
      status,
      code: error.code,
      retryable: body.error.retryable === true || error.retryable,
    })
  }
  return errorFromStatus(status)
}

export class ApiClient {
  private readonly lifetime = new AbortController()

  constructor(
    readonly origin: string,
    private readonly sessionVersion = '',
    private readonly onSessionInvalid?: () => void,
  ) {}

  close(): void {
    this.lifetime.abort()
  }

  private async request(path: string, init: RequestInit, options: RequestOptions = {}): Promise<Response> {
    const external = combineSignals([options.signal, this.lifetime.signal])
    const { signal, cleanup } = withTimeout(external.signal, options.timeoutMs ?? DEFAULT_TIMEOUT)
    try {
      return await fetch(new URL(path, this.origin), {
        ...init,
        signal,
        cache: 'no-store',
        credentials: 'same-origin',
        mode: 'same-origin',
        redirect: 'error',
      })
    } catch (error) {
      if (signal.aborted) {
        const reason = (signal as AbortSignal).reason
        if (reason instanceof DOMException && reason.name === 'TimeoutError') {
          throw new AppError('Request timed out', { kind: 'timeout', cause: error })
        }
        throw new AppError('Request aborted', { kind: 'aborted', cause: error })
      }
      throw normalizeError(error)
    } finally {
      cleanup()
      external.cleanup()
    }
  }

  private async postJson(path: string, body: unknown, options: RequestOptions = {}): Promise<Response> {
    return this.request(
      path,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Accept: 'application/json', 'X-Generative-Web': '1' },
        body: JSON.stringify(body),
      },
      options,
    )
  }

  async status(signal?: AbortSignal): Promise<WebSessionStatus> {
    const response = await this.request('/web/auth/status', {}, { signal })
    const body = await parseJson(response)
    if (!response.ok) throw errorFromBody(body, response.status)
    if (
      !isRecord(body) ||
      typeof body.configured !== 'boolean' ||
      typeof body.authenticated !== 'boolean' ||
      (body.authenticated && typeof body.session_version !== 'string')
    ) {
      throw new AppError('Invalid session status', { kind: 'invalid' })
    }
    return {
      configured: body.configured,
      authenticated: body.authenticated,
      sessionVersion: body.authenticated ? String(body.session_version) : '',
    }
  }

  async bootstrap(signal: AbortSignal): Promise<string> {
    const response = await this.postJson('/web/auth/bootstrap', {}, { signal })
    return this.readSessionVersion(response)
  }

  async login(password: string): Promise<string> {
    const response = await this.postJson('/web/auth/login', { password })
    return this.readSessionVersion(response)
  }

  private async readSessionVersion(response: Response): Promise<string> {
    const body = await parseJson(response)
    if (!response.ok) throw errorFromBody(body, response.status)
    if (!isRecord(body) || typeof body.session_version !== 'string') {
      throw new AppError('Invalid login response', { kind: 'invalid' })
    }
    return body.session_version
  }

  /** Same-origin requests bound to the cookie session that opened the workspace. */
  async authorizedFetch(path: string, init: RequestInit, options: RequestOptions = {}): Promise<Response> {
    if (!this.sessionVersion) throw new AppError('Sign in required', { kind: 'unauthorized' })
    const headers = new Headers(init.headers)
    headers.set('X-Generative-Session', this.sessionVersion)
    if (init.method === 'POST') headers.set('X-Generative-Web', '1')
    const response = await this.request(path, { ...init, headers }, options)
    if (response.status === 401 && !this.lifetime.signal.aborted) this.onSessionInvalid?.()
    return response
  }

  async rpc<T>(
    op: string,
    data: Record<string, unknown> | undefined,
    sessionId = '',
    options: RequestOptions = {},
  ): Promise<T> {
    const payload: Record<string, unknown> = { id: crypto.randomUUID(), op }
    if (sessionId) payload.session_id = sessionId
    if (data !== undefined) payload.data = data
    const response = await this.authorizedFetch(
      '/web/rpc',
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
        body: JSON.stringify(payload),
      },
      options,
    )
    const body = await parseJson(response)
    if (!response.ok) throw errorFromBody(body, response.status)
    if (!isRecord(body) || body.ok !== true) throw errorFromBody(body, response.status || 500)
    return body.data as T
  }

  async capabilities(signal?: AbortSignal): Promise<PublicCapabilities> {
    const value = await this.rpc<unknown>('capabilities.get', undefined, '', { signal })
    const parsed = parseCapabilities(value)
    if (!parsed) throw new AppError('Invalid capabilities response', { kind: 'invalid' })
    return parsed
  }

  async listChats(cursor = '', signal?: AbortSignal): Promise<ListPage> {
    const data = await this.rpc<unknown>(
      'session.list',
      { pagination: 'snapshot', limit: 50, ...(cursor ? { cursor } : {}) },
      '',
      { signal },
    )
    return parseListPage(data)
  }

  async providerCatalog(signal?: AbortSignal): Promise<ProviderCatalog> {
    return parseProviderCatalog(await this.rpc<unknown>('provider.catalog', undefined, '', { signal }))
  }

  async openSession(
    sessionId: string,
    data: Record<string, unknown>,
    signal?: AbortSignal,
  ): Promise<SessionView> {
    const parsed = parseSessionView(await this.rpc<unknown>('session.open', data, sessionId, { signal }))
    if (!parsed) throw new AppError('Invalid session response', { kind: 'invalid' })
    return parsed
  }

  async getSession(sessionId: string, ifRevision?: number, signal?: AbortSignal): Promise<SessionView> {
    const parsed = parseSessionView(
      await this.rpc<unknown>(
        'session.get',
        ifRevision === undefined ? {} : { if_revision: ifRevision },
        sessionId,
        { signal },
      ),
    )
    if (!parsed) throw new AppError('Invalid session response', { kind: 'invalid' })
    return parsed
  }

  async logout(): Promise<void> {
    const response = await this.authorizedFetch('/web/auth/logout', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: '{}',
    })
    if (!response.ok) throw errorFromBody(await parseJson(response), response.status)
  }
}
