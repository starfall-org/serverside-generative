import { describe, expect, it } from 'vitest'
import { isTrustedOrigin, parseInvite } from './invite'

const BASE = 'https://studio.example:8443'

describe('invite parsing', () => {
  it('accepts a bare pairing code', () => {
    expect(parseInvite('abc123', BASE)).toMatchObject({
      code: 'abc123',
      pin: '',
      version: '',
      origin: null,
      crossOrigin: false,
      insecure: false,
    })
  })

  it('accepts a full invite URL on the same origin', () => {
    expect(parseInvite(`${BASE}/connect#code=abc&pin=sha256%2Fx&v=1`, BASE)).toMatchObject({
      code: 'abc',
      pin: 'sha256/x',
      version: '1',
      origin: BASE,
      crossOrigin: false,
      insecure: false,
    })
  })

  it('rejects cross-origin, unsupported and empty invites', () => {
    expect(() => parseInvite('https://evil.example/connect#code=abc&v=1', BASE)).toThrow('invite.crossOrigin')
    expect(() => parseInvite(`${BASE}/connect#code=abc&v=2`, BASE)).toThrow('invite.unsupportedVersion')
    expect(() => parseInvite(`${BASE}/settings#code=abc`, BASE)).toThrow('invite.invalidPath')
    expect(() => parseInvite('   ', BASE)).toThrow('invite.empty')
    expect(() => parseInvite(`${BASE}/connect#v=1`, BASE)).toThrow('invite.missingCode')
  })

  it('marks plain HTTP off-loopback as insecure', () => {
    expect(parseInvite('abc', 'http://192.168.1.5:8080').insecure).toBe(true)
    expect(parseInvite('abc', 'http://localhost:8080').insecure).toBe(false)
    expect(isTrustedOrigin('https://anything.example')).toBe(true)
    expect(isTrustedOrigin('ftp://localhost')).toBe(false)
  })
})
