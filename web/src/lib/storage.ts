import type { StoredIdentity } from '../api/types'
import { isRecord } from '../api/contracts'

const KEY_PREFIX = 'generative.identity.v1.'

function keyFor(origin: string) {
  return `${KEY_PREFIX}${origin}`
}

function parse(value: string | null): StoredIdentity | null {
  if (!value) return null
  try {
    const parsed: unknown = JSON.parse(value)
    if (!isRecord(parsed)) return null
    if (
      parsed.version !== 1 ||
      typeof parsed.origin !== 'string' ||
      typeof parsed.clientId !== 'string' ||
      typeof parsed.secret !== 'string'
    ) {
      return null
    }
    return {
      version: 1,
      origin: parsed.origin,
      clientId: parsed.clientId,
      secret: parsed.secret,
      serverId: typeof parsed.serverId === 'string' ? parsed.serverId : undefined,
      remember: parsed.remember === true,
      pairedAt: typeof parsed.pairedAt === 'number' ? parsed.pairedAt : 0,
    }
  } catch {
    return null
  }
}

export function readIdentity(origin: string): StoredIdentity | null {
  try {
    const local = parse(window.localStorage.getItem(keyFor(origin)))
    if (local) return local
  } catch {
    // Storage can be denied; the caller may still pair for this tab.
  }
  try {
    return parse(window.sessionStorage.getItem(keyFor(origin)))
  } catch {
    return null
  }
}

export function writeIdentity(identity: StoredIdentity): boolean {
  const serialized = JSON.stringify(identity)
  try {
    if (identity.remember) {
      window.localStorage.setItem(keyFor(identity.origin), serialized)
      window.sessionStorage.removeItem(keyFor(identity.origin))
    } else {
      window.sessionStorage.setItem(keyFor(identity.origin), serialized)
      window.localStorage.removeItem(keyFor(identity.origin))
    }
    return true
  } catch {
    // Fall back to tab-scoped memory storage when persistent storage is denied.
    try {
      window.sessionStorage.setItem(keyFor(identity.origin), serialized)
      return true
    } catch {
      return false
    }
  }
}

export function clearIdentity(origin: string): void {
  try {
    window.localStorage.removeItem(keyFor(origin))
  } catch {
    /* ignore */
  }
  try {
    window.sessionStorage.removeItem(keyFor(origin))
  } catch {
    /* ignore */
  }
}

const PREFERENCE_KEY = 'generative.preferences.v1'

export interface Preferences {
  theme: 'light' | 'dark' | 'system'
  language: 'auto' | 'vi' | 'en'
  model?: { source: 'stored' | 'mobile_ephemeral'; providerId: string; modelId: string; keyId?: string }
}

export const defaultPreferences: Preferences = { theme: 'system', language: 'auto' }

export function readPreferences(): Preferences {
  try {
    const parsed: unknown = JSON.parse(window.localStorage.getItem(PREFERENCE_KEY) || 'null')
    if (!isRecord(parsed)) return defaultPreferences
    return {
      theme:
        parsed.theme === 'light' || parsed.theme === 'dark' || parsed.theme === 'system'
          ? parsed.theme
          : 'system',
      language: parsed.language === 'vi' || parsed.language === 'en' ? parsed.language : 'auto',
      model: isRecord(parsed.model) ? (parsed.model as Preferences['model']) : undefined,
    }
  } catch {
    return defaultPreferences
  }
}

export function writePreferences(preferences: Preferences): void {
  try {
    window.localStorage.setItem(PREFERENCE_KEY, JSON.stringify(preferences))
  } catch {
    /* ignore nonsecret preference failure */
  }
}
