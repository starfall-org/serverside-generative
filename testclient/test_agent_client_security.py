#!/usr/bin/env python3
"""Security tests for agent-client state file handling (no server required)."""
import json
import os
from pathlib import Path
import tempfile
import unittest

from agent_client import load_trusted_state


class StatePathSecurityTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.state = self.directory / "client-state.json"
        self.state.write_text(json.dumps({"client_id": "local-test", "secret": "private"}), encoding="utf-8")
        self.state.chmod(0o600)

    def test_valid_private_state(self):
        path, state = load_trusted_state(self.state)
        self.assertEqual(path, self.state.resolve())
        self.assertEqual(state["client_id"], "local-test")

    def test_reject_parent_directory_traversal(self):
        with self.assertRaises(ValueError):
            load_trusted_state(self.directory / ".." / self.directory.name / self.state.name)

    def test_reject_symlink(self):
        link = self.directory / "alias.json"
        link.symlink_to(self.state)
        with self.assertRaises(ValueError):
            load_trusted_state(link)

    def test_reject_shared_secret_file(self):
        self.state.chmod(0o644)
        with self.assertRaises(PermissionError):
            load_trusted_state(self.state)

    def test_reject_large_state_file(self):
        self.state.write_bytes(b" " * 65537)
        with self.assertRaises(ValueError):
            load_trusted_state(self.state)

    def test_reject_non_object(self):
        self.state.write_text("[]", encoding="utf-8")
        with self.assertRaises(ValueError):
            load_trusted_state(self.state)

    def test_reject_regular_file_outside_roots(self):
        # /etc/hosts cannot be used as a user-controlled credential state.
        host_path = Path("/etc/hosts")
        if host_path.exists():
            with self.assertRaises(ValueError):
                load_trusted_state(host_path)


if __name__ == "__main__":
    unittest.main()
