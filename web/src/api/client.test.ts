import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiClient } from './client'

const origin = 'https://studio.example'
const sessionVersion = 'public-session-version'

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status })
}

describe('ApiClient web sessions', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('sends credentials and same-origin headers for web RPC', async () => {
    const fetchMock = vi.fn(async (url: URL, init: RequestInit) => {
      expect(url.href).toBe(origin + '/web/rpc')
      expect(init.credentials).toBe('same-origin')
      const headers = new Headers(init.headers)
      expect(headers.get('X-Generative-Web')).toBe('1')
      expect(headers.get('X-Generative-Session')).toBe(sessionVersion)
      expect(headers.has('Authorization')).toBe(false)
      expect(JSON.parse(String(init.body))).toMatchObject({
        op: 'session.list',
        data: { pagination: 'snapshot' },
      })
      return json({ ok: true, data: { version: 1, items: [], next_cursor: '' } })
    })
    vi.stubGlobal('fetch', fetchMock)
    await expect(new ApiClient(origin, sessionVersion).listChats()).resolves.toMatchObject({ items: [] })
    expect(fetchMock).toHaveBeenCalledOnce()
  })

  it('reports a changed session without retrying the operation', async () => {
    const fetchMock = vi.fn(async () => json({ error: { code: 'unauthorized', message: 'expired' } }, 401))
    vi.stubGlobal('fetch', fetchMock)
    const invalid = vi.fn()
    const client = new ApiClient(origin, sessionVersion, invalid)
    await expect(client.rpc('session.cancel', { request_id: 'run' }, 'chat')).rejects.toMatchObject({
      status: 401,
    })
    expect(invalid).toHaveBeenCalledOnce()
    expect(fetchMock).toHaveBeenCalledOnce()
  })

  it('keeps the password in the login request body', async () => {
    const fetchMock = vi.fn(async (url: URL, init: RequestInit) => {
      expect(url.href).toBe(origin + '/web/auth/login')
      expect(new Headers(init.headers).get('X-Generative-Web')).toBe('1')
      expect(JSON.parse(String(init.body))).toEqual({ password: 'temporary-password' })
      return json({ session_version: sessionVersion })
    })
    vi.stubGlobal('fetch', fetchMock)
    await expect(new ApiClient(origin).login('temporary-password')).resolves.toBe(sessionVersion)
  })
})
