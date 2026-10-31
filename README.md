# serverside-generative

A self-hosted, **memory-only session runtime** for Generative Android. Single-file Go CLI binary at deployment time; no Go toolchain, database, Firebase, broker, or permanent session storage is required.

> **Current status: transport + pairing + replay MVP.** The server has a deliberately small *test-only echo runtime*. It does **not** yet invoke real AI providers, MCP servers, or Android tools. Do not treat it as a finished remote execution feature.

## Architecture

- **Handshake / pairing** establishes a short-lived ticket and pins the server's TLS certificate.
- **Data transport** is direct TLS 1.3 over TCP, with 4-byte big-endian length-prefixed JSON frames (max 4 MiB). The framing can later evolve to Protobuf without changing the handshake channels.
- **Persistent reconnect identity** is a randomly generated 256-bit client secret; the server stores only its SHA-256 hash (0600). Android / testclient stores the secret locally (0600).
- **Session runtime** keeps all chat state, API/MCP configuration, live events, and running jobs in RAM. No session, snapshot, event, credential, or output is written to disk by this application.
- **Reconnect** uses the saved client ID and secret to reauthenticate directly over TLS, without repeating SSH / HTTP / QR. Client sends `sync` and the last locally committed sequence; only missing events are returned.
- **ACK** from Android is sent only **after** local persistence succeeds; ACK removes consumed event data from server RAM. Gaps are explicit errors, not silent data loss.
- **Restart** loses all runtime sessions and pending results. The TLS identity and paired client credential hashes survive; Android must re-handoff context.

### Important network limitation

**Pairing is not NAT traversal.** This MVP connects to a reachable TCP endpoint and does *not* implement TCP simultaneous-open/hole punching, STUN, TURN, a relay, or automatic address discovery. SSH/HTTP can carry the pairing ticket and be closed afterwards, but the separate TLS/TCP endpoint must remain reachable. Human Relay requires out-of-band transfer of a QR/code and a reachable TCP transport; a QR alone cannot traverse two restrictive NATs.

## Install (no Go required on the server)

Download the prebuilt `serverside-generative` CLI for your OS/CPU from the repository CI artifacts or a tagged release. On Linux:

```bash
chmod +x serverside-generative
./serverside-generative version
```

To build **as a developer**, install Go 1.25+ and run:

```bash
go build -trimpath -ldflags='-s -w' -o serverside-generative ./cmd/serverside-generative
```

### Start the server

For local experimentation:

```bash
export GENERATIVE_HANDSHAKE_PASSWORD='replace-with-a-long-random-password'
./serverside-generative serve --human
```

For another device on a reachable network:

```bash
export GENERATIVE_HANDSHAKE_PASSWORD='replace-with-a-long-random-password'
./serverside-generative serve \
  --tcp-listen 0.0.0.0:7090 \
  --advertise server.example.com:7090 \
  --http-listen 127.0.0.1:7091 \
  --human
```

`--advertise` must name an address **reachable by the client**, not `0.0.0.0`. Never expose the plaintext HTTP handshake listener to the public Internet. Put it behind a properly authenticated HTTPS/WSS TLS-terminating reverse proxy, or use SSH/Human pairing instead. The TCP data channel uses pinned TLS 1.3.

The server normally persists only these connection-related files under `~/.config/serverside-generative/`:

| File | Purpose |
| --- | --- |
| `identity.key`, `identity.crt` | Long-lived TLS identity (so reconnect does not require approving a new fingerprint) |
| `clients.json` | Client IDs and **hashes** of client reconnect secrets |
| `control.sock` | Unix socket (only while server is running) |

No provider credentials, chat contents, event logs, MCP configurations, or snapshots are stored. Operating-system swap, core dumps, and external provider logs must be controlled separately if strict non-persistence is required.

## Handshake: three interchangeable methods

### 1. SSH (no application password)

The client uses a previously trusted SSH host and public-key login; the SSH session runs a one-shot CLI command through a permission-limited local Unix socket:

```bash
ssh -T -o BatchMode=yes -o StrictHostKeyChecking=yes user@server \
  serverside-generative handshake --stdio
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
./serverside-generative serve --human --http-listen off
# Any time afterwards, print a fresh 2-minute invite:
./serverside-generative pair
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

This test transfers RAM-only context, starts the test `echo` job, **disconnects immediately**, lets it complete on the server, reconnects, replays events, then ACKs them and confirms that they are removed from RAM.

The test client requires **Python 3.10+ standard library only**. To run the full end-to-end suite:

```bash
go test ./...
python3 -m unittest discover -s testclient -p test_integration.py -v
```

## Framed data protocol v1

After TLS certificate fingerprint validation, send length-prefixed JSON:

```json
{"type":"auth","ticket":"ONE_TIME_TICKET"}
```

Server replies `{"type":"paired","client_id":"...","secret":"...","protocol":1}`. Persist the ID and secret on the Android client securely. Reconnect with `{"type":"auth","client_id":"...","secret":"..."}`; no new ticket is required.

Additional messages:

| Client message | Server message | Purpose |
| --- | --- | --- |
| `handoff {session_id, revision, state}` | `handoff_ack` | Replace in-memory state with strictly increasing revision |
| `start {session_id, request_id, kind:"echo", content, delay_ms}` | `start_ack` | Test-only async job, idempotent by request ID |
| `sync {session_id, since}` | `sync_begin`, `event*`, `sync_end` | Replay events then continuously subscribe |
| `ack {session_id, seq}` | `ack_ok` | Release events **after** local durable write |
| `status {session_id}` | `status` | Inspect active in-RAM job/event counts |
| `delete {session_id}` | `deleted` | Clear RAM session and cancel jobs |
| `revoke` | `revoked` | Delete current client's reconnect credential |

Each payload is prefixed by a **4-byte unsigned big-endian byte length**. Credentials must never be logged. Client identity scopes each session, preventing cross-client access to the same session ID.

## Limitations / next steps

- This MVP has no real model/API/MCP executor. Only the simulated `echo` kind proves handoff and RAM replay.
- TCP hole punching and address discovery remain separate work. Neither HTTP, SSH nor QR signaling alone removes the requirement for a reachable data endpoint.
- No persistent event recovery after server restart. A client must resend the context from its own database.
- Current in-memory limits are 128 sessions, 2 MiB state per session and 2,048 retained events per session; overflow yields `sync_gap`.
- A real Android client should persist reconnect secrets in Android Keystore-backed storage, and release Temporary Chats without handing them off.
- Credential revocation should be enforced across already-active connections as this project evolves.
