import { describe, expect, it, vi } from 'vitest'
import type { ApiClient } from './client'
import { startEventStream, type EventStreamHandle } from './events'
import type { SessionEvent } from './types'

function sseResponse(frames: string[]): Response {
  const encoder = new TextEncoder()
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const frame of frames) controller.enqueue(encoder.encode(frame))
      controller.close()
    },
  })
  return new Response(stream, {
    status: 200,
    headers: { 'Content-Type': 'text/event-stream' },
  })
}

function fakeClient(handler: (accept: string) => Response | Promise<Response>): ApiClient {
  return {
    authorizedFetch: vi.fn(async (_path: string, init: RequestInit) => {
      const accept = new Headers(init.headers).get('Accept') || ''
      return handler(accept)
    }),
  } as unknown as ApiClient
}

describe('event stream', () => {
  it('parses SSE frames, deduplicates sequence numbers and closes cleanly', async () => {
    const event = {
      session_id: 'c',
      seq: 1,
      request_id: 'r',
      kind: 'delta',
      data: { text: 'hi' },
    }
    const frames = [
      `event: message\ndata: ${JSON.stringify(event)}\n\n`,
      `data: ${JSON.stringify(event)}\n\n`,
    ]
    const events: SessionEvent[] = []
    const controller = new AbortController()
    const handleRef: { current?: EventStreamHandle } = {}
    handleRef.current = startEventStream({
      client: fakeClient(() => sseResponse(frames)),
      sessionId: 'c',
      after: 0,
      signal: controller.signal,
      onEvent: (received) => {
        events.push(received)
        handleRef.current?.close()
      },
      onGap: vi.fn(),
    })

    await vi.waitFor(() => expect(events).toHaveLength(1))
    expect(events[0]).toMatchObject({ seq: 1, kind: 'delta', data: { text: 'hi' } })
    handleRef.current.close()
  })

  it('reports an event gap without dispatching the gap as a delta', async () => {
    const onGap = vi.fn()
    const onEvent = vi.fn()
    const controller = new AbortController()
    const handle = startEventStream({
      client: fakeClient(() =>
        sseResponse([
          `event: message\ndata: ${JSON.stringify({ session_id: 'c', seq: 9, kind: 'event_gap', data: {} })}\n\n`,
        ]),
      ),
      sessionId: 'c',
      after: 4,
      signal: controller.signal,
      onEvent,
      onGap,
    })

    await vi.waitFor(() => expect(onGap).toHaveBeenCalledTimes(1))
    expect(onEvent).not.toHaveBeenCalled()
    handle.close()
  })

  it('notifies the owner to refresh its cursor before reconnecting a clean SSE stream', async () => {
    const onReconnect = vi.fn()
    const controller = new AbortController()
    const handle = startEventStream({
      client: fakeClient(() => sseResponse([])),
      sessionId: 'c',
      after: 3,
      signal: controller.signal,
      onEvent: vi.fn(),
      onGap: vi.fn(),
      onReconnect,
    })

    await vi.waitFor(() => expect(onReconnect).toHaveBeenCalled())
    handle.close()
  })

  it('falls back to JSON long polling when a proxy rewrites SSE', async () => {
    const onTransport = vi.fn()
    const controller = new AbortController()
    const handleRef: { current?: EventStreamHandle } = {}
    const client = fakeClient((accept) => {
      if (accept.includes('text/event-stream')) {
        return new Response('{"events":[]}', {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      }
      return new Response(
        JSON.stringify({
          events: [
            {
              session_id: 'c',
              seq: 2,
              request_id: 'r',
              kind: 'delta',
              data: { text: 'polled' },
            },
          ],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      )
    })

    const events: SessionEvent[] = []
    handleRef.current = startEventStream({
      client,
      sessionId: 'c',
      after: 0,
      signal: controller.signal,
      onEvent: (event) => {
        events.push(event)
        handleRef.current?.close()
      },
      onGap: vi.fn(),
      onTransport,
    })

    await vi.waitFor(() => expect(events).toHaveLength(1))
    expect(events[0]?.data).toEqual({ text: 'polled' })
    expect(onTransport).toHaveBeenCalledWith('poll')
    handleRef.current.close()
  })
})
