import * as React from 'react'
import type {
  ChatMessage,
  ChatSummary,
  Conversation,
  EphemeralProvider,
  ModelSelection,
  ProviderCatalog,
  RuntimeConfig,
  SessionEvent,
  SessionView,
  ToolActivity,
} from '../../api/types'
import { parseConversation } from '../../api/contracts'
import { startEventStream, type EventStreamHandle } from '../../api/events'
import { AppError, isAbort, normalizeError } from '../../lib/errors'
import { utf8Bytes, decodeBase64Chunks, base64ByteLength } from '../../lib/bytes'
import { uid } from '../../lib/id'
import { downloadFile, safeFilename } from '../../lib/format'
import { useConnection } from '../connection/ConnectionProvider'
import { usePreferences } from '../../app/preferences'
import { eventOwnedByRun, reconcileSnapshot } from './sessionState'

export type RunPhase =
  'submitting' | 'streaming' | 'cancelling' | 'completed' | 'failed' | 'interrupted' | 'unknown'

export interface RunState {
  requestId: string
  phase: RunPhase
  streamText: string
  reasoning: string
  tools: ToolActivity[]
  startedAt: number
  error?: AppError
}

export interface ChatState {
  conversation: Conversation | null
  revision: number | null
  eventSeq: number
  lastSeq: number
  loading: boolean
  error: AppError | null
  run: RunState | null
  draft: string
  stale: boolean
  instructions: string
}

export interface ConflictState {
  chatId: string
  message: string
}

interface ChatContextValue {
  chats: ChatSummary[]
  listLoading: boolean
  listError: AppError | null
  nextCursor: string
  activeId: string
  active: ChatState
  catalog: ProviderCatalog | null
  catalogLoading: boolean
  catalogError: AppError | null
  selection: ModelSelection
  hasEphemeralKey: boolean
  conflict: ConflictState | null
  newChat: () => Promise<void>
  openChat: (id: string) => Promise<void>
  setActiveChat: (id: string) => void
  refreshChats: (reset?: boolean) => Promise<void>
  loadMoreChats: () => Promise<void>
  send: (content: string) => Promise<void>
  cancel: () => Promise<void>
  setDraft: (text: string) => void
  setInstructions: (text: string) => void
  renameChat: (id: string, title: string) => Promise<void>
  deleteChat: (id: string) => Promise<void>
  exportChat: (id: string, format: 'markdown' | 'json') => Promise<void>
  importChat: (file: File) => Promise<void>
  reloadChat: (id: string) => Promise<void>
  resolveConflict: () => Promise<void>
  dismissConflict: () => void
  setSelection: (selection: ModelSelection, apiKey?: string) => void
  refreshCatalog: () => Promise<void>
  getChat: (id: string) => ChatState
  maxRunBytes: number
  maxMessageBytes: number
  conversationMaxBytes: number
}

const ChatContext = React.createContext<ChatContextValue | null>(null)

const emptyChat = (): ChatState => ({
  conversation: null,
  revision: null,
  eventSeq: 0,
  lastSeq: 0,
  loading: false,
  error: null,
  run: null,
  draft: '',
  stale: false,
  instructions: '',
})

const MAX_IMPORT_BYTES = 8 << 20
// Idle subscriptions are cheap but not free: bound them so browsing many
// conversations cannot exhaust the daemon's per-owner subscription budget.
const MAX_IDLE_STREAMS = 8
const ACTIVE_RUN_PHASES: RunPhase[] = ['submitting', 'streaming', 'cancelling']

interface PendingComposerState {
  draft: string
  instructions: string
  selection: ModelSelection | null
  apiKey: string
}

interface CancelTimer {
  timer: number
  requestId: string
}

function isActiveRun(run: RunState | null | undefined): boolean {
  return Boolean(run && ACTIVE_RUN_PHASES.includes(run.phase))
}

function eventBelongsToRun(event: SessionEvent, run: RunState | null | undefined): boolean {
  return eventOwnedByRun(event.request_id, run?.requestId)
}

function parseRuntimeNumbers(selection: ModelSelection) {
  const modelConfig: Record<string, number> = {}
  const config: Record<string, number> = {}
  if (selection.temperature.trim()) {
    const value = Number(selection.temperature)
    if (Number.isFinite(value)) modelConfig.temperature = value
  }
  if (selection.topP.trim()) {
    const value = Number(selection.topP)
    if (Number.isFinite(value)) modelConfig.top_p = value
  }
  if (selection.topK.trim()) {
    const value = Number(selection.topK)
    if (Number.isFinite(value)) modelConfig.top_k = Math.round(value)
  }
  if (selection.maxTokens.trim()) {
    const value = Number(selection.maxTokens)
    if (Number.isFinite(value)) config.maxTokens = Math.round(value)
  }
  return {
    modelConfig: Object.keys(modelConfig).length ? modelConfig : undefined,
    config: Object.keys(config).length ? config : undefined,
  }
}

function buildRuntime(selection: ModelSelection, apiKey: string, instructions: string): RuntimeConfig {
  const instructionMap = instructions.trim() ? { 'client-instructions': instructions } : undefined
  if (selection.source === 'stored') {
    if (!selection.providerId || !selection.modelId) {
      throw new AppError('Choose a provider and model', { kind: 'invalid', code: 'missing_runtime_config' })
    }
    return {
      source: 'stored',
      provider_id: selection.providerId,
      model_id: selection.modelId,
      ...(selection.keyId ? { key_id: selection.keyId } : {}),
      ...(instructionMap ? { instructions: instructionMap } : {}),
    }
  }
  if (!selection.modelId) {
    throw new AppError('Choose a model', { kind: 'invalid', code: 'missing_runtime_config' })
  }
  const { modelConfig, config } = parseRuntimeNumbers(selection)
  const provider: EphemeralProvider = {
    type: selection.providerType,
    modelId: selection.modelId,
    ...(selection.baseUrl.trim() ? { baseUrl: selection.baseUrl.trim() } : {}),
    auth: apiKey ? { method: 'bearer_token' as const, value: apiKey } : { method: 'none' as const },
    ...(modelConfig ? { modelConfig } : {}),
    ...(config ? { config } : {}),
  }
  const runtime: RuntimeConfig = { source: 'mobile_ephemeral', provider }
  if (instructionMap) runtime.instructions = instructionMap
  return runtime
}

function summaryFromConversation(conversation: Conversation): ChatSummary {
  return {
    id: conversation.id,
    title: conversation.title,
    created_at: conversation.created_at,
    updated_at: conversation.updated_at,
    revision: 0,
    has_messages: conversation.messages.length > 0,
    has_summary: Boolean(conversation.summary),
  }
}

function markdownFor(conversation: Conversation): string {
  const lines: string[] = [`# ${conversation.title || 'Conversation'}`, '']
  for (const message of conversation.messages) {
    const version = message.versions[message.active_version_index] ?? message.versions[0]
    if (!version) continue
    const label = message.role === 'user' ? 'You' : message.role === 'system' ? 'System' : 'Assistant'
    lines.push(`## ${label}`, '', version.content || '', '')
    if (version.reasoning_content) {
      lines.push('<details><summary>Reasoning</summary>', '', version.reasoning_content, '', '</details>', '')
    }
    for (const tool of version.toolActivity ?? []) {
      lines.push(`> Tool: ${tool.name} (${tool.status || 'unknown'})`, '')
      if (tool.arguments) lines.push('```json', tool.arguments, '```', '')
      if (tool.response) lines.push('```', tool.response, '```', '')
    }
  }
  return lines.join('\n')
}

function sanitizeForExport(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(sanitizeForExport)
  if (value && typeof value === 'object') {
    const out: Record<string, unknown> = {}
    for (const [key, child] of Object.entries(value as Record<string, unknown>)) {
      if (/^(api_?key|authorization|secret|token|password|credential|runtime)$/i.test(key)) continue
      out[key] = sanitizeForExport(child)
    }
    return out
  }
  return value
}

function normalizeImportedConversation(raw: unknown): Conversation {
  const candidate =
    raw && typeof raw === 'object' && 'conversation' in raw
      ? (raw as { conversation: unknown }).conversation
      : raw
  const parsed = parseConversation(candidate)
  if (!parsed) throw new AppError('Invalid conversation', { kind: 'invalid' })
  const now = Date.now()
  const messages: ChatMessage[] = parsed.messages.map((message) => ({
    ...message,
    id: message.id || uid(),
    versions: message.versions.map((version) => ({ ...version, content: version.content ?? '' })),
    active_version_index: message.active_version_index,
  }))
  const ids = new Set(messages.map((message) => message.id))
  const summary = parsed.summary && ids.has(parsed.summary.through_message_id) ? parsed.summary : undefined
  return {
    ...parsed,
    id: uid(),
    title: (parsed.title || 'Imported conversation').slice(0, 200),
    created_at: now,
    updated_at: now,
    messages,
    ...(summary ? { summary } : { summary: null }),
  }
}

export function ChatProvider({ children }: { children: React.ReactNode }) {
  const { client, setTransport, capabilities, phase } = useConnection()
  const { preferences, setDefaultModel } = usePreferences()

  const chatsRef = React.useRef<Record<string, ChatState>>({})
  const [, forceRender] = React.useReducer((value: number) => value + 1, 0)
  const [chatList, setChatList] = React.useState<ChatSummary[]>([])
  const [listLoading, setListLoading] = React.useState(false)
  const [listError, setListError] = React.useState<AppError | null>(null)
  const [nextCursor, setNextCursor] = React.useState('')
  const [activeId, setActiveIdState] = React.useState('')
  const activeIdRef = React.useRef('')
  const [catalog, setCatalog] = React.useState<ProviderCatalog | null>(null)
  const [catalogLoading, setCatalogLoading] = React.useState(true)
  const [catalogError, setCatalogError] = React.useState<AppError | null>(null)
  const [conflict, setConflict] = React.useState<ConflictState | null>(null)

  const selectionsRef = React.useRef<Record<string, ModelSelection>>({})
  const ephemeralKeysRef = React.useRef<Record<string, string>>({})
  const streamsRef = React.useRef<Map<string, { handle: EventStreamHandle; controller: AbortController }>>(
    new Map(),
  )
  const opensRef = React.useRef<Map<string, AbortController>>(new Map())
  const cancelTimersRef = React.useRef<Map<string, CancelTimer>>(new Map())
  const pendingRef = React.useRef<PendingComposerState>({
    draft: '',
    instructions: '',
    selection: null,
    apiKey: '',
  })
  const listCursorRef = React.useRef('')
  const mountedRef = React.useRef(true)

  const maxRunBytes = React.useMemo(
    () => capabilities?.chat_features?.negotiation?.generation?.max_run_content_bytes ?? 64 << 10,
    [capabilities],
  )
  const maxMessageBytes = React.useMemo(
    () => capabilities?.chat_features?.negotiation?.generation?.max_message_bytes ?? 512 << 10,
    [capabilities],
  )
  const conversationMaxBytes = React.useMemo(
    () => capabilities?.chat_features?.conversation_max_bytes ?? 3 << 20,
    [capabilities],
  )

  const getChat = React.useCallback((id: string): ChatState => {
    return chatsRef.current[id] ?? emptyChat()
  }, [])

  const updateChat = React.useCallback((id: string, updater: (chat: ChatState) => ChatState) => {
    if (!mountedRef.current) return
    const current = chatsRef.current[id] ?? emptyChat()
    chatsRef.current[id] = updater(current)
    forceRender()
  }, [])

  const setActiveChat = React.useCallback((id: string) => {
    activeIdRef.current = id
    setActiveIdState(id)
  }, [])

  const defaultSelection = React.useCallback((): ModelSelection => {
    const preferred = preferences.model
    if (preferred?.source === 'stored') {
      return {
        source: preferred.source,
        providerId: preferred.providerId,
        modelId: preferred.modelId,
        keyId: preferred.keyId || '',
        providerType: 'openai',
        baseUrl: '',
        maxTokens: '',
        temperature: '',
        topP: '',
        topK: '',
      }
    }
    const firstProvider = catalog?.providers.find((provider) => provider.models.length > 0)
    const firstModel = firstProvider?.models.find((model) => model.text_generation)
    return {
      source: 'stored',
      providerId: firstProvider?.id || '',
      modelId: firstModel?.id || '',
      keyId: firstProvider?.keys[0]?.id || '',
      providerType: 'openai',
      baseUrl: '',
      maxTokens: '',
      temperature: '',
      topP: '',
      topK: '',
    }
  }, [catalog, preferences.model])

  const selection = activeId
    ? (selectionsRef.current[activeId] ?? defaultSelection())
    : (pendingRef.current.selection ?? defaultSelection())

  const setSelection = React.useCallback(
    (next: ModelSelection, apiKey?: string) => {
      const id = activeIdRef.current
      if (!id) {
        // The composer is usable before the first chat exists; carry the
        // selection into the conversation that the first send creates.
        pendingRef.current.selection = next
        if (apiKey !== undefined) pendingRef.current.apiKey = apiKey
      } else {
        selectionsRef.current[id] = next
        if (apiKey !== undefined) ephemeralKeysRef.current[id] = apiKey
      }
      if (next.source === 'stored') {
        setDefaultModel({
          source: next.source,
          providerId: next.providerId,
          modelId: next.modelId,
          ...(next.keyId ? { keyId: next.keyId } : {}),
        })
      }
      forceRender()
    },
    [setDefaultModel],
  )

  const clearCancelTimer = React.useCallback((id: string) => {
    const entry = cancelTimersRef.current.get(id)
    if (!entry) return
    window.clearTimeout(entry.timer)
    cancelTimersRef.current.delete(id)
  }, [])

  const refreshCatalog = React.useCallback(async () => {
    setCatalogLoading(true)
    setCatalogError(null)
    try {
      const result = await client.providerCatalog()
      if (!mountedRef.current) return
      setCatalog(result)
    } catch (raw) {
      if (!mountedRef.current) return
      setCatalogError(normalizeError(raw))
    } finally {
      if (mountedRef.current) setCatalogLoading(false)
    }
  }, [client])

  const refreshChats = React.useCallback(
    async (reset = false) => {
      if (reset) {
        setListLoading(true)
        setListError(null)
      }
      try {
        const cursor = reset ? '' : listCursorRef.current
        let page
        try {
          page = await client.listChats(cursor)
        } catch (raw) {
          // Snapshot cursors expire after five minutes or a daemon restart.
          // Restart the traversal from the first page instead of failing forever.
          const error = normalizeError(raw)
          const expired = cursor && (error.code === 'invalid_request' || error.code === 'not_found')
          if (!expired) throw error
          listCursorRef.current = ''
          setNextCursor('')
          reset = true
          page = await client.listChats('')
        }
        if (!mountedRef.current) return
        const items = page.items.filter((item) => !item.deleted_at)
        setChatList((current) => {
          if (reset) return items
          const seen = new Set(current.map((item) => item.id))
          return [...current, ...items.filter((item) => !seen.has(item.id))]
        })
        listCursorRef.current = page.next_cursor
        setNextCursor(page.next_cursor)
      } catch (raw) {
        if (!mountedRef.current) return
        setListError(normalizeError(raw))
      } finally {
        if (mountedRef.current) setListLoading(false)
      }
    },
    [client],
  )

  const loadMoreChats = React.useCallback(async () => {
    if (!listCursorRef.current || listLoading) return
    await refreshChats(false)
  }, [refreshChats, listLoading])

  React.useEffect(() => {
    mountedRef.current = true
    void refreshChats(true)
    void refreshCatalog()
    const streams = streamsRef.current
    const opens = opensRef.current
    const cancelTimers = cancelTimersRef.current
    return () => {
      mountedRef.current = false
      ephemeralKeysRef.current = {}
      selectionsRef.current = {}
      pendingRef.current = { draft: '', instructions: '', selection: null, apiKey: '' }
      chatsRef.current = {}
      for (const stream of streams.values()) stream.handle.close()
      streams.clear()
      for (const controller of opens.values()) controller.abort()
      opens.clear()
      for (const entry of cancelTimers.values()) window.clearTimeout(entry.timer)
      cancelTimers.clear()
    }
  }, [refreshChats, refreshCatalog])

  const pruneIdleStreams = React.useCallback((activeChatId: string) => {
    if (streamsRef.current.size <= MAX_IDLE_STREAMS) return
    for (const [id, stream] of streamsRef.current) {
      if (streamsRef.current.size <= MAX_IDLE_STREAMS) break
      if (id === activeChatId) continue
      if (isActiveRun(chatsRef.current[id]?.run)) continue
      stream.handle.close()
      streamsRef.current.delete(id)
    }
  }, [])

  const resyncRef = React.useRef<(chatId: string) => void>(() => {})

  const ensureStream = React.useCallback(
    (chatId: string, after: number) => {
      const existing = streamsRef.current.get(chatId)
      if (existing && !existing.controller.signal.aborted) {
        return
      }
      existing?.handle.close()
      const controller = new AbortController()
      const handle = startEventStream({
        client,
        sessionId: chatId,
        after,
        signal: controller.signal,
        onTransport: (transport) => {
          if (mountedRef.current) setTransport(transport)
        },
        onEvent: (event) => handleEvent(chatId, event),
        onGap: () => {
          void recover(chatId)
        },
        onReconnect: () => {
          // A reconnect may cross a daemon restart, where event_seq resets.
          // Re-open the session and subscribe again from the fresh cursor.
          if (mountedRef.current) resyncRef.current(chatId)
        },
        onError: (error, willRetry) => {
          if (!mountedRef.current) return
          if (!willRetry && error.kind === 'not_found') {
            void recover(chatId)
          } else if (!willRetry) {
            streamsRef.current.delete(chatId)
            updateChat(chatId, (chat) => ({
              ...chat,
              error,
              run: chat.run
                ? { ...chat.run, phase: chat.run.phase === 'streaming' ? 'unknown' : chat.run.phase }
                : null,
            }))
          }
        },
      })
      streamsRef.current.set(chatId, { handle, controller })
      pruneIdleStreams(chatId)
    },
    // handleEvent/recover are stable callbacks declared below via refs.
    [client, pruneIdleStreams, setTransport, updateChat], // eslint-disable-line react-hooks/exhaustive-deps
  )

  const closeStream = React.useCallback((chatId: string) => {
    const stream = streamsRef.current.get(chatId)
    if (stream) {
      stream.handle.close()
      streamsRef.current.delete(chatId)
    }
  }, [])

  const acceptSessionView = React.useCallback(
    (chatId: string, view: SessionView): ChatState => {
      updateChat(chatId, (chat) => {
        const next = reconcileSnapshot(chat, view)
        if (!next) return chat
        return {
          ...chat,
          ...next,
          loading: false,
          error: null,
          stale: false,
        }
      })
      return getChat(chatId)
    },
    [getChat, updateChat],
  )

  const refreshSnapshot = React.useCallback(
    async (chatId: string, ifRevision?: number): Promise<ChatState | null> => {
      try {
        const chat = getChat(chatId)
        const revision = ifRevision ?? chat.revision ?? undefined
        let view: SessionView
        try {
          view = await client.getSession(chatId, revision)
        } catch (raw) {
          const error = normalizeError(raw)
          if (error.code !== 'resource_exhausted' && error.code !== 'payload_too_large') throw error
          view = await readChunked(chatId)
        }
        if (!mountedRef.current) return null
        return acceptSessionView(chatId, view)
      } catch (raw) {
        if (!mountedRef.current) return null
        const error = normalizeError(raw)
        updateChat(chatId, (chat) => ({ ...chat, error, loading: false }))
        return null
      }
    },
    // readChunked is declared below but only invoked after render.
    [acceptSessionView, client, getChat, updateChat], // eslint-disable-line react-hooks/exhaustive-deps
  )

  const readChunked = React.useCallback(
    async (chatId: string, signal?: AbortSignal): Promise<SessionView> => {
      const info = await client.rpc<{
        session_id: string
        revision: number
        size: number
        event_seq: number
        version: number
      }>('session.open_chunked', {}, chatId, { signal })
      const chunks: string[] = []
      let offset = 0
      for (;;) {
        const chunk = await client.rpc<{ revision: number; offset: number; data: string; done: boolean }>(
          'session.read_chunk',
          { revision: info.revision, offset, limit: 262144 },
          chatId,
          { signal },
        )
        chunks.push(chunk.data)
        offset = chunk.offset + base64ByteLength(chunk.data)
        if (chunk.done) break
      }
      const conversation = parseConversation(JSON.parse(decodeBase64Chunks(chunks)))
      if (!conversation) throw new AppError('Invalid chunked conversation', { kind: 'invalid' })
      return {
        session_id: chatId,
        revision: info.revision,
        event_seq: info.event_seq,
        conversation,
      }
    },
    [client],
  )

  const openView = React.useCallback(
    async (id: string, data: Record<string, unknown>, signal: AbortSignal) => {
      try {
        return await client.openSession(id, data, signal)
      } catch (raw) {
        const error = normalizeError(raw)
        if (error.code !== 'resource_exhausted' && error.code !== 'payload_too_large') throw error
        return readChunked(id, signal)
      }
    },
    [client, readChunked],
  )

  const syncSessionCursor = React.useCallback(
    async (chatId: string, signal?: AbortSignal): Promise<ChatState | null> => {
      const chat = getChat(chatId)
      const data: Record<string, unknown> = {}
      if (chat.revision !== null) data.if_revision = chat.revision
      const view = await openView(chatId, data, signal ?? new AbortController().signal)
      if (!mountedRef.current || signal?.aborted) return null
      return acceptSessionView(chatId, view)
    },
    [acceptSessionView, getChat, openView],
  )

  const recover = React.useCallback(
    async (chatId: string) => {
      if (opensRef.current.has(chatId)) return
      const controller = new AbortController()
      opensRef.current.set(chatId, controller)
      const state = getChat(chatId)
      const runWasActive = isActiveRun(state.run)
      if (runWasActive) {
        updateChat(chatId, (chat) => ({
          ...chat,
          run: chat.run ? { ...chat.run, phase: 'unknown' } : null,
          error: new AppError('Live events were missed', { kind: 'conflict', code: 'event_gap' }),
        }))
      }
      closeStream(chatId)
      try {
        const synced = await syncSessionCursor(chatId, controller.signal)
        if (!mountedRef.current || controller.signal.aborted || !synced) return
        // Subscribe only after the fresh cursor is in place, otherwise a
        // daemon restart would replay or skip events against a stale seq.
        ensureStream(chatId, synced.eventSeq)
        if (runWasActive) {
          const lastVersion = synced.conversation?.messages.at(-1)?.versions?.[0]
          const interrupted = lastVersion?.generation_interrupted === true
          updateChat(chatId, (chat) => ({
            ...chat,
            run: chat.run
              ? {
                  ...chat.run,
                  phase: interrupted ? 'interrupted' : 'unknown',
                  streamText: '',
                  reasoning: '',
                  tools: [],
                }
              : null,
          }))
        }
      } catch (raw) {
        if (!mountedRef.current || controller.signal.aborted) return
        const error = normalizeError(raw)
        updateChat(chatId, (chat) => ({
          ...chat,
          error,
          run: chat.run ? { ...chat.run, phase: 'unknown' } : null,
        }))
      } finally {
        if (opensRef.current.get(chatId) === controller) opensRef.current.delete(chatId)
      }
    },
    [closeStream, ensureStream, getChat, syncSessionCursor, updateChat],
  )

  React.useEffect(() => {
    resyncRef.current = (chatId: string) => {
      void recover(chatId)
    }
  }, [recover])

  const previousPhaseRef = React.useRef(phase)
  React.useEffect(() => {
    const previous = previousPhaseRef.current
    previousPhaseRef.current = phase
    if (phase !== 'connected' || (previous !== 'offline' && previous !== 'reconnecting')) return
    void refreshChats(true)
    void refreshCatalog()
    const ids = new Set(streamsRef.current.keys())
    if (activeIdRef.current) ids.add(activeIdRef.current)
    for (const id of ids) void recover(id)
  }, [phase, recover, refreshChats, refreshCatalog])

  const finalizeRun = React.useCallback(
    async (chatId: string, phase: 'completed' | 'failed', code = '', requestId?: string) => {
      const current = getChat(chatId)
      const runId = requestId || current.run?.requestId
      if (!runId || current.run?.requestId !== runId) return
      clearCancelTimer(chatId)
      await refreshSnapshot(chatId)
      if (!mountedRef.current) return
      const state = getChat(chatId)
      if (state.run?.requestId !== runId) return
      const lastVersion = state.conversation?.messages.at(-1)?.versions?.[0]
      const interrupted = lastVersion?.generation_interrupted === true
      updateChat(chatId, (chat) => ({
        ...chat,
        run: chat.run
          ? {
              ...chat.run,
              phase: interrupted ? 'interrupted' : phase,
              streamText: '',
              reasoning: '',
              tools: [],
              ...(code ? { error: new AppError(code, { kind: 'server', code }) } : {}),
            }
          : null,
        error:
          phase === 'failed'
            ? new AppError(code || 'Generation failed', { kind: 'server', code })
            : chat.error,
      }))
      window.setTimeout(() => {
        if (!mountedRef.current) return
        updateChat(chatId, (chat) => ({
          ...chat,
          run: chat.run?.requestId === runId && !isActiveRun(chat.run) ? null : chat.run,
        }))
      }, 1200)
      void refreshChats(true)
    },
    [clearCancelTimer, getChat, refreshChats, refreshSnapshot, updateChat],
  )

  const handleEvent = React.useCallback(
    (chatId: string, event: SessionEvent) => {
      if (!mountedRef.current) return
      const before = getChat(chatId)
      if (event.seq <= before.lastSeq) return
      const runBefore = before.run
      const belongs = eventBelongsToRun(event, runBefore)

      updateChat(chatId, (chat) => {
        if (event.seq <= chat.lastSeq) return chat
        const run = chat.run
        const belongsNow = eventBelongsToRun(event, run)
        switch (event.kind) {
          case 'delta': {
            if (!run || !belongsNow) return { ...chat, lastSeq: event.seq }
            return {
              ...chat,
              lastSeq: event.seq,
              run: {
                ...run,
                phase: run.phase === 'submitting' ? 'streaming' : run.phase,
                streamText: run.streamText + String(event.data?.text ?? ''),
              },
            }
          }
          case 'reasoning_delta': {
            if (!run || !belongsNow) return { ...chat, lastSeq: event.seq }
            return {
              ...chat,
              lastSeq: event.seq,
              run: { ...run, reasoning: run.reasoning + String(event.data?.text ?? '') },
            }
          }
          case 'tool_activity': {
            if (!run || !belongsNow) return { ...chat, lastSeq: event.seq }
            const activity = event.data as unknown as ToolActivity
            if (!activity?.id) return { ...chat, lastSeq: event.seq }
            const tools = run.tools.filter((item) => item.id !== activity.id)
            return { ...chat, lastSeq: event.seq, run: { ...run, tools: [...tools, activity] } }
          }
          case 'conversation': {
            const conversation = event.data?.conversation ? parseConversation(event.data.conversation) : null
            const revision = typeof event.data?.revision === 'number' ? event.data.revision : chat.revision
            if (!conversation) return { ...chat, lastSeq: event.seq }
            // Never move the local snapshot backwards if events are replayed
            // or reordered across a reconnect.
            if (chat.revision !== null && revision !== null && revision < chat.revision) {
              return { ...chat, lastSeq: event.seq }
            }
            return {
              ...chat,
              lastSeq: event.seq,
              conversation,
              revision,
              run: run && belongsNow ? { ...run, streamText: '', reasoning: '', tools: [] } : run,
            }
          }
          case 'completed':
          case 'error':
          case 'event_gap':
          default:
            return { ...chat, lastSeq: event.seq }
        }
      })

      if (event.kind === 'completed' && belongs) {
        void finalizeRun(chatId, 'completed', '', event.request_id)
      } else if (event.kind === 'error' && belongs) {
        const code = typeof event.data?.code === 'string' ? event.data.code : 'unknown'
        void finalizeRun(chatId, 'failed', code, event.request_id)
      } else if (event.kind === 'event_gap') {
        void recover(chatId)
      }
    },
    [finalizeRun, getChat, recover, updateChat],
  )

  const openChat = React.useCallback(
    async (id: string, initialConversation?: Conversation) => {
      setActiveChat(id)
      closeStream(id)
      const previous = opensRef.current.get(id)
      previous?.abort()
      const controller = new AbortController()
      opensRef.current.set(id, controller)
      updateChat(id, (chat) => ({ ...chat, loading: true, error: null }))
      try {
        const cached = getChat(id)
        const data: Record<string, unknown> = {}
        if (initialConversation) data.conversation = initialConversation
        if (!initialConversation && cached.revision !== null) data.if_revision = cached.revision
        const view = await openView(id, data, controller.signal)
        if (!mountedRef.current || controller.signal.aborted) return
        let accepted = acceptSessionView(id, view)
        if (!accepted.conversation) {
          accepted = (await refreshSnapshot(id)) ?? accepted
          if (!mountedRef.current || controller.signal.aborted) return
        }
        ensureStream(id, accepted.eventSeq)
        if (!chatList.some((item) => item.id === id) && accepted.conversation) {
          setChatList((current) => [summaryFromConversation(accepted.conversation!), ...current])
        }
      } catch (raw) {
        if (controller.signal.aborted || isAbort(raw)) return
        const error = normalizeError(raw)
        updateChat(id, (chat) => ({ ...chat, loading: false, error }))
      } finally {
        if (opensRef.current.get(id) === controller) opensRef.current.delete(id)
      }
    },
    [
      acceptSessionView,
      chatList,
      closeStream,
      ensureStream,
      getChat,
      openView,
      refreshSnapshot,
      setActiveChat,
      updateChat,
    ],
  )

  const openDraftChat = React.useCallback(
    async (id: string) => {
      if (!activeIdRef.current) {
        const pending = pendingRef.current
        if (pending.selection) selectionsRef.current[id] = pending.selection
        if (pending.apiKey) ephemeralKeysRef.current[id] = pending.apiKey
        updateChat(id, (chat) => ({ ...chat, draft: pending.draft, instructions: pending.instructions }))
        pendingRef.current = { draft: '', instructions: '', selection: null, apiKey: '' }
      }
      await openChat(id)
    },
    [openChat, updateChat],
  )

  const newChat = React.useCallback(async () => {
    setConflict(null)
    await openDraftChat(uid())
  }, [openDraftChat])

  const send = React.useCallback(
    async (content: string) => {
      const trimmed = content.trim()
      if (!trimmed) return
      if (utf8Bytes(trimmed) > maxRunBytes) {
        throw new AppError(`Message exceeds ${maxRunBytes} bytes`, {
          kind: 'invalid',
          code: 'payload_too_large',
        })
      }
      let id = activeIdRef.current
      if (!id) {
        id = uid()
        await openDraftChat(id)
      }
      let chat = getChat(id)
      if (!chat.conversation) {
        await openChat(id)
        chat = getChat(id)
      }
      if (!chat.conversation) {
        throw chat.error ?? new AppError('Conversation unavailable', { kind: 'not_found' })
      }
      if (isActiveRun(chat.run)) return
      clearCancelTimer(id)
      const selectionForChat = selectionsRef.current[id] ?? defaultSelection()
      const apiKey = ephemeralKeysRef.current[id] ?? ''
      const instructionsId = 'client-instructions'
      const selected = new Set(chat.conversation.selected_prompt_ids ?? [])
      const hasInstructions = chat.instructions.trim().length > 0
      if (selected.has(instructionsId) !== hasInstructions) {
        if (hasInstructions) selected.add(instructionsId)
        else selected.delete(instructionsId)
        const nextConversation: Conversation = {
          ...chat.conversation,
          selected_prompt_ids: Array.from(selected),
        }
        try {
          const view = await client.rpc<SessionView>(
            'session.save',
            {
              conversation: nextConversation,
              expected_revision: chat.revision,
              idempotency_key: uid(),
            },
            id,
          )
          if (!mountedRef.current) return
          updateChat(id, (current) => {
            if (current.revision !== null && view.revision < current.revision) return current
            return {
              ...current,
              conversation: view.conversation ? parseConversation(view.conversation) : nextConversation,
              revision: view.revision,
            }
          })
        } catch (raw) {
          const error = normalizeError(raw)
          updateChat(id, (current) => ({
            ...current,
            error,
            stale: error.code === 'revision_conflict',
          }))
          if (error.code === 'revision_conflict') {
            setConflict({ chatId: id, message: error.message })
          }
          throw error
        }
        chat = getChat(id)
      }
      let runtime: RuntimeConfig
      try {
        runtime = buildRuntime(selectionForChat, apiKey, chat.instructions)
      } catch (raw) {
        const error = normalizeError(raw)
        updateChat(id, (current) => ({ ...current, error }))
        throw error
      }
      try {
        // generation must start from a cursor observed after any pending save,
        // otherwise events emitted between the cached open and session.run can
        // be silently skipped.
        const synced = await syncSessionCursor(id)
        if (!mountedRef.current) return
        if (!synced) throw new AppError('Conversation unavailable', { kind: 'not_found' })
        chat = getChat(id)
      } catch (raw) {
        const error = normalizeError(raw)
        updateChat(id, (current) => ({ ...current, error }))
        throw error
      }
      const requestId = uid()
      closeStream(id)
      updateChat(id, (current) => ({
        ...current,
        error: null,
        run: {
          requestId,
          phase: 'submitting',
          streamText: '',
          reasoning: '',
          tools: [],
          startedAt: Date.now(),
        },
      }))
      try {
        await client.rpc('session.configure', { runtime }, id)
        ensureStream(id, getChat(id).eventSeq)
        await client.rpc('session.run', { request_id: requestId, content: trimmed }, id)
        if (!mountedRef.current) return
        updateChat(id, (current) => ({
          ...current,
          draft: current.draft.trim() === trimmed ? '' : current.draft,
          run:
            current.run?.requestId === requestId && current.run.phase === 'submitting'
              ? { ...current.run, phase: 'streaming' }
              : current.run,
        }))
      } catch (raw) {
        const error = normalizeError(raw)
        if (!mountedRef.current) return
        const uncertain = error.kind === 'network' || error.kind === 'timeout' || error.kind === 'server'
        updateChat(id, (current) => ({
          ...current,
          error,
          run:
            current.run?.requestId === requestId
              ? {
                  ...current.run,
                  phase: uncertain ? 'unknown' : 'failed',
                  error,
                }
              : current.run,
        }))
        if (uncertain) {
          ensureStream(id, getChat(id).eventSeq)
          void refreshSnapshot(id)
        }
        throw error
      }
    },
    [
      clearCancelTimer,
      client,
      closeStream,
      defaultSelection,
      ensureStream,
      getChat,
      maxRunBytes,
      openChat,
      openDraftChat,
      refreshSnapshot,
      syncSessionCursor,
      updateChat,
    ],
  )

  const cancel = React.useCallback(async () => {
    const id = activeIdRef.current
    if (!id) return
    const chat = getChat(id)
    const requestId = chat.run?.requestId
    if (!requestId) return
    updateChat(id, (current) => ({
      ...current,
      run: current.run ? { ...current.run, phase: 'cancelling' } : null,
    }))
    try {
      await client.rpc('session.cancel', { request_id: requestId }, id)
    } catch (raw) {
      if (!mountedRef.current) return
      const error = normalizeError(raw)
      updateChat(id, (current) =>
        current.run?.requestId === requestId && current.run.phase === 'cancelling'
          ? { ...current, error, run: { ...current.run, phase: 'streaming' } }
          : current,
      )
      return
    }
    if (
      !mountedRef.current ||
      getChat(id).run?.requestId !== requestId ||
      getChat(id).run?.phase !== 'cancelling'
    )
      return
    clearCancelTimer(id)
    const timer = window.setTimeout(() => {
      const entry = cancelTimersRef.current.get(id)
      if (entry?.requestId !== requestId) return
      cancelTimersRef.current.delete(id)
      void refreshSnapshot(id).then(() => {
        if (!mountedRef.current) return
        updateChat(id, (current) =>
          current.run?.requestId === requestId && current.run.phase === 'cancelling'
            ? { ...current, run: { ...current.run, phase: 'interrupted' } }
            : current,
        )
      })
    }, 8000)
    cancelTimersRef.current.set(id, { timer, requestId })
  }, [clearCancelTimer, client, getChat, refreshSnapshot, updateChat])

  const setDraft = React.useCallback(
    (text: string) => {
      const id = activeIdRef.current
      if (!id) {
        pendingRef.current.draft = text
        forceRender()
        return
      }
      updateChat(id, (chat) => ({ ...chat, draft: text }))
    },
    [updateChat],
  )

  const setInstructions = React.useCallback(
    (text: string) => {
      const id = activeIdRef.current
      if (!id) {
        pendingRef.current.instructions = text
        forceRender()
        return
      }
      updateChat(id, (chat) => ({ ...chat, instructions: text }))
    },
    [updateChat],
  )

  const ensureConversation = React.useCallback(
    async (id: string): Promise<ChatState> => {
      let chat = getChat(id)
      if (!chat.conversation || chat.revision === null) {
        await openChat(id)
        chat = getChat(id)
      }
      if (!chat.conversation || chat.revision === null) {
        throw chat.error ?? new AppError('Conversation unavailable', { kind: 'not_found' })
      }
      return chat
    },
    [getChat, openChat],
  )

  const renameChat = React.useCallback(
    async (id: string, title: string) => {
      const clean = title.trim().slice(0, 200)
      if (!clean) return
      const chat = await ensureConversation(id)
      const conversation: Conversation = { ...chat.conversation!, title: clean, updated_at: Date.now() }
      try {
        const view = await client.rpc<SessionView>(
          'session.save',
          { conversation, expected_revision: chat.revision, idempotency_key: uid() },
          id,
        )
        if (!mountedRef.current) return
        updateChat(id, (current) => ({
          ...current,
          conversation: view.conversation ? parseConversation(view.conversation) : conversation,
          revision: view.revision,
          stale: false,
        }))
        setChatList((current) =>
          current.map((item) => (item.id === id ? { ...item, title: clean, updated_at: Date.now() } : item)),
        )
      } catch (raw) {
        const error = normalizeError(raw)
        if (error.code === 'revision_conflict') {
          setConflict({ chatId: id, message: error.message })
          updateChat(id, (current) => ({ ...current, stale: true }))
        }
        throw error
      }
    },
    [client, ensureConversation, updateChat],
  )

  const deleteChat = React.useCallback(
    async (id: string) => {
      const chat = await ensureConversation(id)
      await client.rpc('session.delete', { expected_revision: chat.revision, idempotency_key: uid() }, id)
      if (!mountedRef.current) return
      closeStream(id)
      delete chatsRef.current[id]
      delete selectionsRef.current[id]
      delete ephemeralKeysRef.current[id]
      setChatList((current) => current.filter((item) => item.id !== id))
      if (activeIdRef.current === id) {
        setActiveChat('')
        setActiveIdState('')
      }
      forceRender()
    },
    [client, closeStream, ensureConversation, setActiveChat],
  )

  const exportChat = React.useCallback(
    async (id: string, format: 'markdown' | 'json') => {
      const chat = await ensureConversation(id)
      const conversation = chat.conversation!
      if (format === 'markdown') {
        downloadFile(safeFilename(conversation.title, 'md'), markdownFor(conversation), 'text/markdown')
      } else {
        const payload = {
          version: 1,
          exported_at: new Date().toISOString(),
          conversation: sanitizeForExport(conversation),
        }
        downloadFile(
          safeFilename(conversation.title, 'json'),
          JSON.stringify(payload, null, 2),
          'application/json',
        )
      }
    },
    [ensureConversation],
  )

  const importChat = React.useCallback(
    async (file: File) => {
      if (file.size > MAX_IMPORT_BYTES) {
        throw new AppError('File too large', { kind: 'invalid', code: 'payload_too_large' })
      }
      const text = await file.text()
      let raw: unknown
      try {
        raw = JSON.parse(text)
      } catch {
        throw new AppError('Invalid JSON', { kind: 'invalid' })
      }
      const conversation = normalizeImportedConversation(raw)
      await openChat(conversation.id, conversation)
      await refreshChats(true)
    },
    [openChat, refreshChats],
  )

  const reloadChat = React.useCallback(
    async (id: string) => {
      closeStream(id)
      updateChat(id, (chat) => ({ ...chat, stale: false }))
      await openChat(id)
      setConflict(null)
    },
    [closeStream, openChat, updateChat],
  )

  const resolveConflict = React.useCallback(async () => {
    if (!conflict) return
    await reloadChat(conflict.chatId)
  }, [conflict, reloadChat])

  const dismissConflict = React.useCallback(() => setConflict(null), [])

  const activeChat = activeId ? chatsRef.current[activeId] : undefined
  const { draft: pendingDraft, instructions: pendingInstructions } = pendingRef.current
  const active = React.useMemo(
    () => activeChat ?? { ...emptyChat(), draft: pendingDraft, instructions: pendingInstructions },
    [activeChat, pendingDraft, pendingInstructions],
  )

  const value = React.useMemo<ChatContextValue>(
    () => ({
      chats: chatList,
      listLoading,
      listError,
      nextCursor,
      activeId,
      active,
      catalog,
      catalogLoading,
      catalogError,
      selection,
      hasEphemeralKey: Boolean(activeId ? ephemeralKeysRef.current[activeId] : pendingRef.current.apiKey),
      conflict,
      newChat,
      openChat,
      setActiveChat,
      refreshChats,
      loadMoreChats,
      send,
      cancel,
      setDraft,
      setInstructions,
      renameChat,
      deleteChat,
      exportChat,
      importChat,
      reloadChat,
      resolveConflict,
      dismissConflict,
      setSelection,
      refreshCatalog,
      getChat,
      maxRunBytes,
      maxMessageBytes,
      conversationMaxBytes,
    }),
    [
      chatList,
      listLoading,
      listError,
      nextCursor,
      activeId,
      active,
      catalog,
      catalogLoading,
      catalogError,
      selection,
      conflict,
      newChat,
      openChat,
      setActiveChat,
      refreshChats,
      loadMoreChats,
      send,
      cancel,
      setDraft,
      setInstructions,
      renameChat,
      deleteChat,
      exportChat,
      importChat,
      reloadChat,
      resolveConflict,
      dismissConflict,
      setSelection,
      refreshCatalog,
      getChat,
      maxRunBytes,
      maxMessageBytes,
      conversationMaxBytes,
    ],
  )

  return <ChatContext.Provider value={value}>{children}</ChatContext.Provider>
}

export function useChat(): ChatContextValue {
  const context = React.useContext(ChatContext)
  if (!context) throw new Error('useChat must be used inside ChatProvider')
  return context
}
