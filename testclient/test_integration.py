#!/usr/bin/env python3
"""End-to-end integration tests: no SSH daemon, Firebase, third-party services or pip packages required."""
from __future__ import annotations

import json
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
import tempfile
import time
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import client  # noqa: E402

ROOT = Path(__file__).resolve().parent.parent
BINARY = Path(os.environ.get("SERVER_BINARY", str(ROOT / "serverside")))
PASSWORD = "integration-test-password-1234"


def open_port() -> int:
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


class ServerRunner:
    def __init__(self, root: Path, *, http_only: bool = False, password: str = PASSWORD,
                 enable_terminal: bool = False, terminal_cwd: str | None = None):
        self.root = root
        self.tcp_port, self.http_port = open_port(), open_port()
        self.ipc = root / "control.sock"
        self.password = password
        self.http_only = http_only
        self.enable_terminal = enable_terminal
        self.terminal_cwd = terminal_cwd
        self.process: subprocess.Popen | None = None
        self.log_file = None

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.http_port}"

    def start(self) -> None:
        self.root.mkdir(parents=True, exist_ok=True)
        args = [
            str(BINARY), "serve",
            "--tcp-listen", f"127.0.0.1:{self.tcp_port}",
            "--advertise", f"127.0.0.1:{self.tcp_port}",
            "--http-listen", f"127.0.0.1:{self.http_port}",
            "--identity-dir", str(self.root),
            "--ipc", str(self.ipc),
        ]
        if self.http_only:
            args.append("--http-only")
        if self.enable_terminal:
            args.append("--enable-terminal")
        if self.terminal_cwd:
            args.extend(["--terminal-cwd", self.terminal_cwd])
        self.log_file = open(self.root / "daemon-test-log.txt", "a")
        env = {**os.environ, "GENERATIVE_HANDSHAKE_PASSWORD": self.password}
        self.process = subprocess.Popen(
            args, env=env,
            stdout=self.log_file, stderr=self.log_file,
        )
        deadline = time.monotonic() + 8
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise RuntimeError(f"Daemon exited early: {self.process.returncode}")
            try:
                sock = socket.create_connection(("127.0.0.1", self.tcp_port), timeout=.15)
                sock.close()
                if self.ipc.exists():
                    if not self.password:
                        return
                    # TCP can be ready before the HTTP/WS handshake listener.
                    try:
                        check = socket.create_connection(("127.0.0.1", self.http_port), timeout=.15)
                    except OSError:
                        time.sleep(.06)
                        continue
                    else:
                        check.close()
                        return
            except (ConnectionError, OSError):
                time.sleep(.06)
        raise TimeoutError("Server failed to start")

    def stop(self) -> None:
        if self.process:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=3)
            self.process = None
        if self.log_file:
            self.log_file.close()
            self.log_file = None


class Integration(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix="ssg-integration-")
        cls.root = Path(cls.temp.name)
        cls.runner = ServerRunner(cls.root / "identity")
        cls.runner.start()

    @classmethod
    def tearDownClass(cls):
        cls.runner.stop()
        cls.temp.cleanup()

    def make_pair(self, invite: dict) -> dict:
        state_file = self.root / f"client-{time.time_ns()}.json"
        state = client.pair(invite, state_file)
        self.assertEqual(state_file.stat().st_mode & 0o777, 0o600)
        self.assertEqual(state["endpoint"], f"127.0.0.1:{self.runner.tcp_port}")
        return state

    def test_01_websocket_then_disconnect_reconnect(self):
        invite, method = client.handshake_http(self.runner.url, PASSWORD)
        self.assertEqual(method, "websocket")
        state = self.make_pair(invite)
        output = client.demo(state, "ws-test", "stream persists in RAM after disconnect", 500)
        self.assertEqual(output["content"], "stream persists in RAM after disconnect")
        self.assertEqual(output["status"]["pending_events"], 0)

    def test_02_http_wrong_password(self):
        with self.assertRaises(PermissionError):
            client.handshake_http(self.runner.url, "totally-wrong-password")

    def test_03_ssh_stdio_authenticated_via_control_socket(self):
        command = f"{BINARY} handshake --stdio --ipc {self.runner.ipc}"
        invite = client.handshake_ssh(command)
        state = self.make_pair(invite)
        self.assertEqual(client.demo(state, "ssh-test", "SSH closed; TCP stays up", 350)["content"],
                         "SSH closed; TCP stays up")

    def test_04_human_qr_and_manual(self):
        output = subprocess.check_output(
            [str(BINARY), "pair", "--ipc", str(self.runner.ipc)], text=True
        )
        uri = re.search(r"Invite URI:\s+(generative://\S+)", output).group(1)
        scan_invite = client.handshake_human(uri, None, None, None)
        state = self.make_pair(scan_invite)
        self.assertEqual(client.demo(state, "qr-test", "QR used as one-time invitation", 200)["content"],
                         "QR used as one-time invitation")
        with self.assertRaises(RuntimeError):
            client.auth_socket(scan_invite)
        output = subprocess.check_output(
            [str(BINARY), "pair", "--ipc", str(self.runner.ipc)], text=True
        )
        manual = client.handshake_human(
            None,
            re.search(r"Endpoint:\s+(\S+)", output).group(1),
            re.search(r"TLS SHA-256:\s+(\S+)", output).group(1),
            re.search(r"Manual PIN:\s+(\S+)", output).group(1),
        )
        self.make_pair(manual)

    def test_05_tls_pinning_and_client_isolation(self):
        first = self.make_pair(client.handshake_http(self.runner.url, PASSWORD)[0])
        second = self.make_pair(client.handshake_http(self.runner.url, PASSWORD)[0])
        sock = client.reconnect(first)
        try:
            client.write_frame(sock, {"type": "handoff", "session_id": "exclusive", "revision": 1,
                                      "state": {"private_text": "DO_NOT_WRITE_TO_DISK_87439"}})
            self.assertEqual(client.read_frame(sock)["type"], "handoff_ack")
        finally:
            sock.close()
        sock = client.reconnect(second)
        try:
            client.write_frame(sock, {"type": "status", "session_id": "exclusive"})
            self.assertEqual(client.read_frame(sock)["type"], "error")
        finally:
            sock.close()
        impostor = {**first, "fingerprint": "0" * 64}
        with self.assertRaises(ValueError):
            client.reconnect(impostor)
        self.assertNotIn("DO_NOT_WRITE_TO_DISK_87439",
                         (self.runner.root / "clients.json").read_text())

    def test_06_dedup_and_background(self):
        state = self.make_pair(client.handshake_http(self.runner.url, PASSWORD)[0])
        sock = client.reconnect(state)
        try:
            client.write_frame(sock, {"type": "handoff", "session_id": "dedup", "revision": 1,
                                      "state": {"model": "test"}})
            self.assertEqual(client.read_frame(sock)["type"], "handoff_ack")
            msg = {"type": "start", "session_id": "dedup", "request_id": "same",
                   "kind": "echo", "content": "unique", "delay_ms": 150}
            for _ in range(2):
                client.write_frame(sock, msg)
                self.assertEqual(client.read_frame(sock)["type"], "start_ack")
        finally:
            sock.close()
        time.sleep(.5)
        sock = client.reconnect(state)
        try:
            client.write_frame(sock, {"type": "sync", "session_id": "dedup", "since": 0})
            self.assertEqual(client.read_frame(sock)["type"], "sync_begin")
            events = []
            while True:
                e = client.read_frame(sock)
                if e["type"] == "sync_end":
                    break
                events.append(e)
            self.assertEqual(sum(e.get("event") == "completed" for e in events), 1)
        finally:
            sock.close()

    def test_99_restart_no_session_durability_but_pair_durability(self):
        state = self.make_pair(client.handshake_http(self.runner.url, PASSWORD)[0])
        sock = client.reconnect(state)
        client.write_frame(sock, {"type": "handoff", "session_id": "ephemeral", "revision": 1,
                                  "state": {"api_key": "RAM_ONLY_SECRET_989393"}})
        self.assertEqual(client.read_frame(sock)["type"], "handoff_ack")
        sock.close()
        self.runner.stop()
        self.runner.start()
        sock = client.reconnect(state)
        try:
            client.write_frame(sock, {"type": "status", "session_id": "ephemeral"})
            self.assertEqual(client.read_frame(sock)["type"], "error")
            client.write_frame(sock, {"type": "handoff", "session_id": "ephemeral", "revision": 1,
                                      "state": {"resynced": True}})
            self.assertEqual(client.read_frame(sock)["type"], "handoff_ack")
        finally:
            sock.close()
        disk = (self.runner.root / "clients.json").read_text()
        self.assertNotIn("RAM_ONLY_SECRET_989393", disk)
        self.assertNotIn(state["secret"], disk)


class PasswordlessSSH(unittest.TestCase):
    def test_ssh_handshake_without_http_or_password(self):
        with tempfile.TemporaryDirectory(prefix="ssg-ssh-only-") as directory:
            runner = ServerRunner(Path(directory) / "identity", password="")
            runner.start()
            try:
                self.assertIsNone(runner.process.poll())
                # The HTTP handshake is disabled, not secretly exposed without auth.
                with self.assertRaises(OSError):
                    socket.create_connection(("127.0.0.1", runner.http_port), timeout=.2)
                command = f"{BINARY} handshake --stdio --ipc {runner.ipc}"
                invite = client.handshake_ssh(command)
                self.assertTrue(invite["ticket"])
                state = client.pair(invite, Path(directory) / "ssh-client.json")
                self.assertEqual(
                    client.demo(state, "ssh-only", "SSH-only pairing without application password", 100)["content"],
                    "SSH-only pairing without application password"
                )
                output = (runner.root / "daemon-test-log.txt").read_text()
                self.assertIn("no password; SSH/Human Relay available", output)
                self.assertNotIn("HTTP/WS handshake: 127.0.0.1", output)
            finally:
                runner.stop()


class HTTPFallback(unittest.TestCase):
    def test_fallback_to_http(self):
        with tempfile.TemporaryDirectory(prefix="ssg-fallback-") as directory:
            runner = ServerRunner(Path(directory) / "identity", http_only=True)
            runner.start()
            try:
                invite, method = client.handshake_http(runner.url, PASSWORD)
                self.assertEqual(method, "http")
                state = client.pair(invite, Path(directory) / "client.json")
                self.assertEqual(client.demo(state, "fallback", "http-fallback", 100)["content"],
                                 "http-fallback")
            finally:
                runner.stop()


if __name__ == "__main__":
    unittest.main(verbosity=2)