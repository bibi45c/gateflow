"""Helpers shared by the operator scripts in deploy/aws.

Standard library only. Every external command goes through an injectable runner with
the signature of subprocess.run, so the scripts can be tested without aws or kubectl.

Note: deploy/aws/secrets.py shadows the standard library module of the same name for
every script run from this directory, so no script here may import `secrets`; random
values come from os.urandom instead.
"""
import base64
import json
import os
from pathlib import Path
import re
import tempfile

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
REGION = 'ap-southeast-1'
NAME_PREFIX = 'gateflow-s3'
PROJECT = 'gateflow'
ENVIRONMENT = 's3'
# The user and password of a database URL, and a password parameter, which no output may carry.
URL_CREDENTIALS = re.compile(r'(postgres(?:ql)?://)\S*@', re.IGNORECASE)
URL_PASSWORD = re.compile(r'(password=)[^&\s\'"]*', re.IGNORECASE)


class OperatorError(Exception):
    """A failure the script reports in one line and exits on."""


def random_token(size=24):
    """size random bytes as lowercase hex, like secrets.token_hex(size); safe inside URLs."""
    return os.urandom(size).hex()


def redact_urls(text):
    """text without the user and password of any postgres:// or postgresql:// URL."""
    return URL_PASSWORD.sub(r'\1<redacted>', URL_CREDENTIALS.sub(r'\1<redacted>@', text))


class Redactor:
    """Removes known secret values, and their base64 forms, from text before it is printed."""

    def __init__(self):
        self.values = set()

    def add(self, *values):
        for value in values:
            if isinstance(value, str) and len(value) >= 4:
                self.values.add(value)
                self.values.add(base64.b64encode(value.encode()).decode())

    def __call__(self, text):
        text = str(text)
        for value in sorted(self.values, key=len, reverse=True):
            text = text.replace(value, '<redacted>')
        return redact_urls(text)


def read_json_file(path):
    """Parses a JSON file written by any shell: UTF-8 with or without BOM, or the UTF-16
    that Windows PowerShell 5.1 writes with `>`."""
    data = Path(path).read_bytes()
    if data.startswith((b'\xff\xfe', b'\xfe\xff')):
        text = data.decode('utf-16')
    else:
        text = data.decode('utf-8-sig')
    return json.loads(text)


def terraform_outputs(path, required):
    """The values of `terraform output -json`: each output is {"value": ..., "type": ...,
    "sensitive": ...}. Raises OperatorError naming any required output that is missing or null."""
    try:
        document = read_json_file(path)
    except (OSError, ValueError) as error:
        raise OperatorError(f'cannot read terraform output file {path}: {type(error).__name__}') from error
    if not isinstance(document, dict):
        raise OperatorError('terraform output file is not the JSON object of `terraform output -json`')
    values = {}
    for name, output in document.items():
        if not isinstance(output, dict) or 'value' not in output:
            raise OperatorError(f'terraform output {name!r} has no "value"; pass the file of `terraform output -json`')
        values[name] = output['value']
    missing = [name for name in required if values.get(name) in (None, '', [])]
    if missing:
        raise OperatorError(f'terraform output lacks {", ".join(missing)}')
    return values


def inside_repository(path):
    try:
        Path(path).resolve().relative_to(ROOT)
    except ValueError:
        return False
    return True


def require_ignored(path, runner, what):
    """Refuses a path inside the repository that git does not ignore, so local secrets or
    account identifiers can never be committed by accident. Paths outside are allowed."""
    if not inside_repository(path):
        return
    resolved = Path(path).resolve()
    try:
        result = runner(['git', '-C', str(ROOT), 'check-ignore', '-q', '--', str(resolved)],
                        capture_output=True, text=True, encoding='utf-8')
    except OSError as error:
        raise OperatorError(f'cannot run git to check that the {what} path is ignored: {error.strerror}') from error
    if result.returncode == 1:
        relative = resolved.relative_to(ROOT).as_posix()
        raise OperatorError(f'{what} path {relative} is inside the repository and not ignored by git; '
                            'use a path under .local/ or outside the repository')
    if result.returncode:
        raise OperatorError(f'git check-ignore exited with {result.returncode}; '
                            f'cannot confirm that the {what} path is ignored')


def write_private(path, text):
    """Writes text atomically, readable only by the owner where the platform supports it."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    handle, temporary = tempfile.mkstemp(dir=path.parent, prefix=f'.{path.name}.', suffix='.tmp')
    try:
        with os.fdopen(handle, 'w', encoding='utf-8', newline='\n') as stream:
            stream.write(text)
        if os.name != 'nt':
            os.chmod(temporary, 0o600)
        os.replace(temporary, path)
    except BaseException:
        Path(temporary).unlink(missing_ok=True)
        raise


def kubectl(kubeconfig, context, namespace=None):
    """kubectl with an explicit kubeconfig and context; never the default kubeconfig."""
    command = ['kubectl', '--kubeconfig', str(kubeconfig), '--context', context]
    return command + ['-n', namespace] if namespace else command


def command_name(command):
    """Program and subcommand only; the other arguments can carry local paths."""
    words = iter(command[1:])
    for word in words:
        if word.startswith('-'):
            if '=' not in word:
                next(words, None)  # every option before the subcommand takes a value
            continue
        return f'{command[0]} {word}'
    return command[0]


def run(runner, command, redact=redact_urls, input=None, ok=(0,)):
    """Runs command and returns the CompletedProcess; raises OperatorError, with the last
    lines of stderr redacted, when it cannot start or exits with a code outside ok."""
    try:
        result = runner(command, capture_output=True, text=True, encoding='utf-8', errors='replace', input=input)
    except OSError as error:
        raise OperatorError(f'{command_name(command)} could not start: {error.strerror}') from error
    if result.returncode not in ok:
        stderr = '\n'.join((result.stderr or '').strip().splitlines()[-3:])
        raise OperatorError(redact(f'{command_name(command)} exited with {result.returncode}: {stderr}'))
    return result


def capture(runner, command, redact=redact_urls, input=None):
    return run(runner, command, redact, input).stdout
