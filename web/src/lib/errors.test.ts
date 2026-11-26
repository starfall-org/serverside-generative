import { describe, expect, it } from 'vitest'
import { AppError, errorFromStatus, isAbort, normalizeError } from './errors'

describe('errors', () => {
  it('maps HTTP statuses to safe kinds', () => {
    expect(errorFromStatus(401).kind).toBe('unauthorized')
    expect(errorFromStatus(403).kind).toBe('forbidden')
    expect(errorFromStatus(404).kind).toBe('not_found')
    expect(errorFromStatus(409, 'revision_conflict').kind).toBe('conflict')
    expect(errorFromStatus(413).kind).toBe('invalid')
    expect(errorFromStatus(422).kind).toBe('invalid')
    expect(errorFromStatus(429).kind).toBe('rate_limited')
    expect(errorFromStatus(500).kind).toBe('server')
    expect(errorFromStatus(501).kind).toBe('unsupported')
  })

  it('keeps codes and retryability', () => {
    const error = errorFromStatus(429, 'resource_exhausted', 'Busy')
    expect(error).toMatchObject({
      kind: 'rate_limited',
      status: 429,
      code: 'resource_exhausted',
      retryable: true,
      message: 'Busy',
    })
    expect(errorFromStatus(400, 'invalid_request').retryable).toBe(false)
  })

  it('normalizes unknown values without leaking stack details', () => {
    expect(normalizeError(new TypeError('fetch failed'))).toMatchObject({ kind: 'network' })
    expect(normalizeError(new AppError('x', { kind: 'timeout' })).kind).toBe('timeout')
    expect(normalizeError('plain')).toMatchObject({ kind: 'unknown', message: 'plain' })
    expect(isAbort(new DOMException('stop', 'AbortError'))).toBe(true)
    expect(isAbort(new AppError('stop', { kind: 'aborted' }))).toBe(true)
  })
})
