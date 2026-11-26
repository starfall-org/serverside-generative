// Runtime validation for network input. The server is the source of truth, but
// the client must never crash on malformed or additive payloads.

export function isRecord(value: unknown): value is Record<string, any> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

export function asString(value: unknown, fallback = ''): string {
  return typeof value === 'string' ? value : fallback
}

export function asNumber(value: unknown, fallback = 0): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : fallback
}

export function asBoolean(value: unknown, fallback = false): boolean {
  return typeof value === 'boolean' ? value : fallback
}

export function asArray<T>(value: unknown): T[] {
  return Array.isArray(value) ? (value as T[]) : []
}

export function parseCapabilities(value: unknown): import('../api/types').PublicCapabilities | null {
  if (!isRecord(value) || typeof value.protocol !== 'number') return null
  return {
    ...value,
    protocol: value.protocol,
    transports: asArray<string>(value.transports),
    operations: asArray<string>(value.operations),
  }
}

export function parseSessionView(value: unknown): import('../api/types').SessionView | null {
  if (!isRecord(value) || typeof value.session_id !== 'string') return null
  return {
    session_id: value.session_id,
    revision: asNumber(value.revision),
    event_seq: asNumber(value.event_seq),
    conversation: isRecord(value.conversation) ? (value.conversation as any) : undefined,
    unchanged: value.unchanged === true,
  }
}

export function parseConversation(value: unknown): import('../api/types').Conversation | null {
  if (!isRecord(value) || typeof value.id !== 'string' || typeof value.title !== 'string') return null
  if (typeof value.created_at !== 'number' || typeof value.updated_at !== 'number') return null
  if (!Array.isArray(value.messages)) return null
  const ids = new Set<string>()
  const messages: import('../api/types').ChatMessage[] = []
  for (const raw of value.messages) {
    if (!isRecord(raw) || typeof raw.id !== 'string' || !raw.id || ids.has(raw.id)) return null
    if (raw.role !== 'user' && raw.role !== 'model' && raw.role !== 'system') return null
    ids.add(raw.id)
    const versions = asArray<Record<string, any>>(raw.versions).map((version) => ({
      ...version,
      content: asString(version.content),
    }))
    let active = asNumber(raw.active_version_index, 0)
    if (!Number.isInteger(active) || active < 0 || (versions.length > 0 && active >= versions.length)) {
      active = 0
    }
    messages.push({ ...raw, id: raw.id, role: raw.role, versions, active_version_index: active })
  }
  return { ...value, messages } as import('../api/types').Conversation
}

export function parseEvent(value: unknown): import('../api/types').SessionEvent | null {
  if (!isRecord(value) || typeof value.session_id !== 'string' || typeof value.seq !== 'number') return null
  if (typeof value.kind !== 'string') return null
  return {
    session_id: value.session_id,
    seq: value.seq,
    request_id: asString(value.request_id),
    kind: value.kind,
    data: isRecord(value.data) ? value.data : {},
  }
}

export function parseListPage(value: unknown): import('../api/types').ListPage {
  if (!isRecord(value)) return { version: 1, items: [], next_cursor: '' }
  const items = asArray<Record<string, any>>(value.items)
    .filter((item) => isRecord(item) && typeof item.id === 'string')
    .map((item) => ({ ...item, id: item.id, revision: asNumber(item.revision) }))
  return {
    version: asNumber(value.version, 1),
    items: items as import('../api/types').ChatSummary[],
    next_cursor: asString(value.next_cursor),
  }
}

export function parseProviderCatalog(value: unknown): import('../api/types').ProviderCatalog {
  if (!isRecord(value)) return { version: 1, revision: 0, providers: [] }
  const providers = asArray<Record<string, any>>(value.providers)
    .filter((provider) => isRecord(provider) && typeof provider.id === 'string')
    .map((provider) => ({
      id: provider.id,
      name: asString(provider.name, provider.id),
      type: asString(provider.type),
      custom_model_id: asBoolean(provider.custom_model_id),
      key_selection_required: asBoolean(provider.key_selection_required),
      keys: asArray<Record<string, any>>(provider.keys).filter(
        (key) => isRecord(key) && typeof key.id === 'string',
      ) as import('../api/types').ProviderKey[],
      models: asArray<Record<string, any>>(provider.models)
        .filter((model) => isRecord(model) && typeof model.id === 'string')
        .map((model) => ({
          id: model.id,
          name: asString(model.name, model.id),
          availability: asString(model.availability, 'unavailable'),
          text_generation: model.text_generation !== false,
          context_limit: typeof model.context_limit === 'number' ? model.context_limit : null,
          output_limit: typeof model.output_limit === 'number' ? model.output_limit : null,
        })),
    }))
  return {
    version: asNumber(value.version, 1),
    revision: asNumber(value.revision),
    providers: providers as import('../api/types').CatalogProvider[],
  }
}
