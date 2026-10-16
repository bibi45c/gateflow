import base64
import contextlib
import io
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest

from loader import AWS, completed, load

secrets_script = load('secrets')
common = sys.modules['common']

ARN = 'arn:aws:secretsmanager:ap-southeast-1:123456789012:secret:rds!db-00000000-0000-0000-0000-000000000000-AbCdEf'
HOST = 'gateflow-s3.abcdefghijkl.ap-southeast-1.rds.amazonaws.com'
OUTPUTS = {'database_address': HOST, 'database_port': 5432, 'database_name': 'gateflow',
           'master_secret_arn': ARN, 'cluster_name': 'gateflow-s3', 'vpc_id': 'vpc-0123456789abcdef0'}
URL = re.compile(r'^postgres://(access|metering):([0-9a-f]{48})@' + re.escape(HOST) + r':5432/gateflow\?sslmode=require$')


def terraform_file(directory, outputs=OUTPUTS, raw=False, encoding='utf-8'):
    document = outputs if raw else {key: {'sensitive': False, 'type': 'string', 'value': value}
                                    for key, value in outputs.items()}
    path = Path(directory)/'outputs.json'
    path.write_text(json.dumps(document), encoding=encoding)
    return path


class FakeCluster:
    """Stands in for aws, kubectl and git; Secrets live in a dict of decoded values."""

    def __init__(self, password='master-password-one', ignored=0, apply_stderr=None):
        self.password, self.ignored, self.apply_stderr = password, ignored, apply_stderr
        self.secrets, self.commands, self.inputs = {}, [], []

    def put(self, name, values, labels=None):
        self.secrets[name] = {'values': dict(values), 'labels': labels}

    def __call__(self, command, **kwargs):
        self.commands.append(list(command))
        if kwargs.get('input') is not None:
            self.inputs.append(kwargs['input'])
        if command[0] == 'git':
            return completed(command, self.ignored)
        if command[:3] == ['aws', 'secretsmanager', 'get-secret-value']:
            secret = json.dumps({'username': 'gateflow_admin', 'password': self.password})
            return completed(command, stdout=json.dumps({'ARN': ARN, 'SecretString': secret}))
        if command[0] == 'kubectl':
            verb = command[7]
            if verb == 'get':
                found = self.secrets.get(command[9])
                if not found:
                    return completed(command)
                data = {key: base64.b64encode(value.encode()).decode() for key, value in found['values'].items()}
                metadata = {'name': command[9], 'annotations': {'kubectl.kubernetes.io/last-applied-configuration': '{}'}}
                if found['labels']:
                    metadata['labels'] = found['labels']
                return completed(command, stdout=json.dumps({'kind': 'Secret', 'metadata': metadata, 'data': data}))
            if verb == 'apply':
                if self.apply_stderr:
                    return completed(command, 1, stderr=self.apply_stderr)
                document = json.loads(kwargs['input'])
                values = {key: base64.b64decode(value).decode() for key, value in document['data'].items()}
                self.put(document['metadata']['name'], values, document['metadata'].get('labels'))
                return completed(command, stdout='secret configured')
        raise AssertionError(f'unexpected command {command}')

    def applies(self):
        return [json.loads(text) for text in self.inputs]


class SecretsTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.dir = Path(directory.name)
        self.outputs = terraform_file(self.dir)
        self.state = self.dir/'state'/'secrets-state.json'

    def run_script(self, cluster, *extra, state=None):
        argv = ['--kubeconfig', str(self.dir/'kubeconfig'), '--kube-context', 'eks-test', '--terraform-output',
                str(self.outputs), '--state', str(state or self.state), *extra]
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = secrets_script.main(argv, runner=cluster)
        return code, out.getvalue(), err.getvalue()

    def state_values(self):
        return json.loads(self.state.read_text(encoding='utf-8'))

    def secret_values(self, cluster):
        values = {cluster.password}
        values.update(self.state_values().values())
        for secret in cluster.secrets.values():
            values.update(secret['values'].values())
        return {value for value in values if value not in {'require', 'gateflow', 'gateflow_admin', '5432', HOST}}

    def test_first_run_creates_both_secrets_with_every_key(self):
        cluster = FakeCluster()
        code, out, err = self.run_script(cluster)
        self.assertEqual(code, 0, err)
        bootstrap = cluster.secrets['gateflow-bootstrap']['values']
        runtime = cluster.secrets['gateflow-runtime']['values']
        self.assertEqual(set(bootstrap), set(secrets_script.BOOTSTRAP_KEYS))
        self.assertEqual(set(runtime), set(secrets_script.RUNTIME_KEYS))
        self.assertEqual((bootstrap['PGHOST'], bootstrap['PGPORT'], bootstrap['PGDATABASE']), (HOST, '5432', 'gateflow'))
        self.assertEqual((bootstrap['PGUSER'], bootstrap['PGPASSWORD'], bootstrap['PGSSLMODE']),
                         ('gateflow_admin', 'master-password-one', 'require'))
        state = self.state_values()
        self.assertEqual(set(state), set(secrets_script.STATE_KEYS))
        for role in ('access', 'metering'):
            match = URL.match(runtime[f'{role.upper()}_DATABASE_URL'])
            self.assertIsNotNone(match, role)
            self.assertEqual(match.group(1), role)
            self.assertEqual(match.group(2), state[f'{role.upper()}_PASSWORD'])
            self.assertEqual(bootstrap[f'{role.upper()}_PASSWORD'], state[f'{role.upper()}_PASSWORD'])
        for key in secrets_script.RUNTIME_KEYS[2:]:
            self.assertEqual(runtime[key], state[key])
        self.assertEqual(len(set(state.values())), len(state))
        self.assertIn('gateflow-bootstrap: applied PGHOST, PGPORT', out)
        self.assertIn('master password refreshed', out)
        manifest = cluster.applies()[0]
        self.assertEqual((manifest['kind'], manifest['type'], manifest['metadata']['namespace']),
                         ('Secret', 'Opaque', 'gateflow'))

    def test_values_travel_only_on_stdin_and_never_in_output(self):
        cluster = FakeCluster()
        code, out, err = self.run_script(cluster)
        self.assertEqual(code, 0, err)
        values = self.secret_values(cluster)
        self.assertGreaterEqual(len(values), 7)
        argv = '\n'.join(' '.join(command) for command in cluster.commands)
        for value in values:
            self.assertNotIn(value, argv)
            self.assertNotIn(value, out + err)
            self.assertNotIn(base64.b64encode(value.encode()).decode(), out + err)
        applies = [command for command in cluster.commands if 'apply' in command]
        self.assertEqual(len(applies), 2)
        for command in applies:
            self.assertEqual(command[-3:], ['apply', '-f', '-'])
            self.assertEqual(command[1:5], ['--kubeconfig', str(self.dir/'kubeconfig'), '--context', 'eks-test'])

    def test_existing_state_values_are_kept_and_only_missing_ones_generated(self):
        self.state.parent.mkdir()
        kept = {'ACCESS_PASSWORD': 'a' * 48, 'ACCESS_KEY_PEPPER': 'p' * 48, 'ACCESS_SEED_SECRET': 's' * 48}
        self.state.write_text(json.dumps(kept), encoding='utf-8')
        cluster = FakeCluster()
        code, out, err = self.run_script(cluster)
        self.assertEqual(code, 0, err)
        state = self.state_values()
        for key, value in kept.items():
            self.assertEqual(state[key], value)
        self.assertEqual(set(state), set(secrets_script.STATE_KEYS))
        self.assertIn('generated METERING_PASSWORD, ACCESS_INTERNAL_TOKEN, METERING_INTERNAL_TOKEN', out)
        self.assertEqual(cluster.secrets['gateflow-runtime']['values']['ACCESS_KEY_PEPPER'], 'p' * 48)

    def test_second_run_reuses_every_value(self):
        cluster = FakeCluster()
        self.assertEqual(self.run_script(cluster)[0], 0)
        before = (self.state.read_text(encoding='utf-8'), dict(cluster.secrets['gateflow-runtime']['values']))
        cluster.password = 'master-password-two'
        code, out, err = self.run_script(cluster)
        self.assertEqual(code, 0, err)
        self.assertEqual((self.state.read_text(encoding='utf-8'), cluster.secrets['gateflow-runtime']['values']), before)
        self.assertEqual(cluster.secrets['gateflow-bootstrap']['values']['PGPASSWORD'], 'master-password-two')
        self.assertIn('reused every value', out)

    def test_missing_state_with_existing_secret_stops_without_generating(self):
        cluster = FakeCluster()
        cluster.put('gateflow-runtime', {'ACCESS_KEY_PEPPER': 'pepper-in-cluster'})
        code, out, err = self.run_script(cluster)
        self.assertEqual(code, 1)
        self.assertIn('state file', err)
        self.assertIn('restore', err)
        self.assertFalse(self.state.exists())
        self.assertEqual(cluster.inputs, [])
        self.assertNotIn('pepper-in-cluster', out + err)

    def test_incomplete_state_with_existing_secret_stops(self):
        self.state.parent.mkdir()
        self.state.write_text(json.dumps({'ACCESS_PASSWORD': 'a' * 48}), encoding='utf-8')
        cluster = FakeCluster()
        cluster.put('gateflow-bootstrap', {'ACCESS_PASSWORD': 'a' * 48})
        code, _, err = self.run_script(cluster)
        self.assertEqual(code, 1)
        self.assertIn('lacks METERING_PASSWORD', err)
        self.assertEqual(cluster.inputs, [])
        self.assertEqual(json.loads(self.state.read_text(encoding='utf-8')), {'ACCESS_PASSWORD': 'a' * 48})

    def test_state_that_conflicts_with_the_cluster_stops(self):
        cluster = FakeCluster()
        self.assertEqual(self.run_script(cluster)[0], 0)
        state = self.state_values()
        state['METERING_PASSWORD'] = 'f' * 48
        self.state.write_text(json.dumps(state), encoding='utf-8')
        applied = len(cluster.inputs)
        code, out, err = self.run_script(cluster)
        self.assertEqual(code, 1)
        self.assertIn('does not match', err)
        self.assertIn('METERING_PASSWORD', err)
        self.assertEqual(len(cluster.inputs), applied)
        self.assertNotIn('f' * 48, out + err)

    def test_refuses_a_state_path_git_does_not_ignore(self):
        cluster = FakeCluster(ignored=1)
        inside = common.ROOT/'deploy'/'aws'/'tests'/'not-ignored-state.json'
        code, _, err = self.run_script(cluster, state=inside)
        self.assertEqual(code, 1)
        self.assertIn('not ignored by git', err)
        self.assertFalse(inside.exists())
        self.assertEqual(cluster.inputs, [])
        git = [command for command in cluster.commands if command[0] == 'git']
        self.assertEqual(git[0][3:6], ['check-ignore', '-q', '--'])

    def test_ignore_check_rules(self):
        inside = common.ROOT/'.local'/'state.json'
        for code, fails in ((0, False), (1, True), (128, True)):
            calls = []

            def runner(command, **kwargs):
                calls.append(command)
                return completed(command, code)
            if fails:
                with self.assertRaises(common.OperatorError):
                    common.require_ignored(inside, runner, 'state file')
            else:
                common.require_ignored(inside, runner, 'state file')
            self.assertEqual(len(calls), 1)
        calls = []
        common.require_ignored(self.dir/'outside.json', lambda command, **kwargs: calls.append(command), 'state file')
        self.assertEqual(calls, [])

    def test_refresh_master_only_replaces_only_pgpassword(self):
        cluster = FakeCluster()
        self.assertEqual(self.run_script(cluster)[0], 0)
        cluster.secrets['gateflow-bootstrap']['values']['EXTRA_KEY'] = 'kept-extra-value'
        cluster.secrets['gateflow-bootstrap']['labels'] = {'team': 'gateflow'}
        bootstrap = dict(cluster.secrets['gateflow-bootstrap']['values'])
        runtime = dict(cluster.secrets['gateflow-runtime']['values'])
        state = self.state.read_text(encoding='utf-8')
        cluster.password, cluster.inputs = 'master-password-rotated', []
        code, out, err = self.run_script(cluster, '--refresh-master-only')
        self.assertEqual(code, 0, err)
        self.assertEqual(len(cluster.inputs), 1)
        manifest = cluster.applies()[0]
        self.assertEqual(manifest['metadata']['name'], 'gateflow-bootstrap')
        self.assertEqual(manifest['metadata']['labels'], {'team': 'gateflow'})
        self.assertEqual(set(manifest['data']), set(bootstrap))
        after = cluster.secrets['gateflow-bootstrap']['values']
        self.assertEqual(after['PGPASSWORD'], 'master-password-rotated')
        self.assertEqual({k: v for k, v in after.items() if k != 'PGPASSWORD'},
                         {k: v for k, v in bootstrap.items() if k != 'PGPASSWORD'})
        self.assertEqual(cluster.secrets['gateflow-runtime']['values'], runtime)
        self.assertEqual(self.state.read_text(encoding='utf-8'), state)
        self.assertIn('PGPASSWORD refreshed', out)
        self.assertIn('gateflow-runtime: unchanged', out)
        for value in ('master-password-rotated', 'kept-extra-value', *runtime.values()):
            self.assertNotIn(value, out + err)
            self.assertNotIn(value, ' '.join(' '.join(command) for command in cluster.commands))

    def test_refresh_master_only_needs_the_existing_secret(self):
        cluster = FakeCluster()
        code, _, err = self.run_script(cluster, '--refresh-master-only')
        self.assertEqual(code, 1)
        self.assertIn('run without --refresh-master-only first', err)
        self.assertEqual(cluster.inputs, [])

    def test_terraform_output_values_are_read_from_value(self):
        self.outputs = terraform_file(self.dir, raw=True)
        code, _, err = self.run_script(FakeCluster())
        self.assertEqual(code, 1)
        self.assertIn('"value"', err)
        self.outputs = terraform_file(self.dir, dict(OUTPUTS, master_secret_arn=None))
        code, _, err = self.run_script(FakeCluster())
        self.assertEqual(code, 1)
        self.assertIn('lacks master_secret_arn', err)

    def test_terraform_output_written_by_windows_powershell_is_read(self):
        self.outputs = terraform_file(self.dir, encoding='utf-16')
        self.assertEqual(common.terraform_outputs(self.outputs, ['database_port'])['database_port'], 5432)
        cluster = FakeCluster()
        self.assertEqual(self.run_script(cluster)[0], 0)
        self.assertEqual(cluster.secrets['gateflow-bootstrap']['values']['PGHOST'], HOST)

    def test_failure_output_is_redacted(self):
        cluster = FakeCluster(apply_stderr='Secret "x" is invalid: master-password-one rejected')
        code, out, err = self.run_script(cluster)
        self.assertEqual(code, 1)
        self.assertIn('<redacted>', err)
        self.assertNotIn('master-password-one', out + err)
        for value in self.state_values().values():
            self.assertNotIn(value, out + err)

    def test_runs_as_a_script_despite_shadowing_the_stdlib_secrets_module(self):
        result = subprocess.run([sys.executable, str(AWS/'secrets.py'), '--help'], capture_output=True, text=True,
                                encoding='utf-8', cwd=self.dir)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('--refresh-master-only', result.stdout)


if __name__ == '__main__':
    unittest.main()
