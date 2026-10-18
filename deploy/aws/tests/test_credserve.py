import contextlib
from datetime import datetime, timezone
import io
import json
from pathlib import Path
import tempfile
import threading
import unittest
import urllib.error
import urllib.request

from loader import AWS, completed, load

credserve = load('credserve')

KEY_ID = 'ASIAEXAMPLEKEYID0000'
SECRET = 'secret-access-key-must-not-leak'
SESSION = 'session-token-must-not-leak'
EXPORT = json.dumps({'Version': 1, 'AccessKeyId': KEY_ID, 'SecretAccessKey': SECRET, 'SessionToken': SESSION,
                     'Expiration': '2026-10-19T10:15:00+00:00'})
TOKEN = 'f' * 32
# No proxy: the environment's proxy settings must not reroute 127.0.0.1.
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def fixed_now():
    return datetime(2026, 10, 19, 10, 0, 0, tzinfo=timezone.utc)


class ServerTests(unittest.TestCase):
    def start(self, export):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.log = Path(directory.name)/'credserve.log'
        server = credserve.make_server(0, TOKEN, export, self.log, now=fixed_now)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        self.server = server
        return f'http://127.0.0.1:{server.server_address[1]}'

    def get(self, url):
        try:
            with OPENER.open(url, timeout=10) as response:
                return response.status, response.read().decode('utf-8')
        except urllib.error.HTTPError as error:
            with error:
                return error.code, error.read().decode('utf-8', 'replace')

    def test_listens_on_loopback_only(self):
        self.start(lambda: (EXPORT, None))
        self.assertEqual(self.server.server_address[0], '127.0.0.1')

    def test_token_path_serves_the_export_and_logs_only_the_expiry(self):
        base = self.start(lambda: (EXPORT, None))
        status, body = self.get(f'{base}/{TOKEN}')
        self.assertEqual((status, json.loads(body)['AccessKeyId']), (200, KEY_ID))
        log = self.log.read_text(encoding='utf-8')
        self.assertEqual(log, '2026-10-19T10:00:00Z served, expires 2026-10-19T10:15:00+00:00\n')
        for value in (KEY_ID, SECRET, SESSION, TOKEN):
            self.assertNotIn(value, log)

    def test_any_other_path_is_refused_without_exporting(self):
        calls = []
        base = self.start(lambda: calls.append(1) or (EXPORT, None))
        for path in ('/', '/' + TOKEN[:-1], f'/{TOKEN}/x', f'/{TOKEN}?a=1'):
            status, body = self.get(base + path)
            self.assertEqual(status, 404, path)
            self.assertNotIn(SECRET, body)
        self.assertEqual(calls, [])
        log = self.log.read_text(encoding='utf-8')
        self.assertEqual(log.count('refused: unknown path'), 4)
        self.assertNotIn(TOKEN[:-1], log)

    def test_failed_export_is_500_and_logged(self):
        base = self.start(lambda: (None, 'aws configure export-credentials exited with 255'))
        self.assertEqual(self.get(f'{base}/{TOKEN}')[0], 500)
        self.assertIn('failed: aws configure export-credentials exited with 255', self.log.read_text(encoding='utf-8'))

    def test_garbled_export_is_not_served(self):
        base = self.start(lambda: (f'not json {SECRET}', None))
        status, body = self.get(f'{base}/{TOKEN}')
        self.assertEqual(status, 500)
        self.assertNotIn(SECRET, body + self.log.read_text(encoding='utf-8'))


class ExporterTests(unittest.TestCase):
    def test_runs_export_credentials_with_the_profile(self):
        commands = []

        def runner(command, **kwargs):
            commands.append(command)
            return completed(command, 0, EXPORT)
        self.assertEqual(credserve.exporter(runner, 'ops')(), (EXPORT, None))
        self.assertEqual(commands, [['aws', 'configure', 'export-credentials', '--format', 'process', '--profile', 'ops']])

    def test_failure_reports_the_exit_code_not_the_output(self):
        body, problem = credserve.exporter(lambda c, **k: completed(c, 255, stderr=f'expired {SECRET}'))()
        self.assertIsNone(body)
        self.assertEqual(problem, 'aws configure export-credentials exited with 255')


class MainTests(unittest.TestCase):
    def test_writes_a_new_token_and_never_prints_it(self):
        with tempfile.TemporaryDirectory() as directory:
            token_file = Path(directory)/'credserve.token'
            served = []
            stdout = io.StringIO()
            with contextlib.redirect_stdout(stdout):
                code = credserve.main(['--token-file', str(token_file), '--port', '0'],
                                      runner=lambda c, **k: completed(c, 0, EXPORT), serve=served.append)
            token = token_file.read_text(encoding='utf-8')
            self.assertEqual(code, 0)
            self.assertRegex(token, r'^[0-9a-f]{32}$')
            self.assertNotIn(token, stdout.getvalue())
            self.assertEqual(served[0].server_address[0], '127.0.0.1')
            self.assertIn(str(Path(directory)/'credserve.log'), stdout.getvalue())

    def test_token_file_inside_the_repository_must_be_ignored(self):
        token_file = AWS/'credserve.token'
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr), contextlib.redirect_stdout(io.StringIO()):
            # git check-ignore exits 1: the path is not ignored.
            code = credserve.main(['--token-file', str(token_file)], runner=lambda c, **k: completed(c, 1),
                                  serve=lambda s: None)
        self.assertEqual(code, 2)
        self.assertIn('not ignored by git', stderr.getvalue())
        self.assertFalse(token_file.exists())


if __name__ == '__main__':
    unittest.main()
