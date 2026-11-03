# Structured coding-agent sessions

The `agent_*` transport API is a **paired-client API**, independent of the
existing provider SDK execution runtime, chat sessions, MCP calls and terminal
tools. No `agent_*` operation is advertised by `tools_list`. It does not
turn an agent CLI into a model-callable server tool.

Enable it explicitly:

```sh
serverside serve --enable-agent --agent-root /srv/projects --http-listen off
```

The daemon uses the existing authenticated TLS data channel and context-sync
handshake, but owns separate `agent.Manager` and `agent.Session` objects.
It never calls `Session.Handoff`, `Session.Start`, or a provider SDK for
agent traffic. Every paired client on this **personal** server can control
the same agent sessions. Do not pair clients you do not trust.

## Choosing the executable

Only `agent.type` is mandatory on creation:

```json
{"type":"agent_open","agent":{"type":"codex"}}
```

Defaults: `claude`, `codex`, `opencode` in the server's `PATH`.

For a fork or renamed binary, pass an optional executable name in PATH or
absolute path, **without a shell command or arguments**:

```json
{"type":"agent_open","agent":{"type":"claude","binary":"claude-fork","cwd":"my-project"}}
```

`cwd` defaults to the configured agent root. Explicit relative `cwd` is
resolved beneath that root; absolute `cwd` must also be underneath it.
Symlinks are resolved before the boundary check. This restricts *initial
working directory selection*, not the filesystem operations of the agent:
the process can still access paths permitted to the daemon OS account.
Use a dedicated unprivileged OS user, or a real sandbox/container for
stronger isolation.

## Request/response API

Every request is a length-prefixed JSON frame on the already-authenticated
TLS connection, after normal context synchronization. `agent_open` replies
`agent_opened {session}` containing an opaque `agent_session_id`.

| Request | Response | Meaning |
| --- | --- | --- |
| `agent_open {agent:{type,binary?,cwd?}}` | `agent_opened {session}` | Start direct native protocol session |
| `agent_send {agent_session_id,prompt}` | `agent_send_ack` | Start next turn using the native agent protocol |
| `agent_steer {agent_session_id,prompt}` | `agent_steer_ack` or error | Queue/steer an active turn when supported |
| `agent_events {agent_session_id,since,wait_ms?}` | `agent_events {events,last_seq}` | Poll structured events after cursor |
| `agent_respond {agent_session_id,approval_id,decision}` | `agent_respond_ack` | Respond to a native approval request |
| `agent_interrupt {agent_session_id}` | `agent_interrupt_ack` | Interrupt turn, retain session |
| `agent_status {agent_session_id}` | `agent_status {session}` | Status, busy/running and last sequence |
| `agent_list` | `agent_list {sessions,max_sessions}` | Enumerate agent sessions |
| `agent_close {agent_session_id}` | `agent_closed` | Stop native child process |

Responses for `agent_respond` take a value from `accept`, `decline`,
`cancel`, `acceptForSession`, `once`, `always`, `reject`; the native
adapter maps supported decisions to the CLI API. Only answer an approval
that the user explicitly approved. Do not auto-approve requests.

The `agent_events.events` list is ordered by `seq`. Each item contains
`kind` (`content`, `thinking`, `tool_call`, `tool_result`, `approval`,
`status`, `error`, `completed`) and, when provided, `block_id`,
`tool_call_id`, `approval_id`, `content`, `status`, and **raw** source
JSON. The original payload is deliberately retained so the client can
handle vendor-specific items and schema changes without lossy conversion.
Thinking text is not fabricated: only actual protocol disclosures appear.

`since` is an in-memory sequence cursor, not a terminal byte offset.
Persist the last handled sequence on the client. The server retains a
bounded 2 MiB / 2048-event buffer per agent session; after eviction the
client receives a `sync_gap` error rather than silently missing output.
A disconnect does not terminate the CLI, and another paired client may
resume from its saved cursor. A daemon restart does terminate sessions.
Sessions and event buffers are RAM-only from serverside's perspective;
the external CLIs can maintain their own user-config/history on disk.

## Native adapters

- Claude Code: bidirectional `--input-format stream-json`,
  `--output-format stream-json`, partial message stream, control messages.
- Codex: `codex app-server` JSON-RPC initialize/thread/turn lifecycle,
  streaming item notifications and native approval reply IDs.
- OpenCode: private `127.0.0.1` HTTP API with a random per-process
  password, with runtime detection of legacy endpoints (`/session`,
  `/prompt_async`, `/abort`, `/event`) or OpenCode v2 endpoints
  (`/api/session`, `/api/session/{id}/prompt`, `/interrupt`, `/api/event`).
  OpenCode v2 events use the native `{type,data}` SSE payload, which is
  preserved in `raw`. The v2 prompt API supports `queue` and `steer`.

The optional binary is assumed to speak the **same native protocol** as
its selected type. Unknown or incompatible forks cause a startup/protocol
error. An active running CLI may have separate local credentials, model
configuration or user consent prompts; those are managed by that CLI,
not stored in serverside's provider handoff state.

## Python test client

After pairing using `testclient/client.py`:

```sh
python3 testclient/agent_client.py open --type codex --state client-state.json
python3 testclient/agent_client.py send --agent-session-id ID --prompt 'Explain the repository' --state client-state.json
python3 testclient/agent_client.py events --agent-session-id ID --since 0 --state client-state.json
python3 testclient/agent_client.py interrupt --agent-session-id ID --state client-state.json
python3 testclient/agent_client.py close --agent-session-id ID --state client-state.json
```

Start the next poll at the last processed `seq` and render updates by
`block_id` rather than concatenating completed snapshots with deltas.
A fork can be selected using `open --type codex --binary /path/to/fork`.
