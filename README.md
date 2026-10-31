# serverside-generative

A self-hosted Go CLI session runtime for Generative Android. Execution jobs and API keys remain in RAM; the owner's shared instructions, Agent Skills and non-secret provider catalog are persisted in one private JSON configuration. No Go toolchain, database service, Firebase, or broker is needed at deployment.

> **Current status: Go SDK-backed remote execution alpha.** The server can stream text from OpenAI Responses, OpenAI Chat Completions (including compatible endpoints), Google Gemini, and Anthropic Claude, and can explicitly list/call Streamable HTTP MCP tools. It does **not** yet support autonomous model tool loops, Android integration, multimodal requests, or NAT hole punching.

## Architecture

- **Handshake / pairing** establishes a short-lived ticket and pins the server's TLS certificate.
- **Data transport** is direct TLS 1.3 over TCP, with 4-byte big-endian length-prefixed JSON frames (max 4 MiB). The framing can later evolve to Protobuf without changing the handshake channels.
- **Persistent reconnect identity** is a randomly generated 256-bit client secret; the server stores only its SHA-256 hash (0600). Android / testclient stores the secret locally (0600).
- **One shared configuration for a personal server**: all paired devices see the same instructions, Agent Skills, base URLs and model catalogs. The server stores one private JSON file, not separate copies per client. Pairing itself is per device; shared configuration is per server.
- **Optimistic configuration sync**: each connection confirms the current SHA-256 digest, fetches server changes, or updates the shared configuration with an expected revision. Concurrent/stale device writes are rejected rather than silently overwriting another device's edits.
- **Ephemeral execution** keeps in-progress chat turns, pending events, API keys, MCP auth headers and running jobs in RAM. No session execution state, API credential, conversation transcript or model output is written to disk by this application.
- **Reconnect** uses the saved client ID and secret to reauthenticate directly over TLS, without repeating SSH / HTTP / QR. Client sends `sync` and the last locally committed sequence; only missing events are returned.
- **ACK** from Android is sent only **after** local persistence succeeds; ACK removes consumed event data from server RAM. Gaps are explicit errors, not silent data loss.
- **Restart** preserves instructions, skills, provider URL/model catalogs, TLS identity, and pairing hashes. It loses running jobs and pending chat results. Android re-handoffs active sessions, but does not need to upload unchanged configuration again.

### Important network limitation

**Pairing is not NAT traversal.** This MVP connects to a reachable TCP endpoint and does *not* implement TCP simultaneous-open/hole punching, STUN, TURN, a relay, or automatic address discovery. SSH/HTTP can carry the pairing ticket and be closed afterwards, but the separate TLS/TCP endpoint must remain reachable. Human Relay requires out-of-band transfer of a QR/code and a reachable TCP transport; a QR alone cannot traverse two restrictive NATs.

## Install (no Go required on the server)

Download the prebuilt `serverside` CLI for your OS/CPU from the repository CI artifacts or a tagged release. On Linux:

```bash
chmod +x serverside
./serverside version
```

To build **as a developer**, install Go 1.26.10+ and run:

```bash
go build -trimpath -ldflags='-s -w' -o serverside ./cmd/serverside-generative
```

### Start the server

For local experimentation:

```bash
export GENERATIVE_HANDSHAKE_PASSWORD='replace-with-a-long-random-password'
./serverside serve --human
```

For another device on a reachable network:

```bash
export GENERATIVE_HANDSHAKE_PASSWORD='replace-with-a-long-random-password'
./serverside serve \
  --tcp-listen 0.0.0.0:7090 \
  --advertise server.example.com:7090 \
  --http-listen 127.0.0.1:7091 \
  --human
```

`--advertise` must name an address **reachable by the client**, not `0.0.0.0`. Never expose the plaintext HTTP handshake listener to the public Internet. Put it behind a properly authenticated HTTPS/WSS TLS-terminating reverse proxy, or use SSH/Human pairing instead. The TCP data channel uses pinned TLS 1.3.

The server stores these files under `~/.config/serverside-generative/` (private directory and file permissions):

| File | Purpose |
| --- | --- |
| `identity.key`, `identity.crt` | Long-lived TLS identity (so reconnect does not require approving a new fingerprint) |
| `clients.json` | Client IDs and **hashes** of client reconnect secrets |
| `control.sock` | Unix socket (only while server is running) |
| `shared-config.json` | One shared durable configuration for all paired clients: instructions, Agent Skills (SKILL.md text), provider IDs/base URLs/model catalog. Private file, mode 0600 |

**No API keys, authorization tokens, or MCP credential headers are persisted.** Provider metadata has a strict schema and rejects API key fields and base URLs containing query parameters. Chat execution state and event buffers remain in RAM. Clients must never include credentials in instruction/skill text or model names because these *are intentionally persisted*. OS swap, core dumps, and external provider logs should still be controlled to prevent sensitive RAM data from reaching disk.

## Handshake: three interchangeable methods

### 1. SSH (no application password)

The client uses a previously trusted SSH host and public-key login; the SSH session runs a one-shot CLI command through a permission-limited local Unix socket:

```bash
ssh -T -o BatchMode=yes -o StrictHostKeyChecking=yes user@server \
  serverside handshake --stdio
```

The response is a short-lived JSON invitation. SSH can close immediately; the client uses its ticket to establish the **separate direct TLS/TCP** transport. The server runs no embedded SSH daemon; install/configure OpenSSH and authorized keys separately. Without a reachable SSH path, this handshake cannot work.

Python test client:

```bash
python3 testclient/client.py pair --method ssh --ssh-target user@server --state client-state.json
```

### 2. HTTP / WebSocket (password required)

Set `GENERATIVE_HANDSHAKE_PASSWORD` (at least 12 characters) or `--password` (environment variable is safer: command-line arguments may be visible in process listings). Client tries:

1. `GET /v1/handshake/ws`: WebSocket upgrade + password in JSON message;
2. if WS is unsupported: `POST /v1/handshake`: password in JSON body.

Bad credentials never trigger a fallback attempt. HTTP always returns a short-lived invitation; neither method transports session data.

```bash
python3 testclient/client.py pair --method http \
  --url http://127.0.0.1:7091 --password 'replace-with-a-long-random-password' \
  --state client-state.json
```

For remote clients, use HTTPS / WSS with a certificate verified by the client. Plaintext HTTP passwords are rejected for non-loopback URLs unless a **test-only** override is specified.

### 3. Human Relay (QR / manual PIN, no HTTP or SSH)

```bash
./serverside serve --human --http-listen off
# Any time afterwards, print a fresh 2-minute invite:
./serverside pair
```

The terminal displays a QR encoding `generative://pair?endpoint=...&ticket=...&fingerprint=...`. If the client cannot scan, manually enter the endpoint, **12-digit one-time PIN**, and TLS fingerprint displayed in the terminal. The QR ticket and PIN expire in 2 minutes and are single-use.

```bash
python3 testclient/client.py pair --method human --invite 'generative://pair?...' --state client-state.json
# or
python3 testclient/client.py pair --method human \
  --endpoint 192.168.1.5:7090 --fingerprint SHA256_HEX --pin 012345678901 \
  --state client-state.json
```

**Human Relay is an out-of-band handshake, not a hosted relay service.** It never opens SSH or HTTP endpoints; only the direct TLS/TCP data port is required.

## Test session handoff and offline execution

```bash
python3 testclient/client.py demo --state client-state.json \
  --session my-chat-1 --text 'Generated while the client was offline' --delay-ms 350
```

This test transfers a RAM-only chat turn, starts the test `echo` job, **disconnects immediately**, lets it complete on the server, reconnects, replays events, then ACKs them and confirms that they are removed from RAM.

The test client requires **Python 3.10+ standard library only**. To run the full end-to-end suite:

```bash
go test ./...
python3 -m unittest discover -s testclient -p 'test_*.py' -v
```

## Real API execution with official Go SDKs

Model adapters import and call the official Go SDKs:

| Platform | Go package | In-memory provider platform |
| --- | --- | --- |
| OpenAI Responses | [openai-go/v3](https://github.com/openai/openai-go) | `openai-responses` |
| OpenAI Chat Completions | [openai-go/v3](https://github.com/openai/openai-go) | `openai-chat` |
| Anthropic Claude | [anthropic-sdk-go/v2](https://github.com/anthropics/anthropic-sdk-go) | `anthropic` |
| Google Gemini Developer API | [google.golang.org/genai](https://github.com/googleapis/go-genai) | `gemini` |
| MCP Streamable HTTP | [modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk) | `mcp` array |

Only `openai-chat` offers OpenAI-compatible non-OpenAI providers via a custom `base_url` (not all vendors are truly compatible). Select models explicitly; the server never hardcodes a default model or API key.

**Handoff example (`runtime.json`, created locally on the client for testing):**

```json
{
  "provider": {
    "platform": "openai-responses",
    "api_key": "YOUR_API_KEY",
    "model": "YOUR_MODEL_NAME",
    "max_tokens": 4096
  },
  "conversation": [
    {"role": "system", "content": "You are helpful."},
    {"role": "user", "content": "Continue this conversation."}
  ],
  "mcp": [
    {
      "id": "tools",
      "url": "https://mcp.example.com/mcp",
      "headers": {"Authorization": "Bearer MCP_CREDENTIAL"}
    }
  ]
}
```

The Android client should construct this JSON **in RAM** from the currently active providers/MCP servers. Do not save keys in a permanent server file. The Python smoke client intentionally accepts a file for testing only; protect/delete that local file.

After pairing the Python client, run a real model request:

```bash
python3 testclient/client.py run \
  --state client-state.json \
  --config runtime.json \
  --kind model --session chat-123
```

The Python client sends the session state over pinned TLS, starts the job, **disconnects**, reconnects, and waits for streaming output. It deliberately does not ACK the events because merely printing text to stdout is not a durable client-side commit.

Query the MCP tools explicitly:

```bash
python3 testclient/client.py run --state client-state.json \
  --config runtime.json --kind mcp-list --mcp-server tools
```

Call an MCP tool only after the user has authorized it. Supply arguments in an object JSON file:

```bash
python3 testclient/client.py run --state client-state.json \
  --config runtime.json --kind mcp-call --mcp-server tools \
  --mcp-tool example_tool --arguments-file arguments.json
```

The model SDK adapters currently support **text streaming only**; model tool calls and MCP execution are separate. An MCP tool is **never automatically executed by model output**. MCP Streamable HTTP is supported; local stdio processes and OAuth flows are not yet implemented. Both HTTP provider endpoints and MCP endpoints require HTTPS, except for literal loopback hosts in development.

## Shared configuration sync for multiple personal devices

This server is designed for **a single owner using several paired clients**. Every paired client can read and update the same durable instructions, skills and non-secret provider metadata. The server owns the shared configuration's monotonic revision; client devices may cache a local copy, but must never blindly replace a newer server revision with old local data.

A compatible client exchanges `auth → paired(shared_revision,context_digest) → context_get | context_sync | context_update → session traffic`. The entire configuration travels over TLS with certificate pinning. On every reconnect, unchanged data costs only a digest confirmation.

Example non-secret shared configuration:

```json
{
  "instructions": "Answer concisely.",
  "skills": [
    {"id": "reviewer", "name": "Reviewer", "content": "# Reviewer\\nReview code carefully."}
  ],
  "providers": [
    {"id": "main", "name": "OpenAI-compatible", "platform": "openai-chat",
     "base_url": "https://api.example.com/v1", "models": ["model-a", "model-b"]}
  ]
}
```

**Reading**: after auth, send `context_get`; the server returns `context_state` with its current revision, digest and full shared configuration. Any paired client can fetch it.

**Writing**: send `context_update` with the `revision` last read and the new full `context`. The server atomically persists it only if the expected revision is still current; otherwise it rejects the write as a conflict. A full replacement also supports deletions. Changes from one client become available to the others immediately.

**Unchanged**: send `context_sync` with the current `revision` and `digest`. Server checks them and ACKs without transferring the content again.

The Python test client offers an optional `--context-file` on initial pairing. If the server is empty, it can upload that file. If the server has changed and the local file has not, the test client fetches the newer copy. If *both* were edited, it reports a conflict instead of overwriting anything. Without a context file it fetches the shared data read-only.

```bash
python3 testclient/client.py pair \\
  --method http --url http://127.0.0.1:7091 \\
  --password 'your-long-password' --state client-state.json \\
  --context-file client-context.json
```

For each active model request, Android may send only `{"provider":{"id":"main","model":"model-a","api_key":"..."}}`: platform and base URL are resolved from shared metadata. **API keys are not part of the persisted configuration**. Provider metadata rejects unknown fields and URL query parameters that could contain credentials; sensitive chat execution state and MCP authorization headers remain RAM-only.

The on-wire protocol is **v4** and is incompatible with earlier alpha clients. Configuration is shared; in this alpha, live chat sessions and their pending events are still scoped to the paired client that created them.

## Framed data protocol v4

After TLS certificate fingerprint validation, send length-prefixed JSON:

```json
{"type":"auth","ticket":"ONE_TIME_TICKET"}
```

Server replies `{"type":"paired","client_id":"...","secret":"...","protocol":4,"shared_revision":1,"context_digest":"..."}`. Persist the ID and secret on the Android client securely. Reconnect with `{"type":"auth","client_id":"...","secret":"..."}`; no new ticket is required.

Every connection must fetch, confirm or update the shared configuration before session traffic. Existing paired clients do not need another SSH/HTTP/QR handshake.

Additional messages:

| Client message | Server message | Purpose |
| --- | --- | --- |
| `context_get` | `context_state {revision, digest, context}` | Fetch the shared configuration for any paired client |
| `context_sync {revision, digest}` | `context_ack {revision, digest, cached:true}` | Confirm unchanged shared config without sending contents |
| `context_update {revision, context:{instructions, skills, providers}}` | `context_ack {revision, digest, cached:false}` | Atomic compare-and-swap update of global shared configuration; reject stale revisions |
| `handoff {session_id, revision, state}` | `handoff_ack` | Replace in-memory execution state with strictly increasing revision |
| `start {session_id, request_id, kind:"echo", content, delay_ms}` | `start_ack` | Test-only async echo job, idempotent by request ID |
| `start {session_id, request_id, kind:"model"}` | `start_ack`, then `event` | Run provider SDK in background using RAM-only handoff state |
| `mcp_list {session_id, request_id, mcp_server}` | `start_ack`, then `mcp_tools` event | List available MCP tools with official SDK |
| `mcp_call {session_id, request_id, mcp_server, mcp_tool, arguments}` | `start_ack`, then `mcp_result` event | Explicitly call authorized MCP tool |
| `sync {session_id, since}` | `sync_begin`, `event*`, `sync_end` | Replay events then continuously subscribe |
| `ack {session_id, seq}` | `ack_ok` | Release events **after** local durable write |
| `status {session_id}` | `status` | Inspect active in-RAM job/event counts |
| `delete {session_id}` | `deleted` | Clear RAM session and cancel jobs |
| `revoke` | `revoked` | Delete current client's reconnect credential |

Each payload is prefixed by a **4-byte unsigned big-endian byte length**. Credentials must never be logged. Client identity scopes each session, preventing cross-client access to the same session ID.

## Limitations / next steps

- The API/MCP SDK adapters are functional, but only text-generation and explicit MCP calls are implemented; no automatic model tool-use loop, local MCP stdio command execution, or file/multimodal inputs yet.
- TCP hole punching and address discovery remain separate work. Neither HTTP, SSH nor QR signaling alone removes the requirement for a reachable data endpoint.
- No persistent event recovery after server restart. Android must resend active **chat execution state**; shared instructions, skills and provider catalogs survive. Cross-device chat-session replay is separate from shared configuration and is not yet implemented.
- Current in-memory limits are 128 sessions, 2 MiB state per session and 2,048 retained events per session; overflow yields `sync_gap`.
- A real Android client should persist reconnect secrets in Android Keystore-backed storage, and release Temporary Chats without handing them off.
- Credential revocation should be enforced across already-active connections as this project evolves.
- SDK/network errors are intentionally redacted to a generic `remote_execution_failed` event to avoid leaking API credentials or sensitive response metadata. More detailed safe error classes are future work.
- SDK dependencies may increase binary size; CI builds standalone binaries without requiring a Go installation on the deployment server.