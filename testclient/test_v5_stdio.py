"""Exercise SSH-style stdio without using the daemon TCP data endpoint."""
import http.server
import client
import json
import select
import threading
from pathlib import Path
import struct
import subprocess
import tempfile
import time
import unittest

from test_integration import BINARY, ServerRunner
from test_v5_security import Provider, SENTINEL, request


def write_frame(stream, obj):
    data = json.dumps(obj).encode()
    stream.write(struct.pack('>I', len(data)) + data)
    stream.flush()


def read_frame(stream):
    def read_exact(size):
        data = b''
        while len(data) < size:
            if not select.select([stream], [], [], 8)[0]:
                raise TimeoutError('no server frame')
            part = stream.read(size - len(data))
            if not part:
                raise EOFError('missing frame')
            data += part
        return data
    header = read_exact(4)
    size = struct.unpack('>I', header)[0]
    if not 0 < size <= 4 * 1024 * 1024:
        raise ValueError('invalid frame size')
    return json.loads(read_exact(size))


class StdioIntegration(unittest.TestCase):
    def test_stdio_commands_wait_for_daemon_sockets(self):
        with tempfile.TemporaryDirectory(prefix='gen-start-', dir=Path('/tmp').resolve()) as temp:
            root = Path(temp) / 'identity'
            server = ServerRunner(root)
            pair_cmd = [str(BINARY), 'pair', '--stdio', '--transport', 'tcp', '--ipc', str(server.ipc)]
            stream_cmd = [str(BINARY), 'stream', '--stdio', '--data-socket', str(root / 'run' / 'data.sock')]
            with subprocess.Popen(pair_cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE) as pair, \
                    subprocess.Popen(stream_cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                     stderr=subprocess.PIPE, bufsize=0) as bridge:
                try:
                    time.sleep(.15)
                    self.assertIsNone(pair.poll(), 'pair exited before the daemon started')
                    self.assertIsNone(bridge.poll(), 'stream exited before the daemon started')
                    server.start()
                    stdout, stderr = pair.communicate(timeout=5)
                    self.assertEqual(pair.returncode, 0, stderr)
                    identity = json.loads(stdout)
                    self.assertEqual(identity['transport'], 'tcp')
                    write_frame(bridge.stdin, {'type': 'auth', 'protocol': 5, 'client_id': identity['client_id'],
                                               'secret': identity['secret']})
                    self.assertEqual(read_frame(bridge.stdout)['type'], 'paired')
                    bridge.stdin.close()
                    self.assertEqual(bridge.wait(timeout=5), 0)
                finally:
                    for process in (pair, bridge):
                        if process.poll() is None:
                            process.kill()
                    server.stop()

    def test_ticket_pairing_and_memory_rpc_through_stdio(self):
        with tempfile.TemporaryDirectory(prefix='ssg-stdio-') as temp:
            root = Path(temp) / 'identity'
            server = ServerRunner(root, allow_local_api=True)
            server.start()
            try:
                invite = json.loads(subprocess.check_output([str(BINARY), 'handshake', '--stdio', '--ipc', str(server.ipc)]))
                bridge = subprocess.Popen([str(BINARY), 'stream', '--stdio', '--data-socket', str(root / 'run' / 'data.sock')], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, bufsize=0)
                try:
                    write_frame(bridge.stdin, {'type': 'auth', 'protocol': 5, 'ticket': invite['ticket']})
                    paired = read_frame(bridge.stdout)
                    self.assertEqual(paired['type'], 'paired')
                    self.assertTrue(paired['secret'])
                    conversation = {'id': 'stdio', 'title': 'hello', 'created_at': 1, 'updated_at': 2, 'messages': []}
                    write_frame(bridge.stdin, {'id': 'open', 'op': 'session.open', 'session_id': 'stdio', 'data': {'storage': 'memory', 'conversation': conversation}})
                    self.assertTrue(read_frame(bridge.stdout)['ok'])
                    write_frame(bridge.stdin, {'id': 'get', 'op': 'session.get', 'session_id': 'stdio', 'data': {}})
                    self.assertTrue(read_frame(bridge.stdout)['ok'])
                    upstream = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Provider)
                    threading.Thread(target=upstream.serve_forever, daemon=True).start()
                    try:
                        runtime = {'source': 'mobile_ephemeral', 'provider': {'type': 'openai', 'baseUrl': f'http://127.0.0.1:{upstream.server_port}', 'modelId': 'test', 'auth': {'method': 'bearer_token', 'value': SENTINEL}}}
                        write_frame(bridge.stdin, {'id': 'configure', 'op': 'session.configure', 'session_id': 'stdio', 'data': {'runtime': runtime}})
                        self.assertTrue(read_frame(bridge.stdout)['ok'])
                        write_frame(bridge.stdin, {'id': 'run', 'op': 'session.run', 'session_id': 'stdio', 'data': {'content': 'hello'}})
                        reply = read_frame(bridge.stdout)
                        self.assertEqual(reply['id'], 'run')
                        self.assertTrue(reply['ok'])
                        cursor = 0
                        completed = False
                        deadline = time.monotonic() + 8
                        while not completed and time.monotonic() < deadline:
                            write_frame(bridge.stdin, {'id': 'poll', 'op': 'session.events', 'session_id': 'stdio', 'data': {'after': cursor}})
                            reply = read_frame(bridge.stdout)
                            self.assertEqual(reply['id'], 'poll')
                            self.assertTrue(reply['ok'])
                            self.assertNotIn(SENTINEL, json.dumps(reply))
                            for event in reply['data']:
                                cursor = event['seq']
                                if event['kind'] == 'error':
                                    self.assertEqual(event['data']['code'], 'secret_leak_blocked')
                                    completed = True
                            if not completed:
                                time.sleep(.05)
                        self.assertTrue(completed, 'generation did not emit its terminal event')
                    finally:
                        upstream.shutdown()
                        upstream.server_close()
                    self.assertTrue(list((root / 'data' / 'chats').glob('*.json')))
                    for stored in (root / 'data' / 'chats').glob('*.json'):
                        self.assertNotIn(SENTINEL, stored.read_text())
                    bridge.stdin.close()
                    self.assertEqual(bridge.wait(timeout=5), 0, bridge.stderr.read())
                finally:
                    if bridge.poll() is None:
                        bridge.kill()
                        bridge.wait()
                    bridge.stdout.close()
                    bridge.stderr.close()
            finally:
                server.stop()

class RealSshIntegration(unittest.TestCase):
    def test_pinned_ssh_pairing_and_rejected_keys(self):
        import getpass
        import shutil
        import socket
        from test_integration import open_port

        sshd = '/usr/sbin/sshd'
        if not Path(sshd).is_file() or not shutil.which('ssh') or not shutil.which('ssh-keygen'):
            self.skipTest('OpenSSH server and client are required')
        with tempfile.TemporaryDirectory(prefix='ssg-real-ssh-') as temp:
            root = Path(temp)
            for name in ('host', 'client', 'wrong'):
                subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(root / name)], check=True)
            (root / 'authorized').write_text((root / 'client.pub').read_text())
            port = open_port()
            (root / 'sshd_config').write_text(
                f'Port {port}\nListenAddress 127.0.0.1\nHostKey {root}/host\nPidFile {root}/pid\n'
                f'AuthorizedKeysFile {root}/authorized\nStrictModes no\nUsePAM no\n'
                'PasswordAuthentication no\nKbdInteractiveAuthentication no\nLogLevel ERROR\n'
            )
            daemon = subprocess.Popen([sshd, '-D', '-e', '-f', str(root / 'sshd_config')], stderr=subprocess.PIPE)
            server = ServerRunner(root / 'identity', allow_local_api=True)
            try:
                deadline = time.monotonic() + 5
                while time.monotonic() < deadline:
                    if daemon.poll() is not None:
                        self.skipTest('OpenSSH daemon cannot start in this environment')
                    try:
                        with socket.create_connection(('127.0.0.1', port), timeout=.1):
                            break
                    except OSError:
                        time.sleep(.05)
                else:
                    self.fail('OpenSSH daemon did not start')
                host = (root / 'host.pub').read_text().split()
                (root / 'known').write_text(f'[127.0.0.1]:{port} {host[0]} {host[1]}\n')
                wrong = (root / 'wrong.pub').read_text().split()
                (root / 'wrong_known').write_text(f'[127.0.0.1]:{port} {wrong[0]} {wrong[1]}\n')
                def ssh(key, known):
                    return ['ssh', '-T', '-p', str(port), '-i', str(root / key), '-o', 'BatchMode=yes',
                            '-o', 'IdentitiesOnly=yes', '-o', 'StrictHostKeyChecking=yes',
                            '-o', f'UserKnownHostsFile={root / known}', '-o', 'GlobalKnownHostsFile=/dev/null',
                            '-o', 'ConnectTimeout=5', f'{getpass.getuser()}@127.0.0.1']
                self.assertNotEqual(subprocess.run(ssh('client', 'wrong_known') + ['true'], capture_output=True, timeout=8).returncode, 0)
                self.assertNotEqual(subprocess.run(ssh('wrong', 'known') + ['true'], capture_output=True, timeout=8).returncode, 0)
                server.start()
                paired = json.loads(subprocess.check_output(ssh('client', 'known') + [str(BINARY), 'pair', '--stdio', '--transport', 'tcp', '--ipc', str(server.ipc)], timeout=8))
                self.assertEqual(paired['transport'], 'tcp')
                status, _ = request(server.url + '/v1/auth', {'client_id': paired['client_id'], 'secret': paired['secret']})
                self.assertEqual(status, 401)
                release = threading.Event()
                calls = []
                class StreamingProvider(http.server.BaseHTTPRequestHandler):
                    def log_message(self, *_):
                        pass
                    def do_POST(self):
                        self.rfile.read(int(self.headers.get('Content-Length', '0')))
                        calls.append(1)
                        self.send_response(200)
                        self.send_header('Content-Type', 'text/event-stream')
                        self.end_headers()
                        def chunk(text):
                            value = {'id':'x', 'object':'chat.completion.chunk', 'choices':[{'index':0, 'delta':{'content':text}}]}
                            self.wfile.write(('data: ' + json.dumps(value) + '\n\n').encode())
                            self.wfile.flush()
                        chunk('first output segment with enough padding')
                        if release.wait(8):
                            chunk(' final segment')
                            self.wfile.write(b'data: [DONE]\n\n')
                            self.wfile.flush()
                upstream = http.server.ThreadingHTTPServer(('127.0.0.1', 0), StreamingProvider)
                upstream_thread = threading.Thread(target=upstream.serve_forever, daemon=True)
                upstream_thread.start()
                try:
                    with client.secure_socket(paired['endpoint'], paired['fingerprint']) as tcp:
                        client.write_frame(tcp, {'type':'auth', 'protocol':5, 'client_id':paired['client_id'], 'secret':paired['secret']})
                        self.assertEqual(client.read_frame(tcp)['type'], 'paired')
                        conversation = {'id':'fallback', 'title':'test', 'created_at':1, 'updated_at':2, 'messages':[]}
                        runtime = {'source':'mobile_ephemeral', 'provider':{'type':'openai', 'baseUrl':f'http://127.0.0.1:{upstream.server_port}', 'modelId':'test', 'auth':{'method':'bearer_token', 'value':'secret'}}}
                        client.write_frame(tcp, {'id':'open', 'op':'session.open', 'session_id':'fallback', 'data':{'storage':'memory', 'conversation':conversation, 'runtime':runtime}})
                        self.assertTrue(client.read_frame(tcp)['ok'])
                        run = {'id':'run', 'op':'session.run', 'session_id':'fallback', 'data':{'content':'hello'}}
                        client.write_frame(tcp, run)
                        while True:
                            event = client.read_frame(tcp)
                            if event.get('kind') == 'delta':
                                cursor = event['seq']
                                break
                    fallback = subprocess.Popen(ssh('client', 'known') + [str(BINARY), 'stream', '--stdio', '--data-socket', str(server.root / 'run' / 'data.sock')], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, bufsize=0)
                    try:
                        write_frame(fallback.stdin, {'type':'auth', 'protocol':5, 'client_id':paired['client_id'], 'secret':paired['secret']})
                        self.assertEqual(read_frame(fallback.stdout)['type'], 'paired')
                        write_frame(fallback.stdin, {'id':'reopen', 'op':'session.open', 'session_id':'fallback', 'data':{'storage':'memory', 'after':cursor}})
                        self.assertTrue(read_frame(fallback.stdout)['ok'])
                        write_frame(fallback.stdin, run)
                        self.assertTrue(read_frame(fallback.stdout)['ok'])
                        release.set()
                        while True:
                            event = read_frame(fallback.stdout)
                            if 'seq' in event:
                                self.assertGreater(event['seq'], cursor)
                                cursor = event['seq']
                            self.assertNotIn(event.get('kind'), ('error', 'event_gap'))
                            if event.get('kind') == 'completed':
                                break
                        self.assertEqual(len(calls), 1)
                        fallback.stdin.close()
                        self.assertEqual(fallback.wait(timeout=8), 0, fallback.stderr.read())
                    finally:
                        if fallback.poll() is None:
                            fallback.kill()
                            fallback.wait()
                        fallback.stdout.close()
                        fallback.stderr.close()
                finally:
                    release.set()
                    upstream.shutdown()
                    upstream.server_close()
                    upstream_thread.join(timeout=3)
                bridge = subprocess.Popen(ssh('client', 'known') + [str(BINARY), 'stream', '--stdio', '--data-socket', str(server.root / 'run' / 'data.sock')], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, bufsize=0)
                try:
                    write_frame(bridge.stdin, {'type': 'auth', 'protocol': 5, 'client_id': paired['client_id'], 'secret': paired['secret']})
                    authenticated = read_frame(bridge.stdout)
                    self.assertEqual(authenticated['type'], 'paired')
                    self.assertNotIn('secret', authenticated)
                    conversation = {'id': 'real-ssh', 'title': 'test', 'created_at': 1, 'updated_at': 2, 'messages': []}
                    write_frame(bridge.stdin, {'id': 'open', 'op': 'session.open', 'session_id': 'real-ssh', 'data': {'storage': 'memory', 'conversation': conversation}})
                    self.assertTrue(read_frame(bridge.stdout)['ok'])
                    fixture_path = Path(__file__).resolve().parent.parent / 'internal' / 'compat' / 'testdata' / 'conversation-server.json'
                    fixture = json.loads(fixture_path.read_text())
                    write_frame(bridge.stdin, {'id':'fixture-open', 'op':'session.open', 'session_id':fixture['id'], 'data':{'storage':'memory', 'conversation':fixture}})
                    self.assertEqual(read_frame(bridge.stdout)['data']['conversation'], fixture)
                    write_frame(bridge.stdin, {'id':'fixture-get', 'op':'session.get', 'session_id':fixture['id'], 'data':{}})
                    self.assertEqual(read_frame(bridge.stdout)['data']['conversation'], fixture)
                    bridge.stdin.close()
                    self.assertEqual(bridge.wait(timeout=8), 0, bridge.stderr.read())
                    self.assertGreaterEqual(len(list((server.root / 'data' / 'chats').glob('*.json'))), 2)
                finally:
                    if bridge.poll() is None:
                        bridge.kill()
                        bridge.wait()
                    bridge.stdout.close()
                    bridge.stderr.close()
            finally:
                if server.process is not None:
                    server.stop()
                if daemon.poll() is None:
                    daemon.terminate()
                    daemon.wait(timeout=5)
                daemon.stderr.close()
