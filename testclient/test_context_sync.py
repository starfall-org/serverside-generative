#!/usr/bin/env python3
"""Cross-client shared configuration and optimistic concurrency tests."""
from __future__ import annotations
import json
from pathlib import Path
import tempfile
import unittest
import client
from test_integration import PASSWORD, ServerRunner

class SharedContextIntegration(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp=tempfile.TemporaryDirectory(prefix="serverside-shared-")
        cls.root=Path(cls.temp.name)
        cls.runtime=ServerRunner(cls.root/"server")
        cls.runtime.start()

    @classmethod
    def tearDownClass(cls):
        cls.runtime.stop()
        cls.temp.cleanup()

    def pair_device(self, file: Path | None = None):
        invitation,_=client.handshake_http(self.runtime.url,PASSWORD)
        return client.pair(invitation,self.root/f"client-{len(list(self.root.glob('client-*.json')))}.json",file)

    def test_01_devices_read_single_shared_config(self):
        path=self.root/"settings1.json"
        original={"instructions":"Share across every device","skills":[{"id":"abc","name":"ABC","content":"# ABC"}],
                  "providers":[{"id":"provider","name":"Provider","platform":"openai-chat",
                                "base_url":"https://api.example.com/v1","models":["m1","m2"]}]}
        path.write_text(json.dumps(original))
        a=self.pair_device(path)
        b=self.pair_device()
        with client.reconnect(b) as sock:
            client.write_frame(sock,{"type":"context_get"})
            response=client.read_frame(sock)
            self.assertEqual(response["type"],"context_state")
            self.assertEqual(response["context"]["instructions"],original["instructions"])
            self.assertEqual(response["context"]["providers"][0]["models"],["m1","m2"])
        self.assertEqual(a["last_shared_revision"],1)
        self.assertEqual(b["last_shared_revision"],1)
        self.assertEqual((self.runtime.root/"shared-config.json").stat().st_mode & 0o777,0o600)

    def test_02_second_device_updates_first_without_overwriting(self):
        a=self.pair_device()
        b=self.pair_device()
        # Each client has a different paired identity but shares one revision.
        with client.reconnect(a) as sock:
            client.write_frame(sock,{"type":"context_get"})
            data=client.read_frame(sock)
        new={"instructions":"Written by device B","skills":[],"providers":[]}
        with client.reconnect(b) as sock:
            client.write_frame(sock,{"type":"context_update","revision":data["revision"],"context":new})
            committed=client.read_frame(sock)
            self.assertEqual(committed["type"],"context_ack")
        with client.reconnect(a) as sock:
            client.write_frame(sock,{"type":"context_get"})
            current=client.read_frame(sock)
            self.assertEqual(current["context"]["instructions"],"Written by device B")
            self.assertGreater(current["revision"],data["revision"])
        with client.reconnect(a) as sock:
            client.write_frame(sock,{"type":"context_update","revision":data["revision"],
                                     "context":{"instructions":"stale","skills":[]}})
            rejected=client.read_frame(sock)
            self.assertEqual(rejected["type"],"error")
            self.assertIn("revision conflict",rejected["message"])

    def test_03_shared_configuration_survives_restart(self):
        a=self.pair_device()
        with client.reconnect(a) as sock:
            client.write_frame(sock,{"type":"context_get"})
            snapshot=client.read_frame(sock)
        self.runtime.stop()
        self.runtime.start()
        with client.reconnect(a) as sock:
            client.write_frame(sock,{"type":"context_get"})
            restored=client.read_frame(sock)
            self.assertEqual(restored["revision"],snapshot["revision"])
            self.assertEqual(restored["context"],snapshot["context"])
        self.assertEqual(len(list(self.runtime.root.glob("shared-config.json"))),1)

    def test_04_client_cannot_write_keys_into_shared_schema(self):
        a=self.pair_device()
        with client.reconnect(a) as sock:
            client.write_frame(sock,{"type":"context_get"})
            before=client.read_frame(sock)
            client.write_frame(sock,{"type":"context_update","revision":before["revision"],
                                     "context":{"instructions":"bad","skills":[],"api_key":"unwanted"}})
            failure=client.read_frame(sock)
            self.assertEqual(failure["type"],"error")
        self.assertNotIn("unwanted",(self.runtime.root/"shared-config.json").read_text())

    def test_05_local_updates_pull_and_conflict_detection(self):
        reader = self.pair_device()
        with client.reconnect(reader) as sock:
            client.write_frame(sock, {"type":"context_get"})
            initial = client.read_frame(sock)
        file = self.root / "shared-device-local.json"
        client.save_state(file,initial["context"])
        writer = self.pair_device(file)
        remote = self.pair_device()
        # Another paired device changes the server's authoritative copy.
        with client.reconnect(remote) as sock:
            client.write_frame(sock, {"type":"context_get"})
            before = client.read_frame(sock)
            client.write_frame(sock, {"type":"context_update","revision":before["revision"],
                                      "context":{"instructions":"From remote","skills":[]}})
            ack = client.read_frame(sock)
            self.assertEqual(ack["type"],"context_ack")
        # Unmodified local file adopts remote state automatically.
        with client.reconnect(writer):
            pass
        self.assertEqual(json.loads(file.read_text())["instructions"],"From remote")
        # Local edit since last sync is pushed with the correct revision.
        local = json.loads(file.read_text())
        local["instructions"] = "Edited on device"
        file.write_text(json.dumps(local))
        with client.reconnect(writer):
            pass
        with client.reconnect(remote) as sock:
            client.write_frame(sock,{"type":"context_get"})
            updated = client.read_frame(sock)
            self.assertEqual(updated["context"]["instructions"],"Edited on device")
            # Make a remote edit without updating writer's known revision.
            client.write_frame(sock,{"type":"context_update","revision":updated["revision"],
                                     "context":{"instructions":"Concurrent remote edit","skills":[]}})
            self.assertEqual(client.read_frame(sock)["type"],"context_ack")
        conflicting = json.loads(file.read_text())
        conflicting["instructions"] = "Concurrent local edit"
        file.write_text(json.dumps(conflicting))
        with self.assertRaises(RuntimeError):
            client.reconnect(writer)
        self.assertEqual(json.loads(file.read_text())["instructions"],"Concurrent local edit")

    def test_05_no_context_is_read_only_and_conflict_is_explicit(self):
        a=self.pair_device()
        with client.reconnect(a) as sock:
            client.write_frame(sock,{"type":"context_get"})
            current=client.read_frame(sock)
        file=self.root/"new-device-config.json"
        file.write_text(json.dumps({"instructions":"different local config","skills":[]}))
        invitation,_=client.handshake_http(self.runtime.url,PASSWORD)
        with self.assertRaises(RuntimeError):
            client.pair(invitation,self.root/"conflicting-new-client.json",file)
        with client.reconnect(a) as sock:
            client.write_frame(sock,{"type":"context_get"})
            unchanged=client.read_frame(sock)
        self.assertEqual(unchanged["revision"],current["revision"])

if __name__=="__main__":
    unittest.main(verbosity=2)