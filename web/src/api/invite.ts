export interface Invite {
  code: string
  pin: string
  version: string
  origin: string | null
  crossOrigin: boolean
  insecure: boolean
}

const LOOPBACK = new Set(['localhost', '127.0.0.1', '[::1]', '::1'])

export function isTrustedOrigin(origin: string): boolean {
  try {
    const url = new URL(origin)
    if (url.protocol === 'https:') return true
    return url.protocol === 'http:' && LOOPBACK.has(url.hostname)
  } catch {
    return false
  }
}

export function currentOriginIsTrusted(): boolean {
  return isTrustedOrigin(window.location.origin)
}

/**
 * Accepts a bare pairing code or a full `/connect#code=...&pin=...&v=1` URL.
 * A fragment is never sent to the server; callers must clear it immediately.
 */
export function parseInvite(input: string, baseOrigin = window.location.origin): Invite {
  const text = input.trim()
  if (!text) throw new Error('invite.empty')
  let code = text
  let pin = ''
  let version = ''
  let origin: string | null = null
  let crossOrigin = false

  if (/^https?:\/\//i.test(text)) {
    let url: URL
    try {
      url = new URL(text)
    } catch {
      throw new Error('invite.invalidUrl')
    }
    if (url.pathname !== '/connect' && url.pathname !== '/') throw new Error('invite.invalidPath')
    const fragment = new URLSearchParams(url.hash.slice(1))
    code = fragment.get('code') || ''
    pin = fragment.get('pin') || ''
    version = fragment.get('v') || ''
    origin = url.origin
    crossOrigin = url.origin !== baseOrigin
  }
  if (!code || code.length > 128) throw new Error('invite.missingCode')
  if (version && version !== '1') throw new Error('invite.unsupportedVersion')
  if (crossOrigin) throw new Error('invite.crossOrigin')
  const insecure = !isTrustedOrigin(origin || baseOrigin)
  return { code, pin, version, origin, crossOrigin, insecure }
}

/** Read and immediately strip the invite fragment from the address bar. */
export function consumeInviteFragment(): string {
  const hash = window.location.hash
  if (!hash) return ''
  const value = hash.slice(1)
  window.history.replaceState(null, '', window.location.pathname + window.location.search)
  return value
}
