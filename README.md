# serverside-generative

Self-hosted Go session runtime offering one v5 JSON RPC API over TLS/TCP, SSH stdio, WebSocket and HTTP/SSE. Its v5 API supports the Generative Android **Server Connection** workspace; each paired client has its own server-side conversation history.

## Architecture and pairing

**Current v5 policy (2026-11-15): every conversation is permanently saved in a private per-chat JSON file on the server, regardless of any mobile application's local sync setting.** The server neither requires nor interprets a mobile `memory/sync` chat choice; older `data.storage` values are ignored. `session.open/get` return the complete stored conversation and revision, and `conversation` events push snapshots after durable changes, alongside model/tool progress events. A lost event can be recovered from server storage with `session.get`.

**Only mobile-supplied model provider/MCP/tool runtime settings are temporary RAM data.** They are never written into YAML, encrypted SQLite, chat JSON or backups, and disappear on session close, TTL expiry or server restart. Locally managed server providers and encrypted server-managed credentials are a separate explicit `runtime.source="stored"` configuration.

Server-side features also include a revisioned YAML catalog, encrypted server credentials and custom tools, supported model text-streaming adapters, authorization-controlled MCP/custom HTTP tools, and optional terminal/coding-agent tools. Some providers, built-in settings and media execution flows remain incomplete.

See [server implementation plans](plans/README.md), [v5 API contract](docs/client-api-surface.md), and [design-only future interface diagrams](docs/future-generative-interface.md).

| Client connection type | One-time pairing | Subsequent data traffic |
| --- | --- | --- |
| **TCP** | **SSH only:** `ssh -T USER@HOST generative pair --stdio --transport tcp`. Strict SSH host-key verification required. Result contains durable `client_id`, secret, TLS certificate SHA-256 fingerprint and the direct TCP address. | TLS/TCP on the configured port, with the same identity also valid over **SSH stdio fallback** when a future client implements TCP reachability fallback. No new pairing on reconnect. |
| **HTTP / WebSocket** | **One HTTPS pairing URL**, shown as selectable/copyable text **and** an identical optional QR. The URL's fragment contains a short-lived one-time code and client-visible SPKI pin. Future clients may copy/open/scan it, validate HTTPS/SPKI and redeem the code through `POST /v1/pair` to obtain durable `client_id` and secret. | HTTPS `/v1/auth` yields a short-lived bearer token for `/v1/rpc` and `/v1/events`; or use authenticated `/v1/ws` for full two-way RPC/events. The same HTTP/WS identity authorizes both; it does **not** authorize direct TCP or SSH. |

**QR is not required and is not a different credential or handshake from the displayed URL.** Copying the full URL (including fragment) and scanning its QR must give the same result. Do not place persistent device secrets in URLs or QR. A QR URL code is single-use and expires; protect it as you would a temporary login link.

The server needs a reachable SSH service for SSH pairing or fallback; it does **not** embed an SSH daemon or need a public Generative-specific TCP port for SSH stdio. The SSH command `generative stream --stdio` forwards binary protocol-v5 frames to the **running daemon's private Unix data socket**, not through SSH port forwarding or a second server. The daemon still verifies the paired device identity. TCP fallback is only attempted on reachability failures, not on a rejected TLS certificate or authorization failure. WSS and HTTPS share one HTTP/WS-scoped identity. A future client may fall back to HTTPS RPC/polling on unreachable WSS, but must not downgrade on TLS or authorization failures. The server supports interrupted-stream recovery with event cursors; no Android-side automatic migration has been implemented. Never blindly replay a possibly executed model/tool request after disconnection.

**New-device legacy pairing is disabled by default:** `POST /v1/handshake`, `GET /v1/handshake/ws`, Human Relay PIN/QR and v4 TCP ticket enrollment cannot create new identities. Existing already-paired v4 clients continue to work. An explicit **temporary migration flag** `--allow-legacy-pairing` enables the old enrollment methods for old clients; remove the flag after migration. Password handshake alone does not replace the new connection-type-specific flows.

### Persistence and features

- `~/.config/generative/identity/`: TLS certificate/key, per-client secret hashes, web password hash and session digests, Unix `control.sock` and daemon `run/data.sock` (private filesystem permissions).
- `~/.config/generative/` (or `XDG_CONFIG_HOME/generative/`): revisioned nonsecret `providers.yaml`, `models.yaml`, `mcp.yaml`, `profiles.yaml`; encrypted secret/custom-tool SQLite; **`chats/*.json` for every v5 conversation**. Existing `shared-config.json` remains available for old clients until explicitly migrated.
- OAuth login/refresh, image/video/music binary generation/file storage, and other unsupported adapters are not silently implemented. Unsupported operations return explicit errors.
- Terminal and coding-agent RPC operations are **opt-in** via `--enable-terminal` / `--enable-agent`; authorization is independent from ordinary model tools.
- Metadata edits use a shared validation/CAS path; `config import`, `config diff` and the terminal `manage` editor do not bypass YAML secret restrictions. API keys/custom HTTP tools use encrypted SQLite; live mobile runtime secrets must never enter stored YAML or permanently stored chat JSON.

## Web chat client

With the default `generative serve` settings, open `http://127.0.0.1:7091/` on the server machine. The web workspace opens directly when the web password is unset. The daemon warns on stderr at every startup with the web listener enabled in this mode. Anyone who can reach a passwordless endpoint can use the workspace. Loopback access does not identify an OS user. Keep direct loopback listeners out of reverse proxies and tunnels. A proxy that rewrites the backend Host and strips every forwarding header can be indistinguishable from a local process.

To require a password, run this command under the daemon's OS account while it is running. The prompt hides input. For automation, `--stdin` reads one password from private stdin. Use `--ipc PATH` with a custom control socket.

```sh
generative web password set
```

Use `generative web password reset` to replace the saved password, `generative web password unset` to remove it, and `generative web sessions revoke-all` to revoke sessions independently. An effective password change revokes existing sessions, including any previously bootstrapped without a password. The daemon keeps salted Argon2id verifiers and session digests in `identity/web-auth.json` (mode `0600`). Argon2id uses 64 MiB, 3 passes, 1 lane, a 128-bit salt and a 256-bit output. Only one derivation runs at a time. [OWASP Password Storage Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html#argon2id)

The startup precedence is **explicit `--web-password`**, then **`GENERATIVE_WEB_PASSWORD`**, then the **saved password**, then **passwordless mode**. Explicit empty values are rejected. Both overrides apply only to the running daemon. The private session store keeps a salted verifier to recognize an unchanged override on restart. A saved-password set, reset or unset under an override updates the saved fallback. Change or remove the override and restart to change the effective password. A password flag can be exposed in process listings and shell history. An environment variable can be visible to privileged processes. Prefer the interactive password command or a protected service environment. The service installer rejects password flags in saved service arguments.

For remote browsers, use a dedicated HTTPS hostname, set the exact browser origin, and specify the trusted TLS proxy's source IP. Keep the backend bound to loopback.

```sh
generative serve --http-listen 127.0.0.1:7091 --web-origin https://chat.example.com --web-trusted-proxy 127.0.0.1
```

The proxy must terminate HTTPS, enforce the configured authority, preserve the public upstream `Host` (including a nondefault port), and overwrite `X-Forwarded-Proto` with `https` on requests received over TLS. Redirect or reject incoming HTTP at the proxy. Keep SSE unbuffered and allow long requests on `/web/events`. Forward `/connect` and the existing `/v1/*` routes if you use mobile HTTP/WS pairing on this host. HTTPS web access requires a password by default. `--allow-public-web-without-password` explicitly opens the configured HTTPS origin to anyone who can reach it and triggers an additional startup warning.

Browser sessions survive reload and daemon restart when the effective password and mode stay the same. They expire after 24 hours without authenticated RPC activity or 7 days from issuance, whichever comes first. Direct loopback HTTP uses a separate cookie without the HTTPS Secure flag.

The web workspace has one stable owner. Signing out, resetting the password and restarting preserve its history. Mobile devices and browsers paired through the old console keep their own credentials and durable history. To access or export old browser history, use an existing authenticated client or a trusted previous console build with the existing browser credential for the original origin. In an authenticated v5 client, use `session.list`, then `session.open` to export a conversation (or the chunked read operations for large histories). The server administrator can also list and export these chats using `generative chat list --client-id OLD_ID` and `generative chat export --client-id OLD_ID --id CHAT_ID --out PATH`. The previous browser stored its client ID under `generative.identity.v1.<origin>`. Preserve that credential privately if you still need the old flow. A cross-owner migration is currently unavailable.

Select a server-managed provider/model or enter temporary provider credentials in Model settings. Chats are stored on the server. Temporary provider credentials stay in memory. The daemon releases the web workspace runtime when its last browser session signs out or an administrator revokes every session. Native terminal and coding-agent access continues to use paired clients and the existing server opt-ins. A mobile pairing invitation opened at `/connect` leaves the invitation available for the mobile client to redeem.

The built client is checked in under `internal/node/web` and embedded in the Go binary. To modify it, run `npm ci --prefix web`, `npm run build --prefix web`, then `go build -o generative ./cmd/generative`. The browser tests run against a temporary daemon, HTTPS proxy and fake provider. With Go and OpenSSL installed, run `cd web && npx playwright install chromium && npm run e2e`.

## Install (no Go toolchain required on the deployed server)

Install the **latest published GitHub Release**, including pre-releases, on Linux or macOS (x86-64 / arm64):

```sh
curl -fsSLo install-generative.sh https://raw.githubusercontent.com/starfall-org/serverside-generative/main/install.sh
sh install-generative.sh
generative version
```

The installer selects the binary for your OS/CPU, verifies it against the release's `SHA256SUMS` manifest, and atomically installs `generative` into a user-writable directory on your existing `PATH`. If none is available, it uses `~/.local/bin` and adds that directory to your login shell's `PATH` configuration (open a new shell afterward). **No `sudo` is invoked automatically.** It requires `curl`, `python3` and `sha256sum` or `shasum`. Review the downloaded script before executing it. To pin a particular release, set `SERVERSIDE_TAG=v0.5.0-alpha` when running the script; to choose a destination, set `INSTALL_DIR=/absolute/path` (which must be writable).

To build from source instead, install Go 1.26.10+ and run:

```sh
go build -trimpath -ldflags='-s -w' -o generative ./cmd/generative
./generative version
```

### Run as a managed service

```sh
generative service install -- --tcp-listen 127.0.0.1:7090 --http-listen 127.0.0.1:7091
generative service status
generative service logs
generative service stop
generative service start
generative service restart
generative service uninstall
```

For web access, append the appropriate `--web-origin` and, for an HTTPS proxy, `--web-trusted-proxy` flags to the service install command. The default loopback service opens passwordless and logs the startup warning. To enable protection, run `generative web password set` under the service account or set `GENERATIVE_WEB_PASSWORD` through the service manager’s protected environment. A shell environment is inherited only if the launcher passes it to the daemon. The private configuration directory holds web credentials and sessions across service restarts.

On startup, the daemon initializes a fresh empty YAML catalog and encrypted database automatically; existing incomplete or corrupt catalogs are not overwritten. The installer automatically installs and starts the service for the current user, without root privileges, when installing a release that includes the service commands. Additional installer arguments are passed to `serve`. To install only the binary without registering or starting the service, run `SERVERSIDE_INSTALL_SERVICE=0 sh install-generative.sh`.
Existing installations must migrate the two old directories before starting the new daemon. Record the current serve flags, stop any manually started daemon, and uninstall the old managed service using the old executable. Back up the old directories before relocation (do not run this while the daemon or any CLI writer is running):

```sh
serverside service uninstall
# Install the new binary without registering the new service first.
SERVERSIDE_INSTALL_SERVICE=0 sh install-generative.sh
generative config relocate
generative service install -- # append your previous serve flags here
```

`config relocate` moves `$XDG_CONFIG_HOME/serverside` to `generative` and `serverside-generative` to `generative/identity` (default base: `~/.config`). It preserves chat files, encrypted databases and TLS/client identities, and refuses existing destinations or old service/socket paths. If a stale socket remains, confirm the daemon is stopped before removing that socket. No automatic overwrite or merge is performed. Explicit custom `--data-dir` / `--identity-dir` paths are not relocated. System service names remain unchanged.

Flags after `--` are passed to `serve`. Stop a manually launched daemon before
installing the service. Uninstalling the service does not remove chat or pairing
data. Reinstall to change persistent serve flags.

- Linux with an active user Systemd manager: installs and enables a unit with
  automatic restart. To start at machine boot even before login, enable user
  lingering with `loginctl enable-linger "$USER"` where supported.
- macOS: installs a LaunchAgent with RunAtLoad and KeepAlive, starting at login.
- Other Linux environments: the built-in native supervisor detaches from the
  terminal and restarts the server on exit, without external dependencies.
  For a full machine reboot, configure the host's startup hook to invoke
  `generative service start`. No process can survive destruction of its host.

Use `--backend native`, `--backend systemd`, or `--backend launchd` before
`--` to select a backend explicitly. Use an absolute path for data and identity
directories; services preserve the installation working directory. Do not put
passwords in service arguments. A public HTTPS reverse proxy needs its own
service manager.

### Start the daemon manually

```sh
./generative serve \
  --tcp-listen 0.0.0.0:7090 \
  --advertise server.example.com:7090 \
  --http-listen 127.0.0.1:7091
```

Use a correctly configured **HTTPS reverse proxy** for clients reaching HTTP/WS from another machine. Public plaintext data requests are refused; localhost HTTP is intended for trusted local proxies. The HTTP/WS listener does not need a pairing password for URL/QR onboarding or regular v5 data API access. To use only direct SSH/TCP, add `--http-listen off`.

### One-time TCP pairing over SSH

```sh
ssh -T -o BatchMode=yes -o StrictHostKeyChecking=yes USER@HOST \
  generative pair --stdio --transport tcp
```

It emits one JSON object containing `client_id`, `secret`, `fingerprint`, `endpoint` and `transport:"tcp"`. A future client must store the paired secret in a platform-protected credential store. Subsequent TLS/TCP connections authenticate with that pair, and SSH fallback uses the same pair:

```sh
ssh -T -o BatchMode=yes -o StrictHostKeyChecking=yes USER@HOST generative stream --stdio
```

Use `--data-socket PATH` for a non-default daemon socket. Never allocate a PTY or pass client secrets in SSH argv. Only trusted SSH users should be able to run the pairing command. TCP pairing **does not** use a QR, HTTP request or PIN.

### One URL for HTTP and WebSocket pairing

The server must already be running with a reachable `https://` hostname and valid client-visible TLS identity. Run on the server (or a trusted shell session):

```sh
generative pair --transport http \
  --endpoint https://server.example.com \
  --pin 'sha256/BASE64_SPKI_PIN'
```

The command displays the **complete copyable URL and the matching QR**. A future client may paste/open the complete URL or scan the matching QR; all use the same one-time `POST /v1/pair` exchange. On success it must securely store the HTTP/WS-scoped `client_id` and `secret`. Reconnects do not need the code or QR again. Cancel outstanding codes with:

```sh
generative pair --transport http --cancel
```

For HTTP requests, exchange the saved credentials using `POST /v1/auth`; send the returned 15-minute bearer token in the `Authorization` header for `POST /v1/rpc` and `GET /v1/events`. WebSocket uses `GET /v1/ws` plus a single authenticated JSON message, then the same v5 RPC payloads. `GET /v1/capabilities` reports supported operations/transports without exposing private catalog data.

### Manage the nonsecret catalog and encrypted credentials

```sh
generative config init
generative config validate
generative config export --kind providers --out /tmp/providers.yaml
generative config diff --kind providers --file /tmp/providers-edited.yaml
generative config import --kind providers --file /tmp/providers-edited.yaml --if-revision 1 --dry-run
generative config import --kind providers --file /tmp/providers-edited.yaml --if-revision 1
EDITOR=vi generative manage
generative provider key set PROVIDER_ID --key-id main --stdin --if-revision 2
generative mcp credential set SERVER_ID --method bearer --stdin --if-revision 3
generative provider group add --id research --name Research --sort-order 1 --if-revision 4
generative provider group edit --id research --name "Research models" --sort-order 2 --if-revision 5
generative provider group assign --provider-id PROVIDER_ID --group-id research --if-revision 6
generative provider group clear --provider-id PROVIDER_ID --if-revision 7
generative provider group delete --id research --if-revision 8
```

Catalog `--if-revision` is optimistic concurrency control: update the revision shown by `config validate`/catalog output before applying changes. These commands reject credentials in YAML; enter sensitive values only through explicitly supported encrypted-store `--stdin` commands. The TUI uses your editor on a private temporary file and confirms before publishing validated changes.

All v5 chats can be inspected and edited from durable server JSON without persisting mobile-supplied runtime secrets. Use `generative chat validate-all --client-id OWNER_ID` to check the chat directory for malformed snapshots and tombstones without writing files. After directly editing a valid stored snapshot, `generative chat reload --id CHAT_ID --client-id OWNER_ID --if-revision REVISION` validates it, saves the exact pre-reload envelope under the private `.backups/chats/` directory, and publishes a new revision; use `--dry-run` to check first. Backups contain plaintext chat data and preserve the hand edit, not an earlier known-good version. Active sessions must close and reopen to adopt an external edit. Server CLI mutations take the persistent chat job lease before committing. Direct file editors do not take that lock, so stop concurrent writers while hand-editing to avoid a race with the final atomic replacement. Use `generative chat list --client-id OWNER_ID` to find the current chat revision, then:

```sh
EDITOR=vi generative chat edit --client-id OWNER_ID --id CHAT_ID --if-revision REVISION --dry-run
EDITOR=vi generative chat edit --client-id OWNER_ID --id CHAT_ID --if-revision REVISION --yes
```

`chat edit` validates the full Conversation, rejects unsafe editor files and saves only if the owner and expected revision still match. It never edits transient provider/MCP runtime configuration. A dry run validates but does not save; `--yes` is required to publish a changed chat. Other clients or active sessions must explicitly refresh their snapshots; this command does not push changes into an already running generation.

### Legacy v4 interoperability (migration only)

If an unpaired **old v4 client** absolutely requires password/QR/PIN enrollment, start the server explicitly with `--allow-legacy-pairing` (and optional `--password` or `--human`). This overrides the new TCP-only-SSH pairing requirement **temporarily** and must not be used in normal v5 deployment. Already-paired v4 data sessions need no migration flag to reconnect. Old `generative handshake --stdio`, Human Relay and `/v1/handshake` are migration paths only.

## Test session handoff and offline execution

```bash
python3 testclient/client.py demo --state client-state.json \
  --session my-chat-1 --text 'Generated while the client was offline' --delay-ms 350
```

This **legacy v4 demo** transfers a RAM-only v4 handoff, starts the test `echo` job, **disconnects immediately**, lets it complete on the server, reconnects, replays events, then ACKs them and confirms that they are removed from RAM.

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

## Built-in server terminal tools (opt-in)

`generative` includes these native tools for authenticated, paired clients:

| Tool | What it does |
| --- | --- |
| `terminal_create` | Start a new interactive terminal process; returns an unguessable `terminal_id` |
| `terminal_execute` | Write a shell command to the terminal's stdin; returns the first bounded output chunk |
| `terminal_read` | Read captured PTY output by byte offset, with optional long polling |
| `terminal_close` | Kill the shell/process group and free the in-memory buffers |
| `terminal_list` | List terminals currently running on this personal server |
| `tools_list` | Discover built-in tool schemas and whether terminal execution is enabled |

**Terminal execution is disabled by default** because it grants arbitrary OS command execution with the permissions of the `generative` process. On a machine you control, use a dedicated **unprivileged OS account** and working directory:

```bash
./generative serve --enable-terminal --terminal-cwd /srv/generative-work
```

Then, using a paired Python test client:

```bash
python3 testclient/client.py terminal tools --state client-state.json
python3 testclient/client.py terminal create --state client-state.json
# Copy the terminal_id from the JSON response:
python3 testclient/client.py terminal execute --state client-state.json \\
  --terminal-id YOUR_TERMINAL_ID --command "pwd; ls -la"
python3 testclient/client.py terminal read --state client-state.json \\
  --terminal-id YOUR_TERMINAL_ID --offset 0 --wait-ms 300
python3 testclient/client.py terminal close --state client-state.json \\
  --terminal-id YOUR_TERMINAL_ID
```

The read/execute responses provide `text` plus `data_base64` (the original bytes), `offset`, `next_offset`, `truncated`, `running` and, after process exit, `exit_code`. Use the returned `next_offset` on the next read; no need to resend previous bytes. A `truncated:true` response means earlier output exceeded the bounded buffer.

Terminals stay alive **after the client disconnects**, may be accessed by any paired device in this single-owner deployment, and survive multiple concurrent chat requests. They are **not written to the database** and disappear on server restart. At most **8** terminals run at once, each retaining up to **256 KiB** of output, with at most **64 KiB** per read. The 30-minute idle cleanup applies only to terminal processes that already exited.

On Linux/macOS, terminals use a real PTY and closing terminates their process group. Windows builds provide a shell/pipe fallback, not a ConPTY TTY. Terminal commands can themselves create/modify files; the server does not sandbox arbitrary OS commands. Do not give access to a paired device you do not trust.

**Current limitation:** these tools can be called through the authenticated client protocol and discovered through `tools_list`. The model SDK text-streaming loop does not yet automatically expose/execute them as AI-requested function calls; agent tool dispatch needs a separate implementation and appropriate execution controls.

## Shared configuration sync for multiple personal devices

The **legacy v4 shared configuration API below** is for a single owner using several paired clients. V5 instead exposes `config.get/save` and permanently stored chat JSON. Every paired client can read and update the same durable instructions, skills and non-secret provider metadata. The server owns the shared configuration's monotonic revision; client devices may cache a local copy, but must never blindly replace a newer server revision with old local data.

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
| `tools_list` | `tools_list {tools}` | Get built-in tool names, JSON schemas and enablement |
| `terminal_create {rows, cols}` | `terminal_created {terminal_id}` | Create PTY, only if the server enables terminal execution |
| `terminal_execute {terminal_id, command, wait_ms}` | `terminal_executed` | Execute a command; return bounded PTY output |
| `terminal_read {terminal_id, offset, max_bytes, wait_ms}` | `terminal_output` | Read terminal buffer incrementally |
| `terminal_list` | `terminal_list {terminals}` | Discover all active terminals on the personal server |
| `terminal_close {terminal_id}` | `terminal_closed` | Terminate terminal and free resources |
| `sync {session_id, since}` | `sync_begin`, `event*`, `sync_end` | Replay events then continuously subscribe |
| `ack {session_id, seq}` | `ack_ok` | Release events **after** local durable write |
| `status {session_id}` | `status` | Inspect active in-RAM job/event counts |
| `delete {session_id}` | `deleted` | Clear RAM session and cancel jobs |
| `revoke` | `revoked` | Delete current client's reconnect credential |

Each payload is prefixed by a **4-byte unsigned big-endian byte length**. Credentials must never be logged. Client identity scopes each session, preventing cross-client access to the same session ID.

## Limitations / next steps

- The API/MCP SDK adapters support text generation and explicit MCP calls. Built-in terminal tools are directly callable by an authenticated client, but **automatic model tool-use loops** are not yet wired into the SDKs.
- TCP hole punching and address discovery remain separate work. Legacy v4 signaling still requires a reachable TCP data endpoint; v5 can also use the HTTP, WebSocket or Linux SSH stdio data paths.
- **Legacy v4** RAM event buffers do not survive restart. **V5 chat history remains stored in JSON** and can be reopened by session ID; only mobile-supplied provider/MCP runtime must be re-supplied. V5 live event sequences remain bounded RAM.
- **Legacy v4** in-memory limits include 128 sessions, 2 MiB state and 2,048 retained events (`sync_gap`). V5 also bounds RAM events (`event_gap`) and currently has a roughly 3 MiB full-conversation limit within a 4 MiB transport frame; larger chat chunking remains to be implemented.
- A future Android client should securely store paired identity, but its local temporary/sync setting must never change the server's unconditional v5 chat persistence; Android integration is not in scope.
- Credential revocation should be enforced across already-active connections as this project evolves.
- SDK/network errors are intentionally redacted to a generic `remote_execution_failed` event to avoid leaking API credentials or sensitive response metadata. More detailed safe error classes are future work.
- SDK dependencies may increase binary size; CI builds standalone binaries without requiring a Go installation on the deployment server.
