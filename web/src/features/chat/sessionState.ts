import { parseConversation } from '../../api/contracts'
import type { Conversation, SessionView } from '../../api/types'

export interface SnapshotCursor {
  conversation: Conversation | null
  revision: number | null
  eventSeq: number
  lastSeq: number
}

/**
 * Decide whether a session.open/session.get response may replace local state.
 *
 * Two invariants matter here:
 * - a response from an older revision must never roll the snapshot back;
 * - event_seq is not durable across a daemon restart, so an accepted response
 *   always replaces the cursor instead of taking the maximum.
 */
export function reconcileSnapshot(state: SnapshotCursor, view: SessionView): SnapshotCursor | null {
  if (state.revision !== null && view.revision < state.revision) return null
  const conversation = view.conversation ? parseConversation(view.conversation) : null
  if (!view.unchanged && !conversation) return null
  return {
    conversation: view.unchanged ? state.conversation : conversation,
    revision: view.revision,
    eventSeq: view.event_seq,
    lastSeq: view.event_seq,
  }
}

/** Events without a request id cannot be attributed to the current run. */
export function eventOwnedByRun(
  eventRequestId: string | undefined,
  runRequestId: string | undefined,
): boolean {
  return Boolean(eventRequestId && runRequestId && eventRequestId === runRequestId)
}
