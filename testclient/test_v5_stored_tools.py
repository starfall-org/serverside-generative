"""Stored tool execution goes through v5 HTTP and keeps credentials out of chat."""
import http.server
import json
from pathlib import Path
import subprocess
import tempfile
import threading
import time
import unittest

import client
from test_integration import BINARY, PASSWORD, ServerRunner
from test_v5_security import request

SECRET = 'stored-tool-integration-secret-871592'


class Echo(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        body = json.dumps({'echo': self.headers.get('X-Key', '')}).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class StoredToolIntegration(unittest.TestCase):
    def test_stored_tool_grant_always_persisted_and_echo_guard(self):
        upstream = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Echo)
        threading.Thread(target=upstream.serve_forever, daemon=True).start()
        try:
            with tempfile.TemporaryDirectory(prefix='ssg-stored-tool-') as temp:
                base = Path(temp)
                server = ServerRunner(base / 'identity', allow_local_api=True)
                server.start()
                try:
                    invitation, _ = client.handshake_http(server.url, PASSWORD)
                    paired = client.pair(invitation, base / 'client.json')
                    _, auth = request(server.url + '/v1/auth', {'client_id': paired['client_id'], 'secret': paired['secret']})
                    token = auth['token']
                    file = base / 'tool.json'
                    file.write_text(json.dumps({'id': 't', 'name': 'tool', 'enabled': True, 'url': f'http://127.0.0.1:{upstream.server_port}', 'headers': {'X-Key': SECRET}, 'parameters': {'type': 'object', 'properties': {}}}))
                    subprocess.run([str(BINARY), 'tool', 'add', '--file', str(file), '--data-dir', str(server.root / 'data')], check=True, capture_output=True)
                    def rpc(req_id, op, sid, data):
                        return request(server.url + '/v1/rpc', {'id': req_id, 'op': op, 'session_id': sid, 'data': data}, token)
                    for mode in ('memory', 'sync'):
                        sid = 'stored-' + mode
                        conversation = {'id': sid, 'title': 'test', 'created_at': 1, 'updated_at': 2, 'messages': []}
                        runtime = {'source': 'stored', 'toolGrants': {'custom_http': {'t': True}}}
                        self.assertTrue(rpc('open-' + mode, 'session.open', sid, {'storage': mode, 'conversation': conversation, 'runtime': runtime})[1]['ok'])
                        self.assertTrue(rpc('call-' + mode, 'tool.call', sid, {'kind': 'custom_http', 'tool_id': 't', 'arguments': {}})[1]['ok'])
                        deadline = time.monotonic() + 8
                        cursor = 0
                        blocked = False
                        while not blocked and time.monotonic() < deadline:
                            status, page = request(f'{server.url}/v1/events?session_id={sid}&after={cursor}', token=token)
                            self.assertEqual(status, 200)
                            self.assertNotIn(SECRET, json.dumps(page))
                            for event in page['events']:
                                cursor = event['seq']
                                if event['kind'] == 'error':
                                    self.assertEqual(event['data']['code'], 'secret_leak_blocked')
                                    blocked = True
                        self.assertTrue(blocked)
                        self.assertTrue(list((server.root / 'data' / 'chats').glob('*.json')))
                    for path in server.root.rglob('*'):
                        if path.is_file() and not path.is_symlink():
                            self.assertNotIn(SECRET.encode(), path.read_bytes(), str(path))
                finally:
                    server.stop()
        finally:
            upstream.shutdown()
            upstream.server_close()
