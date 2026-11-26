import { describe, expect, it } from 'vitest'
import { eventOwnedByRun, reconcileSnapshot } from './sessionState'

function conversation(id = 'c') {
  return {
    id,
    title: 'Chat',
    created_at: 1,
    updated_at: 2,
    messages: [],
  }
}

describe('reconcileSnapshot', () => {
  it('adopts a lower event_seq after a daemon restart instead of keeping the old cursor', () => {
    const next = reconcileSnapshot(
      { conversation: conversation(), revision: 5, eventSeq: 900, lastSeq: 900 },
      { session_id: 'c', revision: 5, event_seq: 0, unchanged: true },
    )
    expect(next).toMatchObject({ revision: 5, eventSeq: 0, lastSeq: 0 })
  })

  it('rejects a response from an older revision', () => {
    const next = reconcileSnapshot(
      { conversation: conversation(), revision: 7, eventSeq: 90, lastSeq: 90 },
      { session_id: 'c', revision: 6, event_seq: 120, conversation: conversation() },
    )
    expect(next).toBeNull()
  })

  it('accepts a full snapshot for a newer revision', () => {
    const next = reconcileSnapshot(
      { conversation: conversation(), revision: 3, eventSeq: 10, lastSeq: 10 },
      { session_id: 'c', revision: 4, event_seq: 12, conversation: conversation('d') },
    )
    expect(next?.revision).toBe(4)
    expect(next?.lastSeq).toBe(12)
    expect(next?.conversation?.id).toBe('d')
  })

  it('does not clear a cached conversation on an unchanged response', () => {
    const next = reconcileSnapshot(
      { conversation: conversation(), revision: 3, eventSeq: 10, lastSeq: 10 },
      { session_id: 'c', revision: 3, event_seq: 15, unchanged: true },
    )
    expect(next?.conversation?.id).toBe('c')
    expect(next?.lastSeq).toBe(15)
  })

  it('ignores a full response without a conversation', () => {
    const next = reconcileSnapshot(
      { conversation: conversation(), revision: 3, eventSeq: 10, lastSeq: 10 },
      { session_id: 'c', revision: 4, event_seq: 12 },
    )
    expect(next).toBeNull()
  })
})

describe('eventOwnedByRun', () => {
  it('rejects events without a request id and events from another request', () => {
    expect(eventOwnedByRun('new', 'new')).toBe(true)
    expect(eventOwnedByRun('old', 'new')).toBe(false)
    expect(eventOwnedByRun('', 'new')).toBe(false)
    expect(eventOwnedByRun('new', undefined)).toBe(false)
  })
})
