# ServerSide — Problems identified for the Generative “Server Connection” feature

Reviewed: 2026-11-17. **Tracker:** these are server-side requirements/gaps and their current disposition. Keep fixes in `~/serverside-generative`, not as compatibility hacks in `~/generative`.

## P1 — Incomplete remote session-list metadata and unsuitable list ordering (interface blocker)

**Resolved:** list responses project conversation metadata and sort by update/deletion time with an ID tie-break. Snapshot cursors now preserve an owner-bound list across concurrent updates, with bounded retention and expiry; legacy offset pages remain available.

**Observed:** `internal/chatsession/manager.go` `ListItem` exposes only `id`, `title`, `updated_at`, `revision`, `deleted_at`. `Manager.List` sorts by `ID` and paginates by numeric offset. Android's `shared/model/ConversationListItem.kt` needs `createdAt`, `updatedAt`, `providerId`, `modelId`, `profileId`, `reasoningEffort`, `hasMessages`, `hasSummary`, etc.; its history is sorted by newest update. The current API would force mobile to fetch each complete conversation to render normal chat rows, or to degrade the UI.

**Server-side resolution:** offer a lightweight, owner-scoped list projection with sufficient supported metadata and newest-first stable pagination (including deterministic tie-breaks, tombstones/change visibility). Keep the projection updated from durable snapshots without materializing every full transcript in the client. Define a clear list DTO/version and test large lists, concurrent updates and pagination.

## P2 — No typed, read-only server provider/model selection surface (interface blocker)

**Resolved:** `provider.catalog` provides authenticated versioned nonsecret selection metadata, including explicit credential or key-selection state and unknown model limits.

**Observed:** `internal/rpc/dispatch.go` offers `config.get`, returning the raw YAML catalog through `catalog.Get()`, but no versioned, typed `provider.list`/`model.list` read API suitable for Server Connection's remote model picker. `runtime.source="stored"` requires a `provider_id` and `model_id` (`internal/storedruntime/resolve.go`). The Android client should not have to parse server-owned YAML layouts or mirror the Go server's provider rules.

**Server-side resolution:** expose an authenticated read-only metadata/capability endpoint or v5 RPC for available **server-managed** providers/models, display names, supported operations and runtime requirements. Never expose secrets. Include contract tests and stable versioning; treat selection with missing/unavailable credentials explicitly.

## P3 — `session.run` cannot execute the full Android chat model (feature gap)

**Progress:** selected instruction IDs can be resolved from explicit, transient runtime instruction text and forwarded in selection order. Missing or duplicate IDs fail closed. Server-owned instruction resolution, reasoning/tool histories, multimodal inputs, and generation tool loops remain unsupported.

**Observed:** `internal/chatsession/run.go` `Run` and `runtimeState` implement text-only generation and reject histories with files, tool activities, reasoning content/signatures, selected prompt IDs, enabled model tools and other mobile settings. `runtimeState` currently recognizes only a subset of provider types and configuration parameters. `internal/rpc/dispatch.go` exposes tool listing/explicit calls, but those are not a complete AI tool-use loop. Server `README.md` also documents media/OAuth limits.

**Server-side resolution:** define and implement server-owned, versioned multimodal message/attachment transport, tool-call cycles and results, supported reasoning/model settings, and provider capability handling where parity is required. Return precise feature availability; preserve existing conversation fields rather than require Android to strip them or transparently run tools on-device. Until implemented, remote mode should explicitly restrict the affected operations and not claim parity.

## P4 — Whole-chat size limit prevents large existing-format conversations (feature gap)

**Progress:** versioned, owner-bound chunked read and CAS upload support conversations up to 8 MiB, with checksums, bounded memory and explicit restart recovery. Whole-snapshot RPCs and generation remain limited to 3 MiB; Android chunk support requires further live integration validation. Session listing now validates and projects one stored snapshot at a time, caps projected metadata and page bytes, and does not accumulate full transcripts. Directory entries are still enumerated in memory; a fully indexed/paged store and live-device validation remain open.

**Observed:** `internal/chatsession/manager.go` caps logical conversation payload at `3 << 20` bytes and `internal/node/transport.go` caps frames at `4 << 20`. `session.open/get/save` and `conversation` events transfer whole snapshots, and `docs/client-api-surface.md` calls chunking future work.

**Server-side resolution:** design bounded paging/chunking or versioned incremental conversation transfer with atomic revisions, checkpoints and recovery. Keep secure redaction and conflict semantics. Test large histories/attachments and partial-transfer reconnects. The app should not truncate legitimate chat data to fit this ceiling.

## P5 — Coarse capability negotiation hides runtime-specific unsupported features (integration quality)

**Progress:** `chat_features` version 1 now advertises adapters, runtime sources, workspace scope, size limit and major unsupported generation features. The typed catalog reports per-key credential availability and runtime validation where selection is unambiguous, and explicitly unknown model limits; upstream limits are not known to the server. Negotiation version 1 describes per-adapter parameters, modalities, history and bounds. Multi-key models are checked using an available explicitly selected key; upstream-specific limits and availability remain unknown.

**Observed:** `internal/node/v5.go` `capabilities()` advertises `session.run`, `tool.list`, `tool.call`, etc. as operation names; it does not describe available model adapters, supported message modalities, tool-loop availability, upload support, per-model limits, or server-side provider selection constraints. `tool.list` may separately report tool execution as unsupported, but that does not fully describe chat-generation capabilities.

**Server-side resolution:** extend capability responses with stable, non-secret **feature-level** support and constraints, so mobile can disable unsupported remote controls accurately without hard-coding server implementation details. Include tests for disabled and unavailable features.

## P6 — Check multi-device conversation visibility before offering shared-history semantics (design decision)

**Resolved:** sessions are scoped to the authenticated paired client identity; independently paired devices do not share history. This is documented in `docs/client-api-surface.md` and covered by session authorization tests. A shared-account workspace is not currently offered.

**Observed:** `internal/node/v5.go` authenticates requests to the paired `client_id` owner; `internal/chatsession/manager.go` and `chatstore` scope stored sessions/list to that owner. Different devices paired independently may therefore have distinct views on the same physical server, even though both choose the same ServerSide endpoint.

**Server-side resolution (if cross-device shared chats are intended):** define explicit server workspace/account authorization separate from device credentials, including revocation and access controls. Do not make the Android client share secrets or copy chats between device identities as a workaround. If isolation is intended, document it clearly in capabilities and the UX contract.

## P7 — Paired HTTP identity cannot revoke itself (connection lifecycle gap)

**Resolved (HTTP):** `POST /v1/revoke` is authenticated with the caller's bearer token and accepts an empty body only; no client ID can be supplied or targeted. It calls the existing server-side `revokePair` path for that bearer owner, which terminates active sessions and invalidates all tokens for that pairing. Repeated requests with the revoked bearer get 401. Existing owner-scoped durable history remains stored, but a newly paired identity cannot access it. The API is documented in `docs/client-api-surface.md`; `http_revocation_test.go` covers invalid targets, invalidation, other-client isolation and durable-chat retention.

**Android:** explicit user-confirmed server revocation is implemented in Server Connection and deletes local credentials only after a confirmed response; local-only Remove and Re-pair do not revoke old server identities. Full pinned-TLS/live pairing and revoked-identity end-to-end testing remain open.

## P8 — New Android connections cannot resume live events after historical event eviction

**Resolved for updated ServerSide v5:** `session.open` and `session.get` now return
`event_seq` from the authenticated, live, owner-scoped session. `session.open_chunked`
returns the same cursor. Android reopens the session immediately before a new
`session.run`, then subscribes from `event_seq`; it does not reuse stale cursors
across daemon/session restarts. `event_seq` is ephemeral (not a durable revision)
and is deliberately ignored by older clients. Backend tests simulate eviction of
2,048 old events and confirm a fresh cursor remains valid and reset after restart.

**Compatibility:** Servers deployed without `event_seq` may still reject an old
`after=0` cursor after eviction. The Android client preserves its existing safe
snapshot-recovery behavior, does not automatically replay an uncertain run, and
requires the updated server to guarantee gap-free new-run streaming after a
fresh connection. Past evicted deltas cannot be reconstructed from history.

## Suggested resolution order

P1 and P2 block matching existing app list/model-picker behavior. P5 defines honest feature negotiation. P3 and P4 gate advanced remote chat parity. P6 requires a product decision before multi-device shared history is promised. Implement and test fixes on the server first; update `docs/client-api-surface.md` with the resulting wire contracts.

## P9 — Cached Android history previously required full `session.open` transfers

**Resolved:** `session.open` and `session.get` accept the optional `if_revision`
field and respond with `unchanged: true`, current revision and `event_seq` when
an authenticated client's known cached revision matches. The server still loads
and validates the owner-bound durable session; this is a read-only conditional
response, not chat upload/synchronization. Mismatched revisions return the
actual transcript (or the existing chunked-transfer path for large content).
RPC tests cover unchanged, changed, owner-bound and 4 MiB conversation cases.
Android keeps an identity-scoped encrypted, evictable local mirror to avoid
transferring unchanged remote history; older servers can still return full
snapshots without violating the client contract. This does not share chats
across paired identities and does not make Android's Room local chat authoritative.

## P10 — SSH-stdio paired client self-revocation (connection lifecycle)

**Implemented; live-device verification outstanding (2026-11-18).** The v5 `identity.revoke` RPC accepts no request `data` and acts only on the owner authenticated by the TCP/SSH-stdio frame. It returns `{"revoked":true}` only after successful server revocation. Existing durable conversations remain stored; other paired identities are unaffected. This is separate from HTTP's `POST /v1/revoke`; the legacy `type:"revoke"` frame is not used. Android calls the v5 RPC and removes local credentials only after confirmation. Do **not** replay an ambiguous revocation. Go authorization/isolation tests and Android in-memory framed I/O tests pass; real SSH server/device and connection-interruption validation are still pending. Older deployed server binaries without this RPC require upgrade before SSH self-revocation works.

## P11 — Local catalog import parity

**Partially implemented:** authenticated `provider.import`, `mcp.import` and
`mcp.catalog` now persist selected compatible mobile records using the existing
catalog and encrypted credential storage. Model IDs, model configuration,
multiple API keys, MCP headers and query authentication are preserved for
supported records. Existing IDs are not overwritten. Android discovers these
operations via capabilities.

**Outstanding:** migrating OAuth adapter token stores and supporting imported
provider-specific private headers, proxy settings and private provider URLs
requires additional server runtime support. Unsupported records fail closed.
Live-device testing and deployment remain outstanding. OpenCode provider
import has no verified public write API; its MCP add API is runtime-only.

## P12 — SSH RPC reply starvation and misleading open errors

The SSH stdio client polls `session.events` using an explicit cursor. Previously
the daemon also pushed events automatically onto the same framed pipe, causing
an unnecessary duplicate stream and potentially blocking RPC replies under
large event backlogs. SSH stdio now defaults to polling; an explicit `after` on
open opts into push. TCP/WS also subscribe on chunked open. Data-free opens
accept omitted/null payloads, and deleted-session opens return `session_deleted`
instead of an opaque internal error. Regression tests cover the polling wire,
deleted chat and optional open data. Live-device verification remains pending.
