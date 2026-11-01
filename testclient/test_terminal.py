#!/usr/bin/env python3
"""End-to-end terminal tools over authenticated, reconnected TLS transport."""
from __future__ import annotations

import base64
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest

import client
from test_integration import PASSWORD, ServerRunner

class TerminalIntegration(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix="ssg-terminal-")
        cls.root = Path(cls.temp.name)
        cls.runner = ServerRunner(cls.root / "daemon", enable_terminal=True,
                                  terminal_cwd=str(cls.root / "terminal-workdir"))
        (cls.root / "terminal-workdir").mkdir(parents=True, exist_ok=True)
        cls.runner.start()
        invitation, _ = client.handshake_http(cls.runner.url, PASSWORD)
        cls.first = client.pair(invitation, cls.root / "client-a.json")
        invitation, _ = client.handshake_http(cls.runner.url, PASSWORD)
        cls.second = client.pair(invitation, cls.root / "client-b.json")

    @classmethod
    def tearDownClass(cls):
        cls.runner.stop()
        cls.temp.cleanup()

    def call(self, action: str, *, second: bool = False, **payload):
        return client.terminal_request(self.second if second else self.first, action, **payload)

    def wait_for(self, terminal_id: str, offset: int, expected: str, second: bool = False) -> tuple[str, int]:
        data = ""
        deadline = time.monotonic() + 8
        while time.monotonic() < deadline:
            result = self.call("read", second=second, terminal_id=terminal_id,
                               offset=offset, wait_ms=300)
            decoded = base64.b64decode(result["data_base64"])
            self.assertEqual(len(decoded), result["next_offset"] - result["offset"])
            data += decoded.decode("utf-8", "replace")
            offset = result["next_offset"]
            if expected in data:
                return data, offset
        self.fail(f"expected {expected!r} in terminal output; got {data!r}")

    def test_01_tools_and_command_survive_reconnect(self):
        tools = self.call("tools")
        available = {item["name"] for item in tools["tools"] if item["enabled"]}
        self.assertTrue({"terminal_create","terminal_execute","terminal_read","terminal_close"}.issubset(available))
        created = self.call("create", rows=30, cols=120)
        self.assertEqual(created["type"], "terminal_created")
        tid = created["terminal_id"]
        try:
            self.assertIn(tid, [t["terminal_id"] for t in self.call("list")["terminals"]])
            self.call("execute", terminal_id=tid,
                      command="sleep 0.3; printf '%s_%s' 'OUTPUT' 'AFTER_DISCONNECT'", wait_ms=0)
            # First TCP client has already closed by terminal_request.
            time.sleep(.45)
            output, cursor = self.wait_for(tid, 0, "OUTPUT_AFTER_DISCONNECT", second=True)
            self.assertIn("OUTPUT_AFTER_DISCONNECT", output)
            self.assertGreater(cursor,0)
            self.assertIn(tid, [t["terminal_id"] for t in self.call("list", second=True)["terminals"]])
            self.call("execute", second=True, terminal_id=tid,
                      command="printf '%s%s' 'SECOND' '_CLIENT_DONE'", wait_ms=0)
            output, _ = self.wait_for(tid, cursor, "SECOND_CLIENT_DONE")
            self.assertIn("SECOND_CLIENT_DONE", output)
        finally:
            self.assertEqual(self.call("close", terminal_id=tid)["type"], "terminal_closed")
        with self.assertRaises(RuntimeError):
            self.call("read", terminal_id=tid, offset=0)
        self.assertNotIn("OUTPUT_AFTER_DISCONNECT",
                         (self.runner.root / "clients.json").read_text())
        self.assertFalse(list(self.runner.root.glob("terminal*.json")))

    def test_02_parallel_terminals_and_isolation(self):
        ids = []
        try:
            for n in range(4):
                created=self.call("create")
                ids.append(created["terminal_id"])
            for n, tid in enumerate(ids):
                self.call("execute", terminal_id=tid,
                          command=f"printf '%s%s' 'WORKER' '_{n}_DONE'", wait_ms=0)
            for n, tid in enumerate(ids):
                output,_=self.wait_for(tid,0,f"WORKER_{n}_DONE")
                self.assertIn(f"WORKER_{n}_DONE",output)
                for other in range(4):
                    if other != n:
                        self.assertNotIn(f"WORKER_{other}_DONE",output)
        finally:
            for tid in ids:
                self.call("close",terminal_id=tid)

    def test_03_invalid_commands_and_capacity(self):
        created=self.call("create")
        tid=created["terminal_id"]
        try:
            self.call("execute",terminal_id=tid,
                      command="printf '%s' 'multi-line-1'\nprintf '%s' 'multi-line-2'",wait_ms=0)
            with self.assertRaises(RuntimeError):
                self.call("execute",terminal_id=tid,command="X"*20000)
            with self.assertRaises(RuntimeError):
                self.call("create",rows=1,cols=1)
            with self.assertRaises(RuntimeError):
                self.call("close",terminal_id="non-existent-id")
        finally:
            self.call("close",terminal_id=tid)

    def test_04_python_cli_invocation(self):
        args=[sys.executable,str(Path(__file__).with_name("client.py")),
              "terminal","create","--state",str(self.root/"client-a.json")]
        created=json.loads(subprocess.check_output(args,text=True))
        tid=created["terminal_id"]
        try:
            executed=json.loads(subprocess.check_output([
                sys.executable,str(Path(__file__).with_name("client.py")),
                "terminal","execute","--state",str(self.root/"client-a.json"),
                "--terminal-id",tid,"--command","printf '%s%s' 'CLI' '_TERMINAL_OK'",
                "--wait-ms","200"],text=True))
            self.assertEqual(executed["type"],"terminal_executed")
            output,_=self.wait_for(tid,0,"CLI_TERMINAL_OK")
            self.assertIn("CLI_TERMINAL_OK",output)
        finally:
            subprocess.check_call([sys.executable,str(Path(__file__).with_name("client.py")),
                "terminal","close","--state",str(self.root/"client-a.json"),"--terminal-id",tid],
                stdout=subprocess.DEVNULL)

    def test_05_terminal_create_requires_authenticated_pairing(self):
        # Having a reachable TCP/TLS endpoint is NOT enough to run shell tools.
        with client.secure_socket(self.first["endpoint"], self.first["fingerprint"]) as sock:
            client.write_frame(sock, {"type": "terminal_create", "rows": 24, "cols": 80})
            reply = client.read_frame(sock)
            self.assertEqual(reply["type"], "error")
            self.assertIn("auth required", reply["message"])

    def test_99_server_restart_destroys_terminal_runtime(self):
        tid=self.call("create")["terminal_id"]
        self.call("execute",terminal_id=tid,command="sleep 30",wait_ms=0)
        self.runner.stop()
        self.runner.start()
        with self.assertRaises(RuntimeError):
            self.call("read",terminal_id=tid,offset=0)
        self.assertEqual(self.call("list")["terminals"],[])


class DisabledTerminalIntegration(unittest.TestCase):
    def test_default_requires_opt_in(self):
        with tempfile.TemporaryDirectory(prefix="ssg-no-terminal-") as dirname:
            runner=ServerRunner(Path(dirname)/"daemon")
            runner.start()
            try:
                invite,_=client.handshake_http(runner.url,PASSWORD)
                state=client.pair(invite,Path(dirname)/"client.json")
                tools=client.terminal_request(state,"tools")
                self.assertTrue(all(not t["enabled"] for t in tools["tools"]))
                with self.assertRaises(RuntimeError):
                    client.terminal_request(state,"create")
            finally:
                runner.stop()


if __name__ == "__main__":
    unittest.main(verbosity=2)