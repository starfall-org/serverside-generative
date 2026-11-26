import { beforeEach, describe, expect, it } from 'vitest'
import {
  clearIdentity,
  defaultPreferences,
  readIdentity,
  readPreferences,
  writeIdentity,
  writePreferences,
} from './storage'
import type { StoredIdentity } from '../api/types'

const origin = 'https://studio.example'
const base: StoredIdentity = {
  version: 1,
  origin,
  clientId: 'c',
  secret: 's',
  serverId: 'server',
  remember: true,
  pairedAt: 1,
}

describe('identity storage', () => {
  beforeEach(() => {
    window.localStorage.clear()
    window.sessionStorage.clear()
  })

  it('uses localStorage only when the device is remembered', () => {
    expect(writeIdentity(base)).toBe(true)
    expect(window.localStorage.getItem(`generative.identity.v1.${origin}`)).toBeTruthy()
    expect(window.sessionStorage.getItem(`generative.identity.v1.${origin}`)).toBeNull()

    const tabOnly = { ...base, remember: false }
    expect(writeIdentity(tabOnly)).toBe(true)
    expect(window.localStorage.getItem(`generative.identity.v1.${origin}`)).toBeNull()
    expect(window.sessionStorage.getItem(`generative.identity.v1.${origin}`)).toBeTruthy()
    expect(readIdentity(origin)?.remember).toBe(false)
  })

  it('ignores malformed or foreign identities', () => {
    window.localStorage.setItem(`generative.identity.v1.${origin}`, '{"version":2}')
    expect(readIdentity(origin)).toBeNull()
    window.localStorage.setItem(`generative.identity.v1.${origin}`, 'not json')
    expect(readIdentity(origin)).toBeNull()
    expect(readIdentity('https://other.example')).toBeNull()
  })

  it('clears both stores on disconnect', () => {
    writeIdentity(base)
    writeIdentity({ ...base, remember: false })
    clearIdentity(origin)
    expect(window.localStorage.getItem(`generative.identity.v1.${origin}`)).toBeNull()
    expect(window.sessionStorage.getItem(`generative.identity.v1.${origin}`)).toBeNull()
  })
})

describe('preferences', () => {
  beforeEach(() => {
    window.localStorage.clear()
  })

  it('defaults to system theme and automatic language', () => {
    expect(readPreferences()).toEqual({ ...defaultPreferences, language: 'auto' })
  })

  it('round-trips nonsecret preferences and ignores invalid values', () => {
    writePreferences({
      theme: 'dark',
      language: 'en',
      model: { source: 'stored', providerId: 'p', modelId: 'm' },
    })
    expect(readPreferences()).toMatchObject({ theme: 'dark', language: 'en' })
    window.localStorage.setItem('generative.preferences.v1', '{"theme":"neon","language":"fr"}')
    expect(readPreferences()).toEqual({ theme: 'system', language: 'auto', model: undefined })
  })
})
