// Typed v5 protocol contracts. Unknown fields are retained where the server
// treats them as additive; nothing here is a substitute for runtime validation
// at the network boundary (see contracts.ts).

export type ConnectionPhase =
  | 'restoring'
  | 'login'
  | 'authenticating'
  | 'connected'
  | 'reconnecting'
  | 'offline'
  | 'expired'
  | 'incompatible'

export type TransportKind = 'sse' | 'poll' | 'unknown'

export interface PublicCapabilities {
  protocol: number
  transports: string[]
  operations: string[]
  native_terminal?: boolean
  native_agent?: boolean
  chat_features?: ChatFeatures
  [key: string]: unknown
}

export interface ChatFeatures {
  version: number
  provider_catalog?: string
  list_snapshot_pagination?: boolean
  chunked_transfer?: {
    version: number
    max_conversation_bytes: number
    max_chunk_bytes: number
    upload_expiry_seconds: number
    restart_resumable: boolean
  }
  model_limits?: string
  runtime_sources?: string[]
  provider_adapters?: string[]
  text_generation?: boolean
  streaming?: boolean
  attachments?: boolean
  multimodal_input?: boolean
  reasoning_history?: boolean
  selected_instructions?: boolean
  runtime_instruction_handoff?: boolean
  conversation_max_bytes?: number
  workspace_scope?: string
  negotiation?: Negotiation
  [key: string]: unknown
}

export interface Negotiation {
  version: number
  catalog_operation?: string
  modalities?: { input: string[]; output: string[]; attachment_upload: boolean }
  history?: {
    summary: boolean
    reasoning: boolean
    tool_activity: boolean
    selected_instructions: string
  }
  generation?: {
    streaming: boolean
    cancellation: boolean
    tool_loop: boolean
    max_history_messages: number
    max_message_bytes: number
    max_run_content_bytes: number
  }
  parameters?: Record<string, { min?: number; max?: number; [key: string]: unknown }>
  adapters?: Record<string, { parameters: string[] }>
  stored_runtime?: {
    requires_provider_and_model: boolean
    explicit_key_selection_when_configured: boolean
    credential_values_returned: boolean
  }
}

export interface RpcErrorBody {
  code: string
  message: string
  retryable?: boolean
}

export interface RpcReply<T> {
  id: string
  ok: boolean
  data?: T
  error?: RpcErrorBody
}

export interface AuthResult {
  token: string
  expires_in: number
  client_id: string
}

export interface PairResult {
  protocol: number
  client_id: string
  secret: string
  server_id?: string
  fingerprint?: string
  endpoint?: string
  tls_pin?: string
  transports?: string[]
}

export interface StoredIdentity {
  version: 1
  origin: string
  clientId: string
  secret: string
  serverId?: string
  remember: boolean
  pairedAt: number
}

export interface SessionView {
  session_id: string
  revision: number
  event_seq: number
  conversation?: Conversation
  unchanged?: boolean
}

export interface ChunkInfo {
  version: number
  session_id: string
  revision: number
  size: number
  event_seq: number
}

export interface Chunk {
  revision: number
  offset: number
  data: string
  done: boolean
}

export interface ChatSummary {
  id: string
  title?: string
  created_at?: number
  updated_at?: number
  deleted_at?: number
  revision: number
  provider_id?: string
  model_id?: string
  profile_id?: string | null
  reasoning_effort?: string | null
  token_count?: number | null
  has_messages?: boolean
  has_summary?: boolean
}

export interface ListPage {
  version: number
  items: ChatSummary[]
  next_cursor: string
}

export interface ToolActivity {
  id: string
  name: string
  wireName?: string
  status?: string
  arguments?: string
  response?: string
  files?: string[]
  contentOffset?: number
  reasoningOffset?: number
  [key: string]: unknown
}

export interface MessageVersion {
  content: string
  timestamp?: string
  files?: string[]
  toolActivity?: ToolActivity[]
  reasoning_content?: string
  reasoning_content_offset?: number
  reasoning_signature?: string
  generation_interrupted?: boolean
  provider_id?: string
  model_id?: string
  model_display_name?: string
  server_request_id?: string
  [key: string]: unknown
}

export interface ChatMessage {
  id: string
  role: 'user' | 'model' | 'system'
  versions: MessageVersion[]
  active_version_index: number
  [key: string]: unknown
}

export interface ConversationSummary {
  id: string
  content: string
  through_message_id: string
  role?: string
  [key: string]: unknown
}

export interface Conversation {
  id: string
  title: string
  created_at: number
  updated_at: number
  messages: ChatMessage[]
  summary?: ConversationSummary | null
  selected_prompt_ids?: string[]
  [key: string]: unknown
}

export type EventKind =
  | 'delta'
  | 'reasoning_delta'
  | 'tool_activity'
  | 'conversation'
  | 'completed'
  | 'error'
  | 'event_gap'
  | string

export interface SessionEvent {
  session_id: string
  seq: number
  request_id?: string
  kind: EventKind
  data?: Record<string, any>
}

export interface ProviderKey {
  id: string
  label: string
  availability: string
}

export interface CatalogModel {
  id: string
  name: string
  availability: string
  text_generation: boolean
  context_limit?: number | null
  output_limit?: number | null
}

export interface CatalogProvider {
  id: string
  name: string
  type: string
  custom_model_id: boolean
  key_selection_required: boolean
  keys: ProviderKey[]
  models: CatalogModel[]
}

export interface ProviderCatalog {
  version: number
  revision: number
  providers: CatalogProvider[]
}

export interface StoredRuntime {
  source: 'stored'
  provider_id: string
  model_id: string
  key_id?: string
  instructions?: Record<string, string>
}

export interface EphemeralProvider {
  type: 'openai' | 'openai_responses' | 'anthropic' | 'google'
  modelId: string
  baseUrl?: string
  auth: { method: 'bearer_token' | 'none'; value?: string }
  modelConfig?: { temperature?: number; top_p?: number; top_k?: number }
  config?: { maxTokens?: number }
}

export interface EphemeralRuntime {
  source: 'mobile_ephemeral'
  provider: EphemeralProvider
  instructions?: Record<string, string>
}

export type RuntimeConfig = StoredRuntime | EphemeralRuntime

export interface MCPInventoryItem {
  id: string
  name: string
  protocol?: string
  [key: string]: unknown
}

export interface ToolInventoryItem {
  name: string
  configuration_supported?: boolean
  execution_supported?: boolean
  requires_media_io?: boolean
  [key: string]: unknown
}

export interface ModelSelection {
  source: 'stored' | 'mobile_ephemeral'
  providerId: string
  modelId: string
  keyId: string
  providerType: EphemeralProvider['type']
  baseUrl: string
  maxTokens: string
  temperature: string
  topP: string
  topK: string
}

export interface ModelPreference {
  source: 'stored' | 'mobile_ephemeral'
  providerId: string
  modelId: string
  keyId?: string
}
