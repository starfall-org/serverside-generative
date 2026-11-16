"""Interrupted v5 transport recovery against the server subprocess."""
import base64
import hashlib
import http.server
import json
import os
from pathlib import Path
import socket
import struct
import subprocess
import tempfile
import threading
import time
import unittest
from urllib.parse import urlsplit

import client
from test_integration import BINARY, PASSWORD, ServerRunner
from test_v5_security import request


class WS:
    def __init__(self, url):
        target = urlsplit(url)
        self.sock = socket.create_connection((target.hostname, target.port), timeout=8)
        key = base64.b64encode(os.urandom(16)).decode()
        self.sock.sendall((f"GET /v1/ws HTTP/1.1\r\nHost: {target.netloc}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n").encode())
        header = b""
        while not header.endswith(b"\r\n\r\n"):
            header += self.exact(1)
        expected = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest())
        if b" 101 " not in header or expected not in header:
            raise AssertionError(header)

    def exact(self, size):
        data = b""
        while len(data) < size:
            part = self.sock.recv(size - len(data))
            if not part:
                raise EOFError("WebSocket closed")
            data += part
        return data

    def send(self, value):
        payload = json.dumps(value).encode()
        mask = os.urandom(4)
        size = len(payload)
        prefix = bytes((0x81, 0x80 | size)) if size < 126 else bytes((0x81, 0xfe)) + struct.pack(">H", size)
        self.sock.sendall(prefix + mask + bytes(byte ^ mask[i % 4] for i, byte in enumerate(payload)))

    def read(self):
        first, second = self.exact(2)
        if first & 15 != 1 or second & 128:
            raise AssertionError((first, second))
        size = second & 127
        if size == 126:
            size = struct.unpack(">H", self.exact(2))[0]
        elif size == 127:
            size = struct.unpack(">Q", self.exact(8))[0]
        return json.loads(self.exact(size))

    def close(self):
        self.sock.close()


class ResumeIntegration(unittest.TestCase):
    def test_interrupted_websocket_then_http(self):
        release = threading.Event()
        calls = []
        class Provider(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass
            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", "0")))
                calls.append(1)
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                def chunk(text):
                    event = {"id":"x", "object":"chat.completion.chunk", "choices":[{"index":0,"delta":{"content":text}}]}
                    self.wfile.write(("data: " + json.dumps(event) + "\n\n").encode())
                    self.wfile.flush()
                chunk("first streaming response segment")
                if release.wait(8):
                    chunk(" second segment")
                    self.wfile.write(b"data: [DONE]\n\n")
                    self.wfile.flush()
        upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Provider)
        thread = threading.Thread(target=upstream.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory(prefix="ssg-resume-") as temp:
                server = ServerRunner(Path(temp) / "identity", allow_local_api=True)
                server.start()
                try:
                    invitation, _ = client.handshake_http(server.url, PASSWORD)
                    pair = client.pair(invitation, Path(temp) / "pair.json")
                    auth = {"client_id": pair["client_id"], "secret": pair["secret"]}
                    status, result = request(server.url + "/v1/auth", auth)
                    self.assertEqual(status, 200)
                    token = result["token"]
                    ws = WS(server.url)
                    try:
                        ws.send({"type":"auth", "protocol":5, **auth})
                        self.assertEqual(ws.read()["type"], "paired")
                        conversation = {"id":"resume", "title":"x", "created_at":1, "updated_at":2, "messages":[]}
                        runtime = {"source":"mobile_ephemeral", "provider":{"type":"openai", "baseUrl":f"http://127.0.0.1:{upstream.server_port}", "modelId":"test", "auth":{"method":"bearer_token", "value":"secret"}}}
                        ws.send({"id":"open", "op":"session.open", "session_id":"resume", "data":{"storage":"memory", "conversation":conversation, "runtime":runtime}})
                        self.assertTrue(ws.read()["ok"])
                        run = {"id":"run", "op":"session.run", "session_id":"resume", "data":{"content":"hello"}}
                        ws.send(run)
                        while True:
                            event = ws.read()
                            if event.get("kind") == "delta":
                                cursor = event["seq"]
                                break
                    finally:
                        ws.close()
                    status, result = request(server.url + "/v1/rpc", run, token)
                    self.assertEqual(status, 200)
                    self.assertTrue(result["ok"])
                    changed = {**run, "data": {"content": "different input"}}
                    status, result = request(server.url + "/v1/rpc", changed, token)
                    self.assertEqual(status, 409)
                    self.assertEqual(result["error"]["code"], "request_already_recorded")
                    status, result = request(f"{server.url}/v1/events?session_id=resume&after=999999", token=token)
                    self.assertEqual(status, 409)
                    self.assertEqual(result["error"]["code"], "event_gap")
                    release.set()
                    status, page = request(f"{server.url}/v1/events?session_id=resume&after={cursor}", token=token)
                    self.assertEqual(status, 200)
                    self.assertTrue(page["events"])
                    event = page["events"][0]
                    self.assertGreater(event["seq"], cursor)
                    self.assertEqual(event["kind"], "delta")
                    cursor = event["seq"]
                    ws = WS(server.url)
                    try:
                        ws.send({"type":"auth", "protocol":5, **auth})
                        self.assertEqual(ws.read()["type"], "paired")
                        ws.send({"id":"reopen", "op":"session.open", "session_id":"resume", "data":{"storage":"memory", "after":cursor}})
                        complete = False
                        while not complete:
                            event = ws.read()
                            if "seq" in event:
                                self.assertGreater(event["seq"], cursor)
                                cursor = event["seq"]
                            self.assertNotIn(event.get("kind"), ("error", "event_gap"))
                            complete = event.get("kind") == "completed"
                    finally:
                        ws.close()
                    status, snapshot = request(server.url + "/v1/rpc", {"id":"get", "op":"session.get", "session_id":"resume", "data":{}}, token)
                    self.assertEqual(status, 200)
                    self.assertTrue(snapshot["ok"])
                    self.assertIn("first streaming response segment second segment", json.dumps(snapshot["data"]["conversation"]))
                    status, result = request(server.url + "/v1/rpc", run, token)
                    self.assertEqual(status, 200)
                    self.assertTrue(result["ok"])
                    self.assertEqual(len(calls), 1)
                finally:
                    release.set()
                    server.stop()
        finally:
            release.set()
            upstream.shutdown()
            upstream.server_close()
            thread.join(timeout=3)

    def test_process_crash_preserves_sync_receipt(self):
        entered, release = threading.Event(), threading.Event()
        calls = []
        class Provider(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass
            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", "0")))
                calls.append(1)
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                event = {"id":"x", "object":"chat.completion.chunk", "choices":[{"index":0,"delta":{"content":"persisted partial output with extra trailing characters"}}]}
                self.wfile.write(("data: " + json.dumps(event) + "\n\n").encode())
                self.wfile.flush()
                entered.set()
                release.wait(8)
        upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Provider)
        thread = threading.Thread(target=upstream.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory(prefix="ssg-crash-") as temp:
                root = Path(temp) / "identity"
                server = ServerRunner(root, allow_local_api=True)
                server.start()
                try:
                    invitation, _ = client.handshake_http(server.url, PASSWORD)
                    pair = client.pair(invitation, Path(temp) / "pair.json")
                    auth = {"client_id":pair["client_id"], "secret":pair["secret"]}
                    token = request(server.url + "/v1/auth", auth)[1]["token"]
                    conversation = {"id":"crash", "title":"x", "created_at":1, "updated_at":2, "messages":[]}
                    runtime = {"source":"mobile_ephemeral", "provider":{"type":"openai", "baseUrl":f"http://127.0.0.1:{upstream.server_port}", "modelId":"test", "auth":{"method":"bearer_token", "value":"secret"}}}
                    opening = {"id":"open", "op":"session.open", "session_id":"crash", "data":{"storage":"sync", "conversation":conversation, "runtime":runtime}}
                    self.assertTrue(request(server.url + "/v1/rpc", opening, token)[1]["ok"])
                    run = {"id":"run", "op":"session.run", "session_id":"crash", "data":{"content":"hello"}}
                    self.assertTrue(request(server.url + "/v1/rpc", run, token)[1]["ok"])
                    self.assertTrue(entered.wait(8))
                    deadline = time.monotonic() + 6
                    while time.monotonic() < deadline:
                        status, view = request(server.url + "/v1/rpc", {"id":"get", "op":"session.get", "session_id":"crash", "data":{}}, token)
                        if status == 200 and view["data"]["revision"] >= 3:
                            break
                        time.sleep(0.05)
                    else:
                        self.fail("stream checkpoint was not persisted")
                    server.process.kill()
                    server.process.wait(timeout=5)
                finally:
                    server.stop()
                restarted = ServerRunner(root, password="", allow_local_api=True)
                restarted.start()
                try:
                    token = request(restarted.url + "/v1/auth", auth)[1]["token"]
                    opening["data"] = {"storage":"sync"}
                    status, result = request(restarted.url + "/v1/rpc", opening, token)
                    self.assertEqual(status, 200)
                    self.assertTrue(result["ok"])
                    messages = result["data"]["conversation"]["messages"]
                    self.assertTrue(messages[-1]["versions"][0]["generation_interrupted"])
                    self.assertIn("persisted partial output", messages[-1]["versions"][0]["content"])
                    status, result = request(restarted.url + "/v1/rpc", run, token)
                    self.assertEqual(status, 409)
                    self.assertEqual(result["error"]["code"], "request_already_recorded")
                    self.assertEqual(len(calls), 1)
                    configure = {"id":"configure", "op":"session.configure", "session_id":"crash", "data":{"runtime":runtime}}
                    self.assertTrue(request(restarted.url + "/v1/rpc", configure, token)[1]["ok"])
                    new_run = {"id":"new-run", "op":"session.run", "session_id":"crash", "data":{"content":"continue"}}
                    status, result = request(restarted.url + "/v1/rpc", new_run, token)
                    self.assertEqual(status, 200)
                    self.assertTrue(result["ok"])
                    deadline = time.monotonic() + 5
                    while len(calls) < 2 and time.monotonic() < deadline:
                        time.sleep(0.02)
                    self.assertEqual(len(calls), 2)
                finally:
                    restarted.stop()
        finally:
            release.set()
            upstream.shutdown()
            upstream.server_close()
            thread.join(timeout=3)

    def test_process_crash_preserves_tool_receipt(self):
        entered, release = threading.Event(), threading.Event()
        calls = []
        class Provider(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass
            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", "0")))
                calls.append(1)
                entered.set()
                release.wait(8)
        upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Provider)
        thread = threading.Thread(target=upstream.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory(prefix="ssg-crash-") as temp:
                root = Path(temp) / "identity"
                server = ServerRunner(root, allow_local_api=True)
                server.start()
                try:
                    invitation, _ = client.handshake_http(server.url, PASSWORD)
                    pair = client.pair(invitation, Path(temp) / "pair.json")
                    auth = {"client_id":pair["client_id"], "secret":pair["secret"]}
                    token = request(server.url + "/v1/auth", auth)[1]["token"]
                    conversation = {"id":"crash", "title":"x", "created_at":1, "updated_at":2, "messages":[]}
                    runtime = {"source":"mobile_ephemeral", "customTools":[{"id":"t", "name":"tool", "enabled":True, "method":"POST", "url":f"http://127.0.0.1:{upstream.server_port}", "parameters":{"type":"object", "properties":{}}}], "toolGrants":{"custom_http":{"t":True}}}
                    opening = {"id":"open", "op":"session.open", "session_id":"crash", "data":{"storage":"sync", "conversation":conversation, "runtime":runtime}}
                    self.assertTrue(request(server.url + "/v1/rpc", opening, token)[1]["ok"])
                    run = {"id":"run", "op":"tool.call", "session_id":"crash", "data":{"kind":"custom_http", "tool_id":"t", "arguments":{}}}
                    self.assertTrue(request(server.url + "/v1/rpc", run, token)[1]["ok"])
                    self.assertTrue(entered.wait(8))
                    server.process.kill()
                    server.process.wait(timeout=5)
                finally:
                    server.stop()
                restarted = ServerRunner(root, password="", allow_local_api=True)
                restarted.start()
                try:
                    token = request(restarted.url + "/v1/auth", auth)[1]["token"]
                    opening["data"] = {"storage":"sync"}
                    status, result = request(restarted.url + "/v1/rpc", opening, token)
                    self.assertEqual(status, 200)
                    self.assertTrue(result["ok"])
                    messages = result["data"]["conversation"]["messages"]
                    self.assertTrue(messages[-1]["versions"][0]["generation_interrupted"])
                    activity = messages[-1]["versions"][0]["toolActivity"][0]
                    self.assertEqual(activity["status"], "error")
                    self.assertEqual(activity["response"], "tool_outcome_unknown_after_restart")
                    status, result = request(restarted.url + "/v1/rpc", run, token)
                    self.assertEqual(status, 409)
                    self.assertEqual(result["error"]["code"], "request_already_recorded")
                    self.assertEqual(len(calls), 1)
                finally:
                    restarted.stop()
        finally:
            release.set()
            upstream.shutdown()
            upstream.server_close()
            thread.join(timeout=3)

    def test_two_processes_race_same_synced_request(self):
        entered, release = threading.Event(), threading.Event()
        calls = []
        class Provider(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass
            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", "0")))
                calls.append(1)
                entered.set()
                release.wait(8)
                try:
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.end_headers()
                    self.wfile.write(b"data: [DONE]\n\n")
                except (BrokenPipeError, ConnectionResetError):
                    pass
        upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Provider)
        thread = threading.Thread(target=upstream.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory(prefix="ssg-split-") as temp:
                root = Path(temp) / "identity"
                first = ServerRunner(root, allow_local_api=True)
                first.start()
                second = None
                try:
                    invitation, _ = client.handshake_http(first.url, PASSWORD)
                    pair = client.pair(invitation, Path(temp) / "pair.json")
                    auth = {"client_id":pair["client_id"], "secret":pair["secret"]}
                    second = ServerRunner(root, password="", allow_local_api=True, data_socket=root / "run" / "second-data.sock")
                    second.ipc = root / "second-control.sock"
                    second.start()
                    tokens = [request(server.url + "/v1/auth", auth)[1]["token"] for server in (first, second)]
                    conversation = {"id":"race", "title":"x", "created_at":1, "updated_at":2, "messages":[]}
                    runtime = {"source":"mobile_ephemeral", "provider":{"type":"openai", "baseUrl":f"http://127.0.0.1:{upstream.server_port}", "modelId":"test", "auth":{"method":"bearer_token", "value":"secret"}}}
                    opening = {"id":"open", "op":"session.open", "session_id":"race", "data":{"storage":"sync", "conversation":conversation, "runtime":runtime}}
                    self.assertTrue(request(first.url + "/v1/rpc", opening, tokens[0])[1]["ok"])
                    opening["data"] = {"storage":"sync", "runtime":runtime}
                    self.assertTrue(request(second.url + "/v1/rpc", opening, tokens[1])[1]["ok"])
                    run = {"id":"run", "op":"session.run", "session_id":"race", "data":{"content":"hello"}}
                    barrier = threading.Barrier(2)
                    results = [None, None]
                    def invoke(index, server):
                        barrier.wait(timeout=5)
                        results[index] = request(server.url + "/v1/rpc", run, tokens[index])
                    workers = [threading.Thread(target=invoke, args=(i, server)) for i, server in enumerate((first, second))]
                    for worker in workers:
                        worker.start()
                    for worker in workers:
                        worker.join(timeout=8)
                        self.assertFalse(worker.is_alive())
                    self.assertEqual(sorted(status for status, _ in results), [200, 409])
                    conflict = next(body for status, body in results if status == 409)
                    self.assertIn(conflict["error"]["code"], ("revision_conflict", "session_busy"))
                    self.assertTrue(entered.wait(8))
                    self.assertEqual(len(calls), 1)
                    loser_index = next(i for i, (status, _) in enumerate(results) if status == 409)
                    loser = (first, second)[loser_index]
                    close = {"id":"close", "op":"session.close", "session_id":"race", "data":{}}
                    self.assertTrue(request(loser.url + "/v1/rpc", close, tokens[loser_index])[1]["ok"])
                    reopened = request(loser.url + "/v1/rpc", opening, tokens[loser_index])[1]
                    self.assertTrue(reopened["ok"])
                    revision = reopened["data"]["revision"]
                    cli = [str(BINARY), "chat", "reload", "--id", "race", "--client-id", pair["client_id"], "--data-dir", str(root / "data"), "--if-revision", str(revision)]
                    result = subprocess.run(cli, capture_output=True, text=True)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("job is active", result.stderr)
                    save = {"id":"save", "op":"session.save", "session_id":"race", "data":{"expected_revision":revision, "idempotency_key":"save-key", "conversation":reopened["data"]["conversation"]}}
                    delete = {"id":"delete", "op":"session.delete", "session_id":"race", "data":{"expected_revision":revision, "idempotency_key":"delete-key"}}
                    for mutation in (save, delete):
                        status, blocked = request(loser.url + "/v1/rpc", mutation, tokens[loser_index])
                        self.assertEqual((status, blocked["error"]["code"]), (409, "session_busy"))
                    distinct = {"id":"distinct", "op":"session.run", "session_id":"race", "data":{"content":"another"}}
                    status, blocked = request(loser.url + "/v1/rpc", distinct, tokens[loser_index])
                    self.assertEqual((status, blocked["error"]["code"]), (409, "session_busy"))
                    tool_runtime = {"source":"mobile_ephemeral", "customTools":[{"id":"t", "name":"tool", "enabled":True, "method":"POST", "url":f"http://127.0.0.1:{upstream.server_port}", "parameters":{"type":"object", "properties":{}}}], "toolGrants":{"custom_http":{"t":True}}}
                    configure = {"id":"configure", "op":"session.configure", "session_id":"race", "data":{"runtime":tool_runtime}}
                    self.assertTrue(request(loser.url + "/v1/rpc", configure, tokens[loser_index])[1]["ok"])
                    tool = {"id":"tool", "op":"tool.call", "session_id":"race", "data":{"kind":"custom_http", "tool_id":"t", "arguments":{}}}
                    status, blocked = request(loser.url + "/v1/rpc", tool, tokens[loser_index])
                    self.assertEqual((status, blocked["error"]["code"]), (409, "session_busy"))
                    self.assertEqual(len(calls), 1)
                finally:
                    release.set()
                    if second:
                        second.stop()
                    first.stop()
        finally:
            release.set()
            upstream.shutdown()
            upstream.server_close()
            thread.join(timeout=3)

    def test_cli_reload_hand_edit_requires_session_reopen(self):
        with tempfile.TemporaryDirectory(prefix="ssg-reload-") as temp:
            root = Path(temp) / "identity"
            server = ServerRunner(root)
            server.start()
            try:
                invitation, _ = client.handshake_http(server.url, PASSWORD)
                pair = client.pair(invitation, Path(temp) / "pair.json")
                auth = {"client_id":pair["client_id"], "secret":pair["secret"]}
                token = request(server.url + "/v1/auth", auth)[1]["token"]
                conversation = {"id":"edited", "title":"original", "created_at":1, "updated_at":2, "messages":[]}
                opening = {"id":"open", "op":"session.open", "session_id":"edited", "data":{"storage":"sync", "conversation":conversation}}
                self.assertTrue(request(server.url + "/v1/rpc", opening, token)[1]["ok"])
                filename = hashlib.sha256(b"edited").hexdigest() + ".json"
                path = root / "data" / "chats" / filename
                snapshot = json.loads(path.read_text())
                snapshot["conversation"]["title"] = "hand edited"
                path.write_text(json.dumps(snapshot))
                get = {"id":"get", "op":"session.get", "session_id":"edited", "data":{}}
                status, result = request(server.url + "/v1/rpc", get, token)
                self.assertEqual((status, result["error"]["code"]), (409, "revision_conflict"))
                command = [str(BINARY), "chat", "reload", "--id", "edited", "--client-id", pair["client_id"], "--data-dir", str(root / "data"), "--if-revision", "1"]
                before_reload = path.read_bytes()
                subprocess.run(command, check=True, capture_output=True, text=True)
                backups = list((root / "data" / ".backups" / "chats").glob("*.json"))
                self.assertEqual(len(backups), 1)
                self.assertEqual(backups[0].read_bytes(), before_reload)
                self.assertEqual(backups[0].stat().st_mode & 0o777, 0o600)
                close = {"id":"close", "op":"session.close", "session_id":"edited", "data":{}}
                self.assertTrue(request(server.url + "/v1/rpc", close, token)[1]["ok"])
                opening["data"] = {"storage":"sync"}
                status, result = request(server.url + "/v1/rpc", opening, token)
                self.assertEqual(status, 200)
                self.assertEqual(result["data"]["revision"], 2)
                self.assertEqual(result["data"]["conversation"]["title"], "hand edited")
            finally:
                server.stop()


    def test_server_conversation_fixture_round_trips_transports_and_restart(self):
        fixture_path = Path(__file__).resolve().parent.parent / "internal" / "compat" / "testdata" / "conversation-server.json"
        fixture = json.loads(fixture_path.read_text())
        with tempfile.TemporaryDirectory(prefix="ssg-fixture-") as temp:
            root = Path(temp) / "identity"
            server = ServerRunner(root)
            server.start()
            try:
                invitation, _ = client.handshake_http(server.url, PASSWORD)
                pair = client.pair(invitation, Path(temp) / "pair.json")
                auth = {"client_id":pair["client_id"], "secret":pair["secret"]}
                token = request(server.url + "/v1/auth", auth)[1]["token"]
                opening = {"id":"open", "op":"session.open", "session_id":fixture["id"], "data":{"storage":"memory", "conversation":fixture}}
                status, result = request(server.url + "/v1/rpc", opening, token)
                self.assertEqual(status, 200)
                self.assertEqual(result["data"]["conversation"], fixture)
                self.assertTrue(list((root / "data" / "chats").glob("*.json")))
                close = {"id":"close", "op":"session.close", "session_id":fixture["id"], "data":{}}
                self.assertTrue(request(server.url + "/v1/rpc", close, token)[1]["ok"])
                # No storage-mode transition: close only releases RAM resources.
                opening["data"].pop("storage", None)
                status, result = request(server.url + "/v1/rpc", opening, token)
                self.assertEqual(status, 200)
                self.assertEqual(result["data"]["conversation"], fixture)
                ws = WS(server.url)
                try:
                    ws.send({"type":"auth", "protocol":5, **auth})
                    self.assertEqual(ws.read()["type"], "paired")
                    ws.send({"id":"get", "op":"session.get", "session_id":fixture["id"], "data":{}})
                    self.assertEqual(ws.read()["data"]["conversation"], fixture)
                finally:
                    ws.close()
                save = {"id":"save", "op":"session.save", "session_id":fixture["id"], "data":{"expected_revision":1, "idempotency_key":"fixture-save", "conversation":fixture}}
                status, first = request(server.url + "/v1/rpc", save, token)
                self.assertEqual(status, 200)
                status, replay = request(server.url + "/v1/rpc", save, token)
                self.assertEqual(status, 200)
                self.assertEqual(first["data"], replay["data"])
                self.assertEqual(first["data"]["conversation"], fixture)
            finally:
                server.stop()
            restarted = ServerRunner(root, password="")
            restarted.start()
            try:
                token = request(restarted.url + "/v1/auth", auth)[1]["token"]
                opening["data"] = {"storage":"sync"}
                status, result = request(restarted.url + "/v1/rpc", opening, token)
                self.assertEqual(status, 200)
                self.assertEqual(result["data"]["revision"], 2)
                self.assertEqual(result["data"]["conversation"], fixture)
            finally:
                restarted.stop()
