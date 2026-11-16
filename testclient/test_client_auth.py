"""Authentication rejects incomplete credentials before opening a connection."""
import unittest
from unittest.mock import patch

import client


class AuthValidationTest(unittest.TestCase):
    def test_missing_invitation_proof_does_not_connect(self):
        with patch.object(client, 'secure_socket') as connect:
            with self.assertRaisesRegex(ValueError, 'Missing ticket or manual PIN'):
                client.auth_socket({'endpoint': 'localhost:1', 'fingerprint': 'a' * 64})
            connect.assert_not_called()

    def test_incomplete_credentials_do_not_connect(self):
        with patch.object(client, 'secure_socket') as connect:
            with self.assertRaises(KeyError):
                client.auth_socket({'endpoint': 'localhost:1', 'fingerprint': 'a' * 64}, {'client_id': 'id'})
            connect.assert_not_called()
