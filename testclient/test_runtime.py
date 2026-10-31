#!/usr/bin/env python3
"""Real Go SDK wire tests against local fake provider endpoints (no paid API calls)."""
from __future__ import annotations

import http.server
import json
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path

from test_integration import PASSWORD, ServerRunner
import client


class FakeProvider(http.server.BaseHTTPRequestHandler):
    recorded: list[tuple[str, dict, dict]] = []

    def log_message(self, fmt, *args):
        pass

    def do_POST(self):
        if self.headers.get("Authorization") != "Bearer mock-key" and self.headers.get("x-api-key") != "mock-key" and self.headers.get("x-goog-api-key") != "mock-key":
            self.send_error(401, "invalid test key")
            return
        raw = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        try:
            body = json.loads(raw)
        except ValueError:
            self.send_error(400, "invalid body")
            return
        self.recorded.append((self.path, dict(self.headers), body))
        if self.path.endswith("/responses"):
            chunks = [
                ("response.output_text.delta", {"type": "response.output_text.delta", "delta": "SDK ",
                                                "sequence_number": 1}),
                ("response.output_text.delta", {"type": "response.output_text.delta", "delta": "working",
                                                "sequence_number": 2}),
            ]
        elif self.path.endswith("/chat/completions"):
            chunks = [
                (None, {"id": "chatcmpl_mock", "object": "chat.completion.chunk",
                        "choices": [{"index": 0, "delta": {"content": "SDK "}}]}),
                (None, {"id": "chatcmpl_mock", "object": "chat.completion.chunk",
                        "choices": [{"index": 0, "delta": {"content": "working"}, "finish_reason": "stop"}]}),
            ]
        elif self.path.endswith("/messages"):
            chunks = [
                ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                         "delta": {"type": "text_delta", "text": "SDK "}}),
                ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                         "delta": {"type": "text_delta", "text": "working"}}),
                ("message_stop", {"type": "message_stop"}),
            ]
        elif "streamGenerateContent" in self.path:
            chunks = [
                (None, {"candidates": [{"content": {"role": "model", "parts": [{"text": "SDK "}]},"finishReason": "STOP"}]}),
                (None, {"candidates": [{"content": {"role": "model", "parts": [{"text": "working"}]},"finishReason": "STOP"}]}),
            ]
        else:
            self.send_error(404, "unsupported mock SDK endpoint")
            return

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        try:
            for name, payload in chunks:
                if name:
                    self.wfile.write(f"event: {name}\n".encode())
                self.wfile.write(("data: " + json.dumps(payload) + "\n\n").encode())
                self.wfile.flush()
            if "streamGenerateContent" not in self.path:
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass


class ProviderWireTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix="serverside-provider-")
        cls.root = Path(cls.temp.name)
        cls.mock = http.server.ThreadingHTTPServer(("127.0.0.1", 0), FakeProvider)
        cls.thread = threading.Thread(target=cls.mock.serve_forever, daemon=True)
        cls.thread.start()
        cls.runtime = ServerRunner(cls.root / "runtime")
        cls.runtime.start()
        cls.invite, _ = client.handshake_http(cls.runtime.url, PASSWORD)
        cls.state = client.pair(cls.invite, cls.root / "client.json")

    @classmethod
    def tearDownClass(cls):
        cls.runtime.stop()
        cls.mock.shutdown()
        cls.mock.server_close()
        cls.thread.join(timeout=5)
        cls.temp.cleanup()

    def model_in_background(self, provider: str, model: str, *, max_tokens: int = 0):
        endpoint = f"http://127.0.0.1:{self.mock.server_port}"
        if provider == "openai-responses" or provider == "openai-chat":
            endpoint += "/v1"
        elif provider == "anthropic":
            endpoint += "/v1"
        state = {
            "provider": {"platform": provider, "api_key": "mock-key", "model": model,
                         "base_url": endpoint, "max_tokens": max_tokens},
            "conversation": [{"role": "system", "content": "Answer succinctly."},
                             {"role": "user", "content": "Say SDK working."}],
            "mcp": [],
        }
        session_id = "model-" + provider
        sock = client.reconnect(self.state)
        try:
            client.write_frame(sock, {"type": "handoff", "session_id": session_id,
                                      "revision": 1, "state": state})
            self.assertEqual(client.read_frame(sock)["type"], "handoff_ack")
            client.write_frame(sock, {"type": "start", "session_id": session_id,
                                      "request_id": "job-1", "kind": "model"})
            answer = client.read_frame(sock)
            self.assertEqual(answer["type"], "start_ack", answer)
        finally:
            sock.close()  # All SDK model activity must continue on the server.

        sock = client.reconnect(self.state)
        sock.settimeout(20)
        try:
            client.write_frame(sock, {"type": "sync", "session_id": session_id, "since": 0})
            self.assertEqual(client.read_frame(sock)["type"], "sync_begin")
            seen = []
            while True:
                event = client.read_frame(sock)
                if event.get("type") != "event":
                    continue
                seen.append(event)
                if event.get("event") in ("completed", "error"):
                    break
            terminal = seen[-1]
            self.assertEqual(terminal["event"], "completed", seen)
            text = "".join(e["content"] for e in seen if e["event"] == "delta")
            self.assertEqual(text, "SDK working")
            client.write_frame(sock, {"type": "ack", "session_id": session_id,
                                      "seq": terminal["seq"]})
            # sync_end may follow replayed completion before the ACK response.
            reply = client.read_frame(sock)
            while reply.get("type") == "sync_end":
                reply = client.read_frame(sock)
            self.assertEqual(reply["type"], "ack_ok", reply)
        finally:
            sock.close()
        self.assertEqual((self.root / "runtime" / "clients.json").read_text().find("mock-key"), -1)
        return state

    def test_openai_responses_sdk(self):
        state = self.model_in_background("openai-responses", "test-response-model")
        self.assertTrue(any("/responses" in path for path, _, _ in FakeProvider.recorded))

    def test_openai_chat_sdk(self):
        self.model_in_background("openai-chat", "test-chat-model")
        self.assertTrue(any(path.endswith("/chat/completions") for path, _, _ in FakeProvider.recorded))

    def test_anthropic_sdk(self):
        self.model_in_background("anthropic", "claude-test-model", max_tokens=32)
        self.assertTrue(any(path.endswith("/messages") for path, _, _ in FakeProvider.recorded))

    def test_python_cli_run_real_model(self):
        config_path = self.root / "mock-runtime.json"
        config_path.write_text(json.dumps({
            "provider": {
                "platform": "openai-chat",
                "api_key": "mock-key",
                "model": "test",
                "base_url": f"http://127.0.0.1:{self.mock.server_port}/v1"
            },
            "conversation": [{"role": "user", "content": "Call the SDK"}],
        }))
        output = subprocess.check_output([
            sys.executable, str(Path(__file__).with_name("client.py")), "run",
            "--state", str(self.root / "client.json"),
            "--config", str(config_path),
            "--kind", "model",
            "--session", "cli-model"
        ], text=True, timeout=30)
        result = json.loads(output)
        self.assertEqual(result["text"], "SDK working")
        self.assertGreaterEqual(result["events"], 2)

    def test_gemini_sdk(self):
        self.model_in_background("gemini", "gemini-test-model")
        self.assertTrue(any("streamGenerateContent" in path for path, _, _ in FakeProvider.recorded))


if __name__ == "__main__":
    unittest.main(verbosity=2)