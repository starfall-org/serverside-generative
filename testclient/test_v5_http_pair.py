"""One-time HTTP pairing URL issuance and scoped credential checks."""
import base64
import json
from pathlib import Path
import socket
import ssl
import struct
import subprocess
import tempfile
import unittest
import urllib.parse

from test_integration import BINARY, ServerRunner
from test_v5_security import request


class HttpPairIntegration(unittest.TestCase):
    def test_url_pairing_scope_replay_and_cancel(self):
        with tempfile.TemporaryDirectory(prefix='ssg-http-pair-') as temp:
            server = ServerRunner(Path(temp) / 'identity')
            server.start()
            try:
                endpoint = f'https://127.0.0.1:{server.http_port}'
                pin = 'sha256/' + base64.b64encode(b'p' * 32).decode()
                def issue():
                    output = subprocess.check_output([str(BINARY), 'pair', '--transport', 'http', '--endpoint', endpoint, '--pin', pin, '--ipc', str(server.ipc)], text=True)
                    line = next(x for x in output.splitlines() if x.startswith('Pairing URL: '))
                    url = urllib.parse.urlsplit(line.removeprefix('Pairing URL: '))
                    params = urllib.parse.parse_qs(url.fragment)
                    self.assertEqual(params['pin'], [pin])
                    self.assertEqual(url.scheme, 'https')
                    return params['code'][0]
                code = issue()
                status, paired = request(server.url + '/v1/pair', {'code': code})
                self.assertEqual(status, 200)
                self.assertEqual(paired['transports'], ['http', 'websocket'])
                self.assertEqual(paired['tls_pin'], pin)
                self.assertEqual(request(server.url + '/v1/pair', {'code': code})[0], 401)
                status, _ = request(server.url + '/v1/auth', {'client_id': paired['client_id'], 'secret': paired['secret']})
                self.assertEqual(status, 200)
                with socket.create_connection(('127.0.0.1', server.tcp_port), timeout=5) as raw:
                    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
                    context.check_hostname = False
                    context.verify_mode = ssl.CERT_NONE
                    with context.wrap_socket(raw, server_hostname='localhost') as tls:
                        payload = json.dumps({'type': 'auth', 'protocol': 5, 'client_id': paired['client_id'], 'secret': paired['secret']}).encode()
                        tls.sendall(struct.pack('>I', len(payload)) + payload)
                        def read_exact(size):
                            data = b''
                            while len(data) < size:
                                chunk = tls.recv(size - len(data))
                                if not chunk:
                                    raise EOFError('missing frame')
                                data += chunk
                            return data
                        size = struct.unpack('>I', read_exact(4))[0]
                        self.assertEqual(json.loads(read_exact(size))['type'], 'error')
                pending = issue()
                subprocess.run([str(BINARY), 'pair', '--transport', 'http', '--cancel', '--ipc', str(server.ipc)], check=True, capture_output=True)
                self.assertEqual(request(server.url + '/v1/pair', {'code': pending})[0], 401)
                for path in server.root.rglob('*'):
                    if path.is_file() and not path.is_symlink():
                        data = path.read_bytes()
                        for secret in (code, pending, paired['secret']):
                            self.assertNotIn(secret.encode(), data, str(path))
            finally:
                server.stop()
