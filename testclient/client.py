#!/usr/bin/env python3
"""Dependency-free Python interoperability client for serverside-generative.

Usage:
  python3 testclient/client.py pair --method http --url http://127.0.0.1:7091 --password 'long-secret' --state /tmp/client.json
  python3 testclient/client.py pair --method ssh --ssh-command './serverside handshake --stdio' --state /tmp/client.json
  python3 testclient/client.py pair --method human --invite 'generative://pair?...' --state /tmp/client.json
  python3 testclient/client.py demo --state /tmp/client.json --session test-1 --text hello --delay-ms 350
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import secrets
import shlex
import socket
import ssl
import struct
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

FRAME_LIMIT = 4 * 1024 * 1024


def secure_socket(endpoint: str, fingerprint: str, timeout: float = 8.0) -> ssl.SSLSocket:
    if len(fingerprint) != 64 or any(c not in "0123456789abcdefABCDEF" for c in fingerprint):
        raise ValueError("Expected SHA-256 certificate fingerprint as 64 hex digits")
    host, port_str = endpoint.rsplit(":", 1)
    host = host.removeprefix("[").removesuffix("]")
    conn = socket.create_connection((host, int(port_str)), timeout=timeout)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    context.check_hostname = False
    context.verify_mode = ssl.CERT_NONE
    context.minimum_version = ssl.TLSVersion.TLSv1_3
    wrapped = None
    try:
        wrapped = context.wrap_socket(conn, server_hostname=host)
        got = hashlib.sha256(wrapped.getpeercert(binary_form=True)).hexdigest()
        if not secrets.compare_digest(got, fingerprint.lower()):
            raise ValueError("TLS identity mismatch; refusing to send credentials")
        wrapped.settimeout(timeout)
        return wrapped
    except BaseException:
        if wrapped is not None:
            wrapped.close()
        else:
            conn.close()
        raise


def write_frame(sock: socket.socket, message: dict[str, Any]) -> None:
    data = json.dumps(message, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    if len(data) > FRAME_LIMIT:
        raise ValueError("Frame too large")
    sock.sendall(struct.pack("!I", len(data)) + data)


def recv_exact(sock: socket.socket, count: int) -> bytes:
    chunks: list[bytes] = []
    while count:
        piece = sock.recv(count)
        if not piece:
            raise EOFError("TCP connection closed")
        chunks.append(piece)
        count -= len(piece)
    return b"".join(chunks)


def read_frame(sock: socket.socket) -> dict[str, Any]:
    (size,) = struct.unpack("!I", recv_exact(sock, 4))
    if not (0 < size <= FRAME_LIMIT):
        raise ValueError("Invalid frame size")
    return json.loads(recv_exact(sock, size))


def context_snapshot(creds: dict[str, Any] | None = None, override: dict[str, Any] | None = None) -> dict[str, Any] | None:
    if override is not None:
        return override
    if creds and creds.get("context_file"):
        return json.loads(Path(creds["context_file"]).read_text(encoding="utf-8"))
    return None


def context_digest(config: dict[str, Any]) -> str:
    # Match Go's field order and omitempty conventions for the typed, nonsecret schema.
    skills = config.get("skills")
    normalized: dict[str, Any] = {
        "instructions": config.get("instructions") or "",
        "skills": None if skills is None else [
            {
                "id": item["id"], "name": item["name"],
                **({"description": item["description"]} if item.get("description") else {}),
                "content": item.get("content") or "",
            } for item in skills
        ],
    }
    providers = config.get("providers") or []
    if providers:
        normalized["providers"] = [
            {
                "id": item["id"], "name": item["name"],
                "platform": item["platform"],
                **({"base_url": item["base_url"]} if item.get("base_url") else {}),
                **({"models": item["models"]} if item.get("models") else {}),
            } for item in providers
        ]
    raw = json.dumps(normalized, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    # encoding/json escapes HTML special characters in string literals.
    raw = raw.replace(b"<", b"\\u003c").replace(b">", b"\\u003e").replace(b"&", b"\\u0026")
    return hashlib.sha256(raw).hexdigest()


def update_client_sync_state(
    creds: dict[str, Any] | None, revision: int, digest: str,
    local_digest: str | None,
) -> None:
    if creds is None:
        return
    creds["last_shared_revision"] = revision
    creds["last_synced_digest"] = digest
    creds["last_local_digest"] = local_digest
    path = creds.get("_state_path")
    if path:
        save_state(Path(path), {k: v for k, v in creds.items() if not k.startswith("_")})


def auth_socket(
    invite: dict[str, Any],
    creds: dict[str, Any] | None = None,
    context_override: dict[str, Any] | None = None,
) -> tuple[ssl.SSLSocket, dict[str, Any]]:
    sock = secure_socket(invite["endpoint"], invite["fingerprint"])
    if creds:
        auth = {"type": "auth", "client_id": creds["client_id"], "secret": creds["secret"]}
    else:
        auth = {"type": "auth"}
        if invite.get("ticket"):
            auth["ticket"] = invite["ticket"]
        elif invite.get("pin"):
            auth["pin"] = invite["pin"]
        else:
            raise ValueError("Missing ticket or manual PIN")
    try:
        write_frame(sock, auth)
        result = read_frame(sock)
        if result.get("type") != "paired":
            raise RuntimeError(f"Pairing failed: {result}")
        if result.get("protocol") != 4:
            raise RuntimeError(f"Unsupported server protocol: {result.get('protocol')}")
        local = context_snapshot(creds, context_override)
        local_hash = context_digest(local) if local is not None else None
        server_revision = result["shared_revision"]
        server_digest = result["context_digest"]
        last_server = (creds or {}).get("last_synced_digest")
        last_local = (creds or {}).get("last_local_digest")
        if local is None:
            # Read shared state without modifying it.
            write_frame(sock, {"type": "context_get"})
            ack = read_frame(sock)
        elif local_hash == server_digest:
            write_frame(sock, {"type": "context_sync",
                               "revision": server_revision, "digest": server_digest})
            ack = read_frame(sock)
        elif server_revision == 0 or (last_server == server_digest and last_local != local_hash):
            # Local edits based on the server's last known state.
            write_frame(sock, {"type": "context_update",
                               "revision": server_revision, "context": local})
            ack = read_frame(sock)
        elif last_local == local_hash:
            # Server changed, but local file did not. Adopt remote state.
            write_frame(sock, {"type": "context_get"})
            ack = read_frame(sock)
            if ack.get("type") == "context_state" and creds and creds.get("context_file"):
                save_state(Path(creds["context_file"]), ack["context"])
                local_hash = context_digest(ack["context"])
        else:
            raise RuntimeError(
                "Shared configuration conflict: both server and local file changed; "
                "resolve explicitly rather than overwriting either copy"
            )
        if ack.get("type") not in ("context_ack", "context_state"):
            raise RuntimeError(f"Shared configuration sync failed: {ack}")
        revision = ack.get("revision", server_revision)
        digest = ack.get("digest", server_digest)
        if ack.get("type") == "context_state":
            result["shared_context"] = ack["context"]
            if local is None:
                local_hash = None
        update_client_sync_state(creds, revision, digest, local_hash)
        result["shared_revision"] = revision
        result["context_digest"] = digest
        result["last_local_digest"] = local_hash
        return sock, result
    except BaseException:
        sock.close()
        raise

def _is_loopback(url: str) -> bool:
    host = (urllib.parse.urlparse(url).hostname or "").lower()
    return host in ("localhost", "127.0.0.1", "::1")


class WebSocketUnsupported(Exception):
    pass


def _http_ws_handshake(url: str, password: str) -> dict[str, Any]:
    target = urllib.parse.urlparse(url)
    if target.scheme not in ("http", "https"):
        raise ValueError("Expected http:// or https:// URL")
    secure = target.scheme == "https"
    host = target.hostname
    if not host:
        raise ValueError("Missing host")
    port = target.port or (443 if secure else 80)
    prefix = target.path.rstrip("/")
    path = (prefix or "") + "/v1/handshake/ws"
    key = base64.b64encode(os.urandom(16)).decode("ascii")
    raw = socket.create_connection((host, port), timeout=7)
    try:
        if secure:
            raw = ssl.create_default_context().wrap_socket(raw, server_hostname=host)
        request = (
            f"GET {path} HTTP/1.1\r\nHost: {target.netloc}\r\n"
            f"Upgrade: websocket\r\nConnection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n"
        )
        raw.sendall(request.encode("ascii"))
        head = bytearray()
        while b"\r\n\r\n" not in head:
            if len(head) > 8192:
                raise RuntimeError("WebSocket response header too large")
            chunk = raw.recv(1024)
            if not chunk:
                raise WebSocketUnsupported("No WS response")
            head.extend(chunk)
        split = head.index(b"\r\n\r\n") + 4
        header, extra = bytes(head[:split]), bytes(head[split:])
        status = int(header.split(b" ", 2)[1])
        if status in (404, 405, 426, 501):
            raise WebSocketUnsupported(f"WebSocket unavailable: HTTP {status}")
        if status != 101:
            raise RuntimeError(f"WebSocket rejected: HTTP {status}")
        lower = header.lower()
        if b"upgrade: websocket" not in lower:
            raise RuntimeError("Invalid WebSocket upgrade")
        # RFC 6455 requires the server to prove it knows the one-time key.
        accept = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest())
        if b"sec-websocket-accept: " + accept.lower() not in lower:
            raise RuntimeError("Invalid WebSocket accept")
        payload = json.dumps({"password": password}).encode()
        mask = os.urandom(4)
        length = len(payload)
        if length <= 125:
            frame = bytes([0x81, 0x80 | length])
        elif length <= 65535:
            frame = bytes([0x81, 0x80 | 126]) + struct.pack("!H", length)
        else:
            raise ValueError("Password too long")
        raw.sendall(frame + mask + bytes(byte ^ mask[i % 4] for i, byte in enumerate(payload)))

        buffer = bytearray(extra)

        def get(count: int) -> bytes:
            while len(buffer) < count:
                chunk = raw.recv(4096)
                if not chunk:
                    raise EOFError("WS closed")
                buffer.extend(chunk)
            result = bytes(buffer[:count])
            del buffer[:count]
            return result

        first, second = get(2)
        op = first & 15
        if op not in (1, 2):
            raise RuntimeError(f"WS unexpected opcode {op}")
        size = second & 127
        if size == 126:
            size = struct.unpack("!H", get(2))[0]
        elif size == 127:
            size = struct.unpack("!Q", get(8))[0]
        if size > 4096:
            raise RuntimeError("WS payload too large")
        payload_bytes = get(size)
        if second & 0x80:
            masking = get(4)
            payload_bytes = bytes(byte ^ masking[i % 4] for i, byte in enumerate(payload_bytes))
        response = json.loads(payload_bytes)
        if "error" in response:
            raise PermissionError(response["error"])
        return response
    finally:
        raw.close()


def _http_post_handshake(url: str, password: str) -> dict[str, Any]:
    dest = url.rstrip("/") + "/v1/handshake"
    request = urllib.request.Request(
        dest,
        data=json.dumps({"password": password}).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=8) as response:
        return json.load(response)


def handshake_http(url: str, password: str, *, allow_insecure: bool = False) -> tuple[dict[str, Any], str]:
    if not url.startswith(("https://", "http://")):
        raise ValueError("HTTP handshake requires URL starting with http or https")
    if url.startswith("http://") and not (_is_loopback(url) or allow_insecure):
        raise ValueError("Refusing password over plaintext Internet: use HTTPS or explicitly --insecure-http")
    try:
        return _http_ws_handshake(url, password), "websocket"
    except PermissionError:
        raise
    except (WebSocketUnsupported, ConnectionError, TimeoutError, OSError):
        # Only fail over for transport/unavailable endpoint errors, not bad credentials.
        return _http_post_handshake(url, password), "http"


def handshake_ssh(command: str) -> dict[str, Any]:
    # Prefer ssh -T -o BatchMode=yes -o StrictHostKeyChecking=yes USER@HOST ...
    argv = shlex.split(command)
    if not argv:
        raise ValueError("Missing SSH command")
    result = subprocess.run(argv, input="", text=True, capture_output=True, timeout=20, check=True)
    return json.loads(result.stdout)


def handshake_human(invite_uri: str | None, endpoint: str | None, fingerprint: str | None, pin: str | None) -> dict[str, Any]:
    if invite_uri:
        parsed = urllib.parse.urlparse(invite_uri)
        if parsed.scheme != "generative" or parsed.netloc != "pair":
            raise ValueError("Invalid human-relay QR URI")
        query = urllib.parse.parse_qs(parsed.query, strict_parsing=True)
        return {"endpoint": query["endpoint"][0], "ticket": query["ticket"][0], "fingerprint": query["fingerprint"][0]}
    if not (endpoint and fingerprint and pin):
        raise ValueError("Manual mode requires --endpoint, --fingerprint and --pin")
    return {"endpoint": endpoint, "fingerprint": fingerprint, "pin": pin}


def save_state(path: Path, state: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    # Atomic, restrictive output. This file contains the persistent reconnect secret.
    tmp = path.with_name(path.name + ".tmp." + secrets.token_hex(4))
    try:
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as handle:
            json.dump(state, handle, indent=2)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp, path)
    finally:
        tmp.unlink(missing_ok=True)


def pair(invitation: dict[str, Any], path: Path, context_file: Path | None = None) -> dict[str, Any]:
    if invitation.get("expires") and invitation["expires"] <= time.time():
        raise ValueError("Pairing invitation expired")
    bootstrap = {"context_file": str(context_file.resolve())} if context_file is not None else {}
    sock, result = auth_socket(invitation, context_override=context_snapshot(bootstrap))
    sock.close()
    if "secret" not in result:
        raise RuntimeError("Server did not return initial reconnect credentials")
    state = {
        "endpoint": invitation["endpoint"],
        "fingerprint": invitation["fingerprint"],
        "client_id": result["client_id"],
        "secret": result["secret"],
        "last_shared_revision": result["shared_revision"],
        "last_synced_digest": result["context_digest"],
        "last_local_digest": result.get("last_local_digest"),
    }
    if context_file is not None:
        state["context_file"] = str(context_file.resolve())
    save_state(path, state)
    return state


def reconnect(state: dict[str, Any], context_override: dict[str, Any] | None = None) -> ssl.SSLSocket:
    sock, _ = auth_socket(state, state, context_override)
    return sock


def demo(state: dict[str, Any], session_id: str, text: str, delay_ms: int = 350) -> dict[str, Any]:
    sock = reconnect(state)
    try:
        write_frame(sock, {"type": "handoff", "session_id": session_id, "revision": 1,
                           "state": {"model": "test-only", "api": {"enabled": False}, "mcp": []}})
        handoff = read_frame(sock)
        if handoff.get("type") != "handoff_ack":
            raise RuntimeError(f"handoff rejected: {handoff}")
        write_frame(sock, {"type": "start", "session_id": session_id,
                           "request_id": "job-" + session_id, "kind": "echo",
                           "content": text, "delay_ms": delay_ms})
        started = read_frame(sock)
        if started.get("type") != "start_ack":
            raise RuntimeError(f"start rejected: {started}")
    finally:
        # Simulate Android being closed: the server must continue the job in RAM.
        sock.close()
    time.sleep((delay_ms / 1000.0) + 0.6)
    sock = reconnect(state)
    events: list[dict[str, Any]] = []
    try:
        write_frame(sock, {"type": "sync", "session_id": session_id, "since": 0})
        begin = read_frame(sock)
        if begin.get("type") != "sync_begin":
            raise RuntimeError(f"failed to synchronize: {begin}")
        while True:
            item = read_frame(sock)
            if item.get("type") == "sync_end":
                break
            if item.get("type") == "event":
                events.append(item)
            else:
                raise RuntimeError(f"Unexpected message: {item}")
        content = "".join(item.get("content", "") for item in events if item["event"] == "delta")
        if content != text or not any(item["event"] == "completed" for item in events):
            raise AssertionError(f"Wrong replay: {events}")
        seq = max((item["seq"] for item in events), default=0)
        # In the Android app this ACK should occur only after local DB commit.
        write_frame(sock, {"type": "ack", "session_id": session_id, "seq": seq})
        ack = read_frame(sock)
        if ack.get("type") != "ack_ok":
            raise RuntimeError(f"ACK failed: {ack}")
        write_frame(sock, {"type": "status", "session_id": session_id})
        status = read_frame(sock)
        if status.get("pending_events") != 0:
            raise AssertionError(f"ACK did not clear server event buffer: {status}")
        return {"session_id": session_id, "replayed_events": len(events), "content": content, "status": status}
    finally:
        sock.close()



def run_remote(
    auth: dict[str, Any],
    config: dict[str, Any],
    session_id: str,
    kind: str,
    mcp_server: str = "",
    mcp_tool: str = "",
    arguments: dict[str, Any] | None = None,
    timeout: int = 120,
) -> dict[str, Any]:
    """Start real remote execution, close Android connection, then reconnect to resync.

    This standalone smoke-test client prints output but deliberately does not ACK:
    a real Android client ACKs only after the event is committed to its local DB.
    """
    request_id = "job-" + secrets.token_hex(8)
    sock = reconnect(auth)
    try:
        write_frame(sock, {"type": "handoff", "session_id": session_id,
                           "revision": time.time_ns(), "state": config})
        reply = read_frame(sock)
        if reply.get("type") != "handoff_ack":
            raise RuntimeError(f"handoff rejected: {reply}")
        if kind == "model":
            request = {"type": "start", "kind": "model"}
        elif kind == "mcp-list":
            request = {"type": "mcp_list", "mcp_server": mcp_server}
        elif kind == "mcp-call":
            request = {"type": "mcp_call", "mcp_server": mcp_server,
                       "mcp_tool": mcp_tool, "arguments": arguments or {}}
        else:
            raise ValueError("unsupported run mode")
        request.update({"session_id": session_id, "request_id": request_id})
        write_frame(sock, request)
        reply = read_frame(sock)
        if reply.get("type") != "start_ack":
            raise RuntimeError(f"remote job rejected: {reply}")
    finally:
        sock.close()

    # Server continues after the first connection is closed.
    sock = reconnect(auth)
    sock.settimeout(timeout)
    seen: list[dict[str, Any]] = []
    try:
        write_frame(sock, {"type": "sync", "session_id": session_id, "since": 0})
        reply = read_frame(sock)
        if reply.get("type") != "sync_begin":
            raise RuntimeError(f"sync failed: {reply}")
        while True:
            item = read_frame(sock)
            if item.get("type") == "event" and item.get("request_id") == request_id:
                seen.append(item)
                if item.get("event") in ("completed", "error"):
                    break
            elif item.get("type") == "error":
                raise RuntimeError(f"sync failed: {item.get('message')}")
        if seen[-1]["event"] != "completed":
            raise RuntimeError("remote execution failed; provider errors are not echoed to protect credentials")
        return {"session_id": session_id, "events": len(seen),
                "text": "".join(e.get("content", "") for e in seen if e.get("event") == "delta"),
                "tools": [json.loads(e["content"]) for e in seen if e.get("event") == "mcp_tools"],
                "tool_results": [json.loads(e["content"]) for e in seen if e.get("event") == "mcp_result"]}
    finally:
        sock.close()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    subs = parser.add_subparsers(dest="command", required=True)
    p = subs.add_parser("pair")
    p.add_argument("--method", choices=("http", "ssh", "human"), required=True)
    p.add_argument("--state", type=Path, default=Path("testclient-state.json"))
    p.add_argument("--url")
    p.add_argument("--password")
    p.add_argument("--insecure-http", action="store_true")
    p.add_argument("--ssh-target")
    p.add_argument("--ssh-command", help="Override for integration tests; with SSH use BatchMode and strict host verification")
    p.add_argument("--invite")
    p.add_argument("--endpoint")
    p.add_argument("--fingerprint")
    p.add_argument("--pin")
    p.add_argument("--context-file", type=Path, help="Non-secret instructions/Agent Skills JSON to upload on each reconnect")
    d = subs.add_parser("demo")
    d.add_argument("--state", type=Path, default=Path("testclient-state.json"))
    d.add_argument("--session", default="demo-1")
    d.add_argument("--text", default="Hello from the memory-only runtime.")
    d.add_argument("--delay-ms", type=int, default=350)
    work = subs.add_parser("run", help="Run real provider/MCP SDK calls from a JSON session config")
    work.add_argument("--state", type=Path, default=Path("testclient-state.json"))
    work.add_argument("--config", type=Path, required=True, help="Session JSON containing provider/API/MCP settings")
    work.add_argument("--session", default="api-test-session")
    work.add_argument("--kind", choices=("model", "mcp-list", "mcp-call"), default="model")
    work.add_argument("--mcp-server", default="")
    work.add_argument("--mcp-tool", default="")
    work.add_argument("--arguments-file", type=Path)
    work.add_argument("--timeout", type=int, default=120)
    args = parser.parse_args()
    if args.command == "pair":
        if args.method == "http":
            if not args.url or not args.password:
                parser.error("HTTP mode needs --url and --password")
            invite, channel = handshake_http(args.url, args.password, allow_insecure=args.insecure_http)
        elif args.method == "ssh":
            if args.ssh_target:
                cmd = f"ssh -T -o BatchMode=yes -o StrictHostKeyChecking=yes {shlex.quote(args.ssh_target)} serverside-generative handshake --stdio"
            else:
                cmd = args.ssh_command or ""
            invite = handshake_ssh(cmd)
            channel = "ssh"
        else:
            invite = handshake_human(args.invite, args.endpoint, args.fingerprint, args.pin)
            channel = "human"
        state = pair(invite, args.state, args.context_file)
        print(json.dumps({"paired": True, "method": channel, "endpoint": state["endpoint"],
                          "tls_fingerprint": state["fingerprint"], "state_file": str(args.state)}))
    elif args.command == "demo":
        state = json.loads(args.state.read_text())
        state["_state_path"] = str(args.state)
        print(json.dumps(demo(state, args.session, args.text, args.delay_ms), ensure_ascii=False))
    else:
        auth = json.loads(args.state.read_text())
        auth["_state_path"] = str(args.state)
        config = json.loads(args.config.read_text())
        arguments = json.loads(args.arguments_file.read_text()) if args.arguments_file else {}
        print(json.dumps(run_remote(auth, config, args.session, args.kind, args.mcp_server,
                                    args.mcp_tool, arguments, args.timeout), ensure_ascii=False))


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, ValueError, KeyError, urllib.error.HTTPError, subprocess.CalledProcessError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        sys.exit(1)