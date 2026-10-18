"""Serve fresh AWS credentials to a Terraform container as credential_process output.

Operator-run (bibi45c only); see deploy/terraform/credential-bridge.md.

    python deploy/aws/credserve.py --token-file .local/cloud-day/credserve.token [--port 17651]

Writes a new random token to --token-file (readable only by the owner where the
platform supports it; inside the repository the path must be ignored by git) and
listens on 127.0.0.1 only. Each GET /<token> runs
`aws configure export-credentials --format process` and returns its JSON, so the SDK
in the container fetches new credentials by itself whenever the short-lived ones of
`aws login` expire. Any other path gets 404, a failed export 500.

Each request is logged to --log (default: credserve.log next to the token file) as its
time, its outcome and the credentials' expiry: never a credential value, the token or
the request path. Stop it with Ctrl-C.
"""
import argparse
from datetime import datetime, timezone
import hmac
import http.server
import json
from pathlib import Path
import subprocess
import sys
import threading

from common import OperatorError, random_token, require_ignored, write_private

HOST = '127.0.0.1'
DEFAULT_PORT = 17651


def exporter(runner, profile=None):
    """Returns export() -> (credential_process JSON text, None) or (None, the reason it failed)."""
    command = ['aws', 'configure', 'export-credentials', '--format', 'process']
    if profile:
        command += ['--profile', profile]

    def export():
        try:
            result = runner(command, capture_output=True, text=True, encoding='utf-8', errors='replace')
        except OSError as error:
            return None, f'aws could not start: {error.strerror}'
        if result.returncode:
            return None, f'aws configure export-credentials exited with {result.returncode}'
        return result.stdout, None
    return export


def utc_now():
    return datetime.now(timezone.utc)


def make_server(port, token, export, log, now=utc_now):
    """A server on 127.0.0.1:port; port 0 picks a free one."""
    lock = threading.Lock()
    expected = f'/{token}'.encode()

    def record(outcome):
        with lock, open(log, 'a', encoding='utf-8') as stream:
            stream.write(f'{now():%Y-%m-%dT%H:%M:%SZ} {outcome}\n')

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            if not hmac.compare_digest(self.path.encode(), expected):
                record('refused: unknown path')
                self.send_error(404)
                return
            body, problem = export()
            expiry = None
            if body is not None:
                try:
                    document = json.loads(body)
                    expiry = document.get('Expiration', '?')
                except (ValueError, AttributeError):
                    body, problem = None, 'the export is not a JSON object'
            if body is None:
                record(f'failed: {problem}')
                self.send_error(500)
                return
            record(f'served, expires {expiry}')
            data = body.encode('utf-8')
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *args):
            pass  # the default request log would print the path, and the path is the token

    return http.server.ThreadingHTTPServer((HOST, port), Handler)


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--token-file', type=Path, required=True, help='file for the new token; must be ignored by git')
    parser.add_argument('--port', type=int, default=DEFAULT_PORT, help=f'port on 127.0.0.1 (default: {DEFAULT_PORT})')
    parser.add_argument('--log', type=Path, help='request log (default: credserve.log next to the token file)')
    parser.add_argument('--profile', help='AWS CLI profile to export (default: the CLI default)')
    args = parser.parse_args(argv)
    if not 0 <= args.port <= 65535:
        parser.error('--port must be 0 to 65535')
    return args


def main(argv=None, runner=subprocess.run, serve=None):
    args = parse_args(argv)
    log = args.log or args.token_file.with_name('credserve.log')
    try:
        require_ignored(args.token_file, runner, 'token')
        token = random_token(16)
        write_private(args.token_file, token)
        server = make_server(args.port, token, exporter(runner, args.profile), log)
    except (OperatorError, OSError) as error:
        print(f'FAIL {error}', file=sys.stderr)
        return 2
    host, port = server.server_address[:2]
    print(f'serving credentials on http://{host}:{port}/<token>; token in {args.token_file}, log in {log}', flush=True)
    try:
        (serve or (lambda s: s.serve_forever()))(server)
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
    return 0


if __name__ == '__main__':
    sys.exit(main())
