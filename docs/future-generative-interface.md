# Future Generative ↔ ServerSide interface concepts

**Design-only; no modifications to `~/generative`.** These diagrams describe a possible future opt-in client interface to the [ServerSide v5 API](client-api-surface.md), not Android UI, Kotlin, Room or Gradle implementation tasks.

## Conceptual onboarding and session selection

```mermaid
flowchart TD
    Start["Hypothetical opt-in ServerSide entry"]
    Cap["GET /v1/capabilities"]
    Mode{"Transport"}
    URL["HTTP / WebSocket<br/>Full HTTPS pairing URL<br/>or identical QR"]
    SSH["TCP / SSH<br/>SSH-only pairing"]
    Pair["Verify trust and store<br/>paired identity securely"]
    Chats["List durable server chats<br/>session.list"]
    Open["session.open<br/>no storage/sync flag"]
    Run["session.run + events"]
    Start --> Cap --> Mode
    Mode -->|HTTP/WS| URL
    Mode -->|TCP/SSH| SSH
    URL --> Pair
    SSH --> Pair
    Pair --> Chats --> Open --> Run
```

QR represents the same complete HTTPS URL as copyable text. Whether the **app** itself syncs or stores a chat locally has no effect on how the ServerSide daemon stores conversations: **every server chat is a durable JSON file**.

## Boundary between app policy and server data

```mermaid
flowchart LR
    App["Existing Generative UI & local chat"]
    Local[("Existing local persistence")]
    Future["Possible future opt-in remote workspace"]
    Server["ServerSide v5"]
    JSON[("Server permanent chat JSON")]
    RAM["Mobile-supplied provider/MCP settings<br/>server RAM only"]
    App --> Local
    Future --> Server
    Server --> JSON
    Server --> RAM
    App -. "No automatic rerouting<br/>or local data migration" .- Future
```

| Topic | Server behavior | Future app decision |
| --- | --- | --- |
| Conversation storage | **Always persisted** in private JSON with revision | Whether/how the app separately stores or syncs its own copy |
| Server-delivered updates | `conversation` snapshot events, deltas and `session.get` | How a future client presents or acknowledges them |
| Provider/MCP from mobile | Session-scoped RAM; lost after server restart | Which config or credentials to explicitly supply |
| Local server provider config | YAML catalog + encrypted SQLite credentials | Whether to select an existing server provider |
| Pairing | SSH-only TCP or URL/QR HTTP/WS | User interface and secure identity storage (not implemented here) |

**No automatic local chat upload, Room rewrite or switch of existing app model requests** is specified. The Go daemon never reads a mobile app's local sync setting.

## Session lifecycle concept

```mermaid
stateDiagram-v2
    [*] --> Disconnected
    Disconnected --> Paired: Explicit pairing
    Paired --> Connected: Authenticated transport
    Connected --> Open: session.open (durable server chat)
    Open --> Ready: runtime configured (if execution required)
    Ready --> Running: session.run
    Running --> Ready: durable final snapshot / completed event
    Running --> Reconnecting: connection lost
    Reconnecting --> Open: session.open or session.get restores server JSON
    Open --> Ready: re-send ephemeral runtime after restart
    Ready --> Closed: session.close (JSON remains)
    Closed --> [*]
```

The chat survives a process restart and a lost network connection. Transient credentials do **not** survive; execution requires fresh runtime settings. On uncertain external model/tool calls, inspect recorded status instead of replaying the request.

## Proposed information groups, not actual screens

```mermaid
flowchart TD
    API["Server API overview"]
    Identity["Paired connection + trust"]
    Catalogue["Server-side providers/models/MCP metadata"]
    Conversations["Durable chat list + session.open/get"]
    Live["Run and streamed events"]
    Tools["Granted tool list/calls"]
    API --> Identity
    API --> Catalogue
    API --> Conversations
    Conversations --> Live
    Conversations --> Tools
```

This conceptual hierarchy intentionally avoids prescribing Compose widgets, Android navigation, a local Room schema, developer-feature hooks or a production release. Those decisions require a separate, explicit request.

**Current scope exclusion:** do not modify `~/generative` anywhere, including `extensions/` or Developer features, and do not build/test Android. All current executable work belongs only to `~/serverside-generative`.
