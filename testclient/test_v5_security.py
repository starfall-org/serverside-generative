"""End-to-end v5 permanent conversation storage and ephemeral runtime credentials."""
import http.server
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request

import client
from test_integration import BINARY, PASSWORD, ServerRunner

SENTINEL = "v5-runtime-secret-sentinel-493571"
LOCAL_SECRET = "v5-local-key-secret-sentinel-817365"


class Provider(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        for part in (SENTINEL[:14], SENTINEL[14:]):
            event = {"id": "x", "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": {"content": part}}]}
            self.wfile.write(("data: " + json.dumps(event) + "\n\n").encode())
            self.wfile.flush()
        try:
            self.wfile.write(b"data: [DONE]\n\n")
        except BrokenPipeError:
            pass


def request(url, payload=None, token=None):
    headers = {"Accept": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    data = None if payload is None else json.dumps(payload).encode()
    if data is not None:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=8) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, json.load(error)


class SecurityIntegration(unittest.TestCase):
    def test_ephemeral_runtime_credentials_no_write_and_all_chats_restart(self):
        upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Provider)
        thread = threading.Thread(target=upstream.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory(prefix="ssg-v5-") as temp:
                root = Path(temp) / "identity"
                runner = ServerRunner(root, allow_local_api=True)
                runner.start()
                try:
                    invitation, _ = client.handshake_http(runner.url, PASSWORD)
                    state = client.pair(invitation, Path(temp) / "mobile-state.json")
                    auth = {"client_id": state["client_id"], "secret": state["secret"]}
                    status, result = request(runner.url + "/v1/auth", auth)
                    self.assertEqual(status, 200)
                    token = result["token"]
                    data_dir = root / "data"
                    for cmd, content in [
                        (["config", "init", "--data-dir", str(data_dir)], None),
                        (["provider", "add", "--id", "p", "--name", "P", "--type", "openai", "--data-dir", str(data_dir), "--if-revision", "1"], None),
                        (["provider", "key", "set", "p", "--key-id", "main", "--stdin", "--data-dir", str(data_dir), "--if-revision", "2"], LOCAL_SECRET + "\n"),
                    ]:
                        completed = subprocess.run([str(BINARY), *cmd], input=content, text=True, capture_output=True, check=True)
                        self.assertNotIn(LOCAL_SECRET, completed.stdout + completed.stderr)

                    def rpc(request_id, operation, session, body):
                        return request(runner.url + "/v1/rpc", {"id": request_id, "op": operation, "session_id": session, "data": body}, token)

                    for storage, session in (("memory", "mem"), ("sync", "sync")):
                        conversation = {"id": session, "title": "Test", "created_at": 1, "updated_at": 2, "messages": []}
                        runtime = {"source": "mobile_ephemeral", "provider": {"type": "openai", "baseUrl": f"http://127.0.0.1:{upstream.server_port}", "modelId": "model", "auth": {"method": "bearer_token", "value": SENTINEL}}}
                        self.assertTrue(rpc("open-" + session, "session.open", session, {"storage": storage, "conversation": conversation, "runtime": runtime})[1]["ok"])
                        self.assertTrue(rpc("run-" + session, "session.run", session, {"content": "hello"})[1]["ok"])
                        cursor = 0
                        deadline = time.monotonic() + 8
                        complete = False
                        while time.monotonic() < deadline and not complete:
                            status, page = request(f"{runner.url}/v1/events?session_id={session}&after={cursor}", token=token)
                            self.assertEqual(status, 200)
                            self.assertNotIn(SENTINEL, json.dumps(page))
                            for event in page["events"]:
                                cursor = event["seq"]
                                if event["kind"] == "error":
                                    self.assertEqual(event["data"]["code"], "secret_leak_blocked")
                                    complete = True
                        self.assertTrue(complete)
                        self.assertTrue((data_dir / "chats").is_dir())
                        self.assertTrue(list((data_dir / "chats").glob("*.json")))
                    for file in root.rglob("*"):
                        if file.is_file() and not file.is_symlink():
                            contents = file.read_bytes()
                            self.assertNotIn(SENTINEL.encode(), contents, str(file))
                            self.assertNotIn(LOCAL_SECRET.encode(), contents, str(file))
                finally:
                    runner.stop()

                restarted = ServerRunner(root, password="", allow_local_api=True)
                restarted.start()
                try:
                    status, result = request(restarted.url + "/v1/auth", auth)
                    self.assertEqual(status, 200)
                    token = result["token"]
                    # The in-process handle is lost on restart; the stored
                    # conversation is recovered without any sync setting.
                    status, result = request(restarted.url + "/v1/rpc", {"id": "get-mem", "op": "session.get", "session_id": "mem", "data": {}}, token)
                    self.assertEqual(status, 404)
                    status, result = request(restarted.url + "/v1/rpc", {"id":"reopen-mem","op":"session.open","session_id":"mem","data":{}}, token)
                    self.assertEqual(status, 200)
                    self.assertEqual(result["data"]["revision"], 3)
                    self.assertTrue(result["data"]["conversation"]["messages"])
                    status, result = request(restarted.url + "/v1/rpc", {"id":"duplicate-mem","op":"session.run","session_id":"mem","data":{"request_id":"run-mem","content":"hello"}}, token)
                    self.assertEqual(result["error"]["code"], "request_already_recorded")
                    status, result = request(restarted.url + "/v1/rpc", {"id": "open-sync", "op": "session.open", "session_id": "sync", "data": {"storage": "sync"}}, token)
                    self.assertTrue(result["ok"])
                    self.assertEqual(result["data"]["revision"], 3)
                    status, result = request(restarted.url + "/v1/rpc", {"id": "run-sync", "op": "session.run", "session_id": "sync", "data": {"content": "hello"}}, token)
                    self.assertEqual(result["error"]["code"], "request_already_recorded")
                    status, result = request(restarted.url + "/v1/rpc", {"id": "new-run-sync", "op": "session.run", "session_id": "sync", "data": {"content": "again"}}, token)
                    self.assertEqual(result["error"]["code"], "missing_runtime_config")
                finally:
                    restarted.stop()
        finally:
            upstream.shutdown()
            upstream.server_close()
            thread.join(timeout=3)


class OAuthExpiryIntegration(unittest.TestCase):
    def test_upstream_unauthorized_is_safe_auth_required_event(self):
        class Unauthorized(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass
            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", "0")))
                self.send_response(401)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"error":{"message":SENTINEL, "type":"authentication_error"}}).encode())
        upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Unauthorized)
        thread = threading.Thread(target=upstream.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory(prefix="ssg-oauth-") as temp:
                server = ServerRunner(Path(temp) / "identity", allow_local_api=True)
                server.start()
                try:
                    invitation, _ = client.handshake_http(server.url, PASSWORD)
                    pair = client.pair(invitation, Path(temp) / "pair.json")
                    auth = {"client_id":pair["client_id"], "secret":pair["secret"]}
                    token = request(server.url + "/v1/auth", auth)[1]["token"]
                    runtime = {"source":"mobile_ephemeral", "provider":{"type":"openai", "baseUrl":f"http://127.0.0.1:{upstream.server_port}", "modelId":"test", "auth":{"method":"oauth", "value":SENTINEL}}}
                    conversation = {"id":"oauth", "title":"x", "created_at":1, "updated_at":2, "messages":[]}
                    opening = {"id":"open", "op":"session.open", "session_id":"oauth", "data":{"storage":"memory", "conversation":conversation, "runtime":runtime}}
                    self.assertTrue(request(server.url + "/v1/rpc", opening, token)[1]["ok"])
                    run = {"id":"run", "op":"session.run", "session_id":"oauth", "data":{"content":"hello"}}
                    self.assertTrue(request(server.url + "/v1/rpc", run, token)[1]["ok"])
                    cursor = 0
                    while True:
                        status, page = request(f"{server.url}/v1/events?session_id=oauth&after={cursor}", token=token)
                        self.assertEqual(status, 200)
                        self.assertNotIn(SENTINEL, json.dumps(page))
                        done = False
                        for event in page["events"]:
                            cursor = event["seq"]
                            if event["kind"] == "error":
                                self.assertEqual(event["data"]["code"], "auth_required")
                                done = True
                        if done:
                            break
                    self.assertTrue((server.root / "data" / "chats").is_dir())
                    for stored in (server.root / "data" / "chats").glob("*.json"):
                        self.assertNotIn(SENTINEL, stored.read_text())
                finally:
                    server.stop()
        finally:
            upstream.shutdown()
            upstream.server_close()
            thread.join(timeout=3)
