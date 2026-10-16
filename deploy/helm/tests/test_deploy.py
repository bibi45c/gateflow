import contextlib
import copy
from datetime import datetime, timezone
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

HELM = Path(__file__).resolve().parents[1]
OLD = 'sha256:' + '1' * 64
NEW = 'sha256:' + '2' * 64
TAG = 'de208ffb766a70706c108c98fa10e00bce37d266'
KUBE_SECRET = 'kubeconfig-token-must-not-leak'
ENV_SECRET = 'seed-secret-must-not-leak'
STAMP = datetime(2026, 10, 16, 8, 30, 0, tzinfo=timezone.utc)
RECORD_KEYS = {'time', 'operation', 'commit', 'release', 'namespace', 'context', 'service', 'previous_digest',
               'new_digest', 'rollback_from', 'chart', 'values_files', 'image_pins', 'images_before', 'images_after',
               'changed_services', 'helm_revision_before', 'helm_revision', 'smoke', 'verified', 'error'}


def load(name):
    spec = importlib.util.spec_from_file_location(name, HELM/f'{name}.py')
    module = importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
    return module


deploy = load('deploy')


def merge(base, overlay):
    for key, value in overlay.items():
        if isinstance(value, dict) and isinstance(base.get(key), dict):
            merge(base[key], value)
        else:
            base[key] = value


class FakeCluster:
    """Stands in for helm, kubectl, git and the smoke suite."""

    def __init__(self, digests=None, smoke_exit=0, also_change=None, upgrade_exit=0, get_values_stderr=None):
        self.values = {'imageTag': TAG, 'secretName': 'gateflow-runtime', 'services': {
            name: {'image': f'ghcr.io/bibi45c/gateflow-{name}', 'digest': (digests or {}).get(name, '')}
            for name in deploy.SERVICES}}
        self.revision, self.smoke_exit, self.also_change, self.upgrade_exit = 3, smoke_exit, also_change, upgrade_exit
        self.get_values_stderr = get_values_stderr
        self.images = self.render()
        self.commands, self.pins, self.smoke_env = [], None, None

    def render(self):
        return {name: f"{s['image']}@{s['digest']}" if s['digest'] else f"{s['image']}:{self.values['imageTag']}"
                for name, s in self.values['services'].items()}

    def __call__(self, command, **kwargs):
        self.commands.append(command)
        done = lambda out='', code=0: subprocess.CompletedProcess(command, code, out, 'fake failure' if code else '')
        if command[0] == 'git':
            return done('abc123\n')
        if command[0] == 'helm' and 'get' in command:
            if self.get_values_stderr:
                return subprocess.CompletedProcess(command, 1, '', self.get_values_stderr)
            return done(json.dumps(self.values))
        if command[0] == 'helm' and 'status' in command:
            return done(json.dumps({'version': self.revision}))
        if command[0] == 'helm' and 'upgrade' in command:
            files = [command[i+1] for i, part in enumerate(command) if part == '-f']
            self.pins = json.loads(Path(files[-1]).read_text(encoding='utf-8'))
            if self.upgrade_exit:
                return done(code=self.upgrade_exit)
            merged = copy.deepcopy(self.values)
            for path in files:
                if path.endswith('.json'):
                    merge(merged, json.loads(Path(path).read_text(encoding='utf-8')))
            self.values, self.revision = merged, self.revision + 1
            self.images = self.render()
            if self.also_change:
                self.images[self.also_change] += '-changed'
            return done()
        if command[0] == 'kubectl':
            items = [{'metadata': {'name': f'gf-{name}'},
                      'spec': {'template': {'spec': {'containers': [{'name': name, 'image': image}]}}}}
                     for name, image in self.images.items()]
            return done(json.dumps({'kind': 'List', 'items': items}))
        if command[1].endswith('smoke.py'):
            self.smoke_env = kwargs.get('env')
            return done(code=self.smoke_exit)
        raise AssertionError(f'unexpected command {command}')


class DeployTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.dir = Path(directory.name)
        self.kubeconfig = self.dir/'kubeconfig'
        self.kubeconfig.write_text(f'users:\n- user:\n    token: {KUBE_SECRET}\n', encoding='utf-8')

    def run_deploy(self, cluster, *extra, record='record.json'):
        argv = ['--kubeconfig', str(self.kubeconfig), '--kube-context', 'kind-gateflow71', '--record',
                str(self.dir/record), *extra]
        with mock.patch.dict(os.environ, {'ACCESS_SEED_SECRET': ENV_SECRET}), \
                contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            code = deploy.main(argv, runner=cluster, now=STAMP)
        return code, json.loads((self.dir/record).read_text(encoding='utf-8'))

    def test_digest_format_is_enforced(self):
        self.assertEqual(deploy.digest(NEW), NEW)
        for bad in ['', '2'*64, 'sha256:' + '2'*63, 'sha256:' + 'A'*64, 'sha512:' + '2'*64, NEW + '\n', ' ' + NEW]:
            with self.subTest(digest=bad), self.assertRaises(Exception):
                deploy.digest(bad)
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            deploy.parse_args(['--kubeconfig', 'k', '--kube-context', 'c', '--service', 'gateway',
                               '--digest', 'sha256:abc', '--skip-smoke'])

    def test_help_lists_the_options(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out), self.assertRaises(SystemExit) as exit:
            deploy.parse_args(['--help'])
        self.assertEqual(exit.exception.code, 0)
        for option in ['--kubeconfig', '--kube-context', '--namespace', '--release', '--service', '--digest',
                       '--rollback-from', '--values', '--record', '--smoke-host', '--smoke-port',
                       '--smoke-postgres-pod', '--skip-smoke']:
            self.assertIn(option, out.getvalue())

    def test_smoke_choice_and_target_are_required(self):
        base = ['--kubeconfig', 'k', '--kube-context', 'c', '--service', 'gateway']
        for argv in [base + ['--digest', NEW], base + ['--digest', NEW, '--skip-smoke', '--smoke-port', '1'],
                     base + ['--skip-smoke'], ['--kubeconfig', 'k', '--kube-context', 'c', '--digest', NEW, '--skip-smoke'],
                     base + ['--digest', NEW, '--rollback-from', 'r.json', '--skip-smoke'],
                     ['--kube-context', 'c', '--service', 'gateway', '--digest', NEW, '--skip-smoke']]:
            with self.subTest(argv=argv), contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                deploy.parse_args(argv)

    def test_pins_change_only_the_target_digest(self):
        values = FakeCluster(digests={'access': OLD}).values
        pins = deploy.image_pins(values, 'gateway', NEW)
        self.assertEqual(pins, {'imageTag': TAG, 'services': {
            'gateway': {'digest': NEW}, 'access': {'digest': OLD}, 'metering': {'digest': ''}, 'mock-upstream': {'digest': ''}}})
        self.assertEqual(deploy.image_pins(values, 'access', None)['services']['access'], {'digest': ''})

    def test_deploy_moves_only_the_target_and_records_everything(self):
        cluster = FakeCluster(digests={'gateway': OLD, 'metering': OLD})
        before = dict(cluster.images)
        code, record = self.run_deploy(cluster, '--service', 'gateway', '--digest', NEW, '--smoke-port', '18071',
                                       '--smoke-postgres-pod', 'postgres-0')
        self.assertEqual(code, 0, record['error'])
        self.assertEqual(set(record), RECORD_KEYS)
        self.assertEqual((record['operation'], record['service'], record['previous_digest'], record['new_digest']),
                         ('deploy', 'gateway', OLD, NEW))
        self.assertEqual((record['commit'], record['release'], record['namespace'], record['context']),
                         ('abc123', 'gf', 'gateflow', 'kind-gateflow71'))
        self.assertEqual(record['time'], '2026-10-16T08:30:00Z')
        self.assertEqual(record['images_before'], before)
        self.assertEqual(record['images_after'], before | {'gateway': f'ghcr.io/bibi45c/gateflow-gateway@{NEW}'})
        self.assertEqual(record['changed_services'], ['gateway'])
        self.assertEqual((record['helm_revision_before'], record['helm_revision']), (3, 4))
        self.assertEqual(record['smoke'], {'result': 'passed', 'exit_code': 0, 'host': '127.0.0.1', 'port': 18071})
        self.assertTrue(record['verified'])
        self.assertIsNone(record['error'])
        self.assertEqual(cluster.pins['services']['metering'], {'digest': OLD})
        upgrade = next(c for c in cluster.commands if 'upgrade' in c)
        self.assertEqual(upgrade[:5], ['helm', '--kubeconfig', str(self.kubeconfig), '--kube-context', 'kind-gateflow71'])
        self.assertIn('--wait', upgrade); self.assertIn('--wait-for-jobs', upgrade)
        self.assertNotIn('rollback', upgrade); self.assertNotIn('--install', upgrade)
        for command in cluster.commands:
            if command[0] in ('helm', 'kubectl'):
                self.assertIn('--kubeconfig', command)
        smoke = next(c for c in cluster.commands if c[1].endswith('smoke.py'))
        self.assertEqual(smoke[2:], ['--host', '127.0.0.1', '--port', '18071', '--kube-context', 'kind-gateflow71',
                                     '--namespace', 'gateflow', '--postgres-pod', 'postgres-0'])
        self.assertEqual(cluster.smoke_env['KUBECONFIG'], str(self.kubeconfig))

    def test_record_holds_no_secrets(self):
        values = self.dir/'values-aws.json'
        values.write_text('{"environment": "aws"}', encoding='utf-8')
        code, record = self.run_deploy(FakeCluster(), '--service', 'access', '--digest', NEW, '-f', str(values),
                                       '--smoke-port', '18071')
        self.assertEqual(code, 0, record['error'])
        text = (self.dir/'record.json').read_text(encoding='utf-8')
        # Deployed values are not copied into the record, nor is the kubeconfig or its path.
        for secret in [KUBE_SECRET, ENV_SECRET, 'gateflow-runtime', json.dumps(str(self.kubeconfig))[1:-1]]:
            self.assertNotIn(secret, text)
        self.assertEqual(record['values_files'], [{'path': 'values-aws.json', 'outside_repo': True,
                                                   'sha256': hashlib.sha256(values.read_bytes()).hexdigest()}])

    def assert_no_local_paths(self, record, paths):
        text = (self.dir/'record.json').read_text(encoding='utf-8')
        for path in paths:
            for form in {str(path), path.as_posix(), json.dumps(str(path))[1:-1]}:
                with self.subTest(path=form):
                    self.assertNotIn(form, text)
                    self.assertNotIn(form, json.dumps(record, ensure_ascii=False))

    def test_failed_helm_keeps_kubeconfig_and_home_out_of_the_record(self):
        home, repo = Path.home(), HELM.parents[1]
        stderr = (f'Error: Kubernetes cluster unreachable: open {self.kubeconfig}: access denied\n'
                  f'cache {home/".kube"/"cache"} and chart {repo/"deploy"/"helm"/"gateflow"}\n')
        cluster = FakeCluster(get_values_stderr=stderr)
        code, record = self.run_deploy(cluster, '--service', 'gateway', '--digest', NEW, '--skip-smoke')
        self.assertEqual(code, 1)
        failed = next(c for c in cluster.commands if 'get' in c)
        self.assertIn(str(self.kubeconfig), failed)
        self.assertTrue(record['error'].startswith('helm get exited with 1: Error: Kubernetes cluster unreachable'),
                        record['error'])
        self.assertIn('<kubeconfig>', record['error'])
        self.assertIn('<home>', record['error'])
        self.assertNotIn('--kubeconfig', record['error'])
        self.assert_no_local_paths(record, [self.kubeconfig, self.kubeconfig.resolve(), home, repo])

    def test_record_paths_inside_the_repository_are_relative(self):
        values = HELM/'values-aws.yaml'
        code, record = self.run_deploy(FakeCluster(), '--service', 'gateway', '--digest', NEW, '-f', str(values),
                                       '--skip-smoke')
        self.assertEqual(code, 0, record['error'])
        self.assertEqual(set(record), RECORD_KEYS)
        self.assertEqual(record['chart'], 'deploy/helm/gateflow')
        self.assertEqual(record['values_files'], [{'path': 'deploy/helm/values-aws.yaml',
                                                   'sha256': hashlib.sha256(values.read_bytes()).hexdigest()}])
        self.assert_no_local_paths(record, [self.kubeconfig, Path.home(), HELM.parents[1]])

    def test_record_paths_outside_the_repository_keep_only_the_file_name(self):
        values = self.dir/'private'/'values-mine.yaml'
        values.parent.mkdir()
        values.write_text('environment: mine\n', encoding='utf-8')
        code, record = self.run_deploy(FakeCluster(), '--service', 'gateway', '--digest', NEW, '-f', str(values),
                                       '--chart', str(self.dir/'chart'), '--skip-smoke')
        self.assertEqual(code, 0, record['error'])
        self.assertEqual(record['values_files'], [{'path': 'values-mine.yaml', 'outside_repo': True,
                                                   'sha256': hashlib.sha256(values.read_bytes()).hexdigest()}])
        self.assertEqual((record['chart'], record['chart_outside_repo']), ('chart', True))
        self.assert_no_local_paths(record, [self.dir, values, Path.home(), HELM.parents[1]])

    def test_other_service_change_fails(self):
        code, record = self.run_deploy(FakeCluster(also_change='access'), '--service', 'gateway', '--digest', NEW,
                                       '--smoke-port', '18071')
        self.assertEqual(code, 1)
        self.assertIn('access', record['error'])
        self.assertEqual(record['changed_services'], ['gateway', 'access'])
        self.assertFalse(record['verified'])
        self.assertEqual(record['smoke']['result'], 'not-run')

    def test_failed_upgrade_and_smoke_are_recorded(self):
        code, record = self.run_deploy(FakeCluster(upgrade_exit=1), '--service', 'gateway', '--digest', NEW,
                                       '--smoke-port', '18071')
        self.assertEqual(code, 1)
        self.assertIn('helm upgrade exited with 1', record['error'])
        self.assertEqual(record['changed_services'], [])
        code, record = self.run_deploy(FakeCluster(smoke_exit=1), '--service', 'gateway', '--digest', NEW,
                                       '--smoke-port', '18071')
        self.assertEqual(code, 1)
        self.assertEqual(record['smoke']['result'], 'failed')
        self.assertEqual(record['smoke']['exit_code'], 1)
        self.assertFalse(record['verified'])

    def test_skipped_smoke_is_recorded_and_not_verified(self):
        cluster = FakeCluster()
        code, record = self.run_deploy(cluster, '--service', 'gateway', '--digest', NEW, '--skip-smoke')
        self.assertEqual(code, 0, record['error'])
        self.assertEqual(record['smoke'], {'result': 'skipped', 'exit_code': None, 'host': '127.0.0.1', 'port': None})
        self.assertFalse(record['verified'])
        self.assertFalse(any(c[1].endswith('smoke.py') for c in cluster.commands if len(c) > 1))

    def test_rollback_from_record_restores_previous_digest(self):
        cluster = FakeCluster(digests={'gateway': OLD})
        before = dict(cluster.images)
        self.run_deploy(cluster, '--service', 'gateway', '--digest', NEW, '--skip-smoke', record='deploy.json')
        code, record = self.run_deploy(cluster, '--rollback-from', str(self.dir/'deploy.json'), '--smoke-port', '18071',
                                       record='rollback.json')
        self.assertEqual(code, 0, record['error'])
        self.assertEqual((record['operation'], record['service'], record['previous_digest'], record['new_digest']),
                         ('rollback', 'gateway', NEW, OLD))
        self.assertEqual((record['rollback_from'], record['rollback_from_outside_repo']), ('deploy.json', True))
        self.assertEqual(cluster.pins['services']['gateway'], {'digest': OLD})
        self.assertEqual(record['images_after'], before)

    def test_rollback_to_tag_clears_the_digest(self):
        cluster = FakeCluster()
        before = dict(cluster.images)
        self.run_deploy(cluster, '--service', 'gateway', '--digest', NEW, '--skip-smoke', record='deploy.json')
        code, record = self.run_deploy(cluster, '--rollback-from', str(self.dir/'deploy.json'), '--skip-smoke',
                                       record='rollback.json')
        self.assertEqual(code, 0, record['error'])
        self.assertEqual((record['previous_digest'], record['new_digest']), (NEW, None))
        self.assertEqual(cluster.pins['services']['gateway'], {'digest': ''})
        self.assertEqual(record['images_after'], before)

    def test_rollback_refuses_stale_or_foreign_records(self):
        cluster = FakeCluster(digests={'gateway': OLD})
        self.run_deploy(cluster, '--service', 'gateway', '--digest', NEW, '--skip-smoke', record='deploy.json')
        self.run_deploy(cluster, '--service', 'gateway', '--digest', 'sha256:' + '3'*64, '--skip-smoke', record='later.json')
        upgrades = sum('upgrade' in c for c in cluster.commands)
        code, record = self.run_deploy(cluster, '--rollback-from', str(self.dir/'deploy.json'), '--skip-smoke',
                                       record='stale.json')
        self.assertEqual(code, 1)
        self.assertIn('refusing', record['error'])
        self.assertEqual(sum('upgrade' in c for c in cluster.commands), upgrades)
        code, record = self.run_deploy(cluster, '--rollback-from', str(self.dir/'later.json'), '--service', 'access',
                                       '--skip-smoke', record='wrong-service.json')
        self.assertEqual(code, 1)
        self.assertIn('does not match', record['error'])
        failed = json.loads((self.dir/'later.json').read_text(encoding='utf-8')) | {'image_pins': None}
        (self.dir/'failed.json').write_text(json.dumps(failed), encoding='utf-8')
        code, record = self.run_deploy(cluster, '--rollback-from', str(self.dir/'failed.json'), '--skip-smoke',
                                       record='from-failed.json')
        self.assertEqual(code, 1)
        self.assertIn('previous_digest', record['error'])

    def test_default_record_path_is_ignored_local_folder(self):
        path = deploy.default_record('gateway', STAMP)
        self.assertEqual(path, HELM.parents[1]/'.local'/'deployments'/'20261016T083000Z-gateway.json')


if __name__ == '__main__':
    unittest.main()
