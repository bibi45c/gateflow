import contextlib
from datetime import datetime, timezone
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from loader import AWS, completed, load

reset_db = load('reset_db')
common = sys.modules['common']

BALANCE = 1_000_000_000_000
URL_SECRET = 'url-password-must-not-leak'
METERING_IMAGE = 'ghcr.io/bibi45c/gateflow-metering@sha256:' + '3' * 64
METERING_POD = {
    'containers': [{
        'name': 'metering', 'image': METERING_IMAGE,
        'env': [{'name': 'DATABASE_URL', 'valueFrom': {'secretKeyRef': {'name': 'gateflow-runtime',
                                                                        'key': 'METERING_DATABASE_URL'}}},
                {'name': 'METERING_URL', 'value': 'http://gf-metering:8082'}],
        'securityContext': {'allowPrivilegeEscalation': False, 'readOnlyRootFilesystem': True,
                            'capabilities': {'drop': ['ALL']}},
        'resources': {'requests': {'cpu': '100m'}}, 'ports': [{'containerPort': 8082}],
        'readinessProbe': {'httpGet': {'path': '/readyz'}}}],
    'initContainers': [{'name': 'check-database-tls', 'image': 'postgres:17'}],
    'automountServiceAccountToken': False, 'nodeSelector': {'gateflow.io/workload': 'services'},
    'securityContext': {'runAsNonRoot': True, 'seccompProfile': {'type': 'RuntimeDefault'}},
}


class FakeKube:
    """Stands in for kubectl against one release with a stateful gateway and database."""

    def __init__(self, keda=False, pause=None, fail=None, seed_phase='Succeeded', verify=None, csv_rows=None):
        self.keda, self.pause, self.fail = keda, pause, fail or set()
        self.seed_phase, self.verify, self.csv_rows = seed_phase, verify, csv_rows
        self.replicas = {'gateway': 2, 'access': 1, 'metering': 1}
        self.gateway_pods = 2
        self.pods, self.deleted, self.created = {}, [], []
        self.commands, self.events = [], []
        self.counts = {'wallets': 11, 'reservations': 5, 'ledger_entries': 4}
        self.truncated = self.seeded = False

    def event(self, name, command, code=0, stdout='', stderr=''):
        self.events.append(name)
        if name in self.fail:
            return completed(command, 1, stderr=f'{name} failed for postgres://metering:{URL_SECRET}@db:5432/gateflow')
        return completed(command, code, stdout, stderr)

    def deployment(self, name):
        item = {'metadata': {'name': f'gf-{name}'}, 'spec': {'replicas': self.replicas[name], 'template': {
            'spec': METERING_POD if name == 'metering' else {'containers': [{'name': name, 'image': name}]}}},
            'status': {'readyReplicas': self.replicas[name]}}
        return item

    def __call__(self, command, **kwargs):
        self.commands.append(list(command))
        if command[0] == 'git':
            return completed(command, 1)
        assert command[:5] == ['kubectl', '--kubeconfig', 'kubeconfig-path', '--context', 'eks-test'], command
        args = command[7:] if command[5] == '-n' else command[5:]
        if command[5] == '-n':
            assert command[6] == 'gateflow'
        verb = args[0]
        if verb == 'api-resources':
            names = 'scaledobjects.keda.sh\nscaledjobs.keda.sh\n' if self.keda else ''
            return self.event('api-resources', command, stdout=names)
        if verb == 'get' and args[1] == 'deployment':
            names = [name[3:] for name in args[2:] if name.startswith('gf-')]
            items = [self.deployment(name) for name in names]
            document = items[0] if len(items) == 1 else {'kind': 'List', 'items': items}
            return self.event(f'get deployment {" ".join(names)}', command, stdout=json.dumps(document))
        if verb == 'get' and args[1] == 'scaledobject.keda.sh':
            annotations = {reset_db.KEDA_PAUSE: self.pause} if self.pause else {}
            return self.event('get scaledobject', command,
                              stdout=json.dumps({'metadata': {'name': args[2], 'annotations': annotations}}))
        if verb == 'scale':
            replicas = int(args[3].split('=')[1])
            result = self.event(f'scale {replicas}', command)
            if not result.returncode:
                self.replicas['gateway'] = replicas
                self.gateway_pods = replicas
            return result
        if verb == 'annotate':
            result = self.event(f'annotate {" ".join(args[3:])}', command)
            if not result.returncode:
                if args[3] == f'{reset_db.KEDA_PAUSE}=0':
                    self.gateway_pods = 0
                else:
                    self.replicas['gateway'] = self.gateway_pods = 2
            return result
        if verb == 'get' and args[1] == 'pods':
            items = [{'metadata': {'name': f'gf-gateway-{n}'}} for n in range(self.gateway_pods)]
            return self.event('get gateway pods', command, stdout=json.dumps({'items': items}))
        if verb == 'create':
            spec = json.loads(kwargs['input'])
            component = spec['metadata']['labels']['app.kubernetes.io/component']
            self.created.append(spec)
            result = self.event(f'create {component}', command)
            if not result.returncode:
                self.pods[spec['metadata']['name']] = component
            return result
        if verb == 'wait':
            return self.event('wait query pod', command)
        if verb == 'get' and args[1] == 'pod':
            return self.event('get seed pod', command, stdout=json.dumps({'status': {'phase': self.seed_phase}}))
        if verb == 'logs':
            if self.seed_phase == 'Succeeded':
                return self.event('seed logs', command, stdout='metering seed: created 11 of 11 wallets\n')
            return self.event('seed logs', command,
                              stdout=f'metering: connect postgres://metering:{URL_SECRET}@db:5432/gateflow failed\n')
        if verb == 'delete':
            self.deleted.append(args[2])
            return self.event(f'delete {self.pods.get(args[2], "unknown")}', command)
        if verb == 'exec':
            return self.psql(command, args, kwargs)
        raise AssertionError(f'unexpected command {command}')

    def psql(self, command, args, kwargs):
        assert args[2:6] == ['--', 'sh', '-c', 'exec psql -X -v ON_ERROR_STOP=1 "$@" "$DATABASE_URL"'], args
        sql = args[-1]
        assert 'access.' not in sql, sql
        if sql.startswith('COPY'):
            table = sql.split('accounting.')[1].split(' ')[0]
            result = self.event(f'copy {table}', command)
            rows = (self.csv_rows or self.counts)[table]
            kwargs['stdout'].write(('header\n' + 'row\n' * rows).encode())
            return subprocess.CompletedProcess(command, result.returncode, None, result.stderr.encode())
        if sql.startswith('SET LOCAL'):
            result = self.event('truncate', command)
            self.truncated = not result.returncode
            return result
        if sql == reset_db.COUNTS_SQL:
            counts = self.counts
            return self.event('counts', command, stdout=f"{counts['wallets']}|{counts['reservations']}|1|7200|"
                                                         f"{counts['ledger_entries']}\n")
        if 'FILTER' in sql:
            values = self.verify or [11, 1, 10, 0, BALANCE, 0, 0, 0]
            return self.event('verify', command, stdout='|'.join(map(str, values)) + '\n')
        raise AssertionError(sql)


class Clock:
    def __init__(self):
        self.now, self.sleeps = 0.0, []

    def __call__(self):
        return self.now

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.now += seconds


class ResetTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.dir = Path(directory.name)

    def run_reset(self, kube, *extra):
        argv = ['--kubeconfig', 'kubeconfig-path', '--kube-context', 'eks-test',
                '--evidence-dir', str(self.dir/'evidence'), *extra]
        out, err = io.StringIO(), io.StringIO()
        clock = Clock()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = reset_db.main(argv, runner=kube, sleep=clock.sleep, clock=clock,
                                 now=datetime(2026, 10, 17, 9, 30, tzinfo=timezone.utc))
        return code, out.getvalue(), err.getvalue()

    def assert_cleaned_up(self, kube):
        self.assertEqual(set(kube.deleted), set(kube.pods))
        self.assertEqual(kube.replicas['gateway'], 2)

    def test_without_yes_only_prints_the_plan(self):
        kube = FakeKube()
        code, out, _ = self.run_reset(kube)
        self.assertEqual(code, 0)
        self.assertEqual(kube.commands, [])
        self.assertIn('nothing was changed', out)
        self.assertIn('TRUNCATE accounting.ledger_entries, accounting.reservations, accounting.wallets', out)
        self.assertIn(f'--balance {BALANCE} --tenants 10', out)
        self.assertFalse((self.dir/'evidence').exists())

    def test_steps_run_in_order_and_restore_the_gateway(self):
        kube = FakeKube()
        code, out, err = self.run_reset(kube, '--yes')
        self.assertEqual(code, 0, err)
        self.assertEqual(kube.events, [
            'get deployment gateway access metering', 'api-resources', 'scale 0', 'get gateway pods',
            'create db-query', 'wait query pod', 'counts', 'copy wallets', 'copy reservations',
            'copy ledger_entries', 'truncate', 'create seed', 'get seed pod', 'seed logs', 'delete seed',
            'verify', 'delete db-query', 'scale 2', 'get deployment gateway'])
        self.assert_cleaned_up(kube)
        self.assertIn('gateway: scaled gf-gateway from 2 to 0', out)
        self.assertIn('metering seed: created 11 of 11 wallets', out)
        self.assertIn(f'tenant-01 balance {BALANCE}', out)
        self.assertIn('gateway: scaled gf-gateway back to 2', out)

    def test_evidence_is_exported_before_the_truncate(self):
        kube = FakeKube()
        self.assertEqual(self.run_reset(kube, '--yes')[0], 0)
        [run] = list((self.dir/'evidence').iterdir())
        self.assertEqual(run.name, '20261017T093000Z')
        record = json.loads((run/'counts.json').read_text(encoding='utf-8'))
        self.assertEqual(record['counts_before'], {'wallets': 11, 'reservations': 5, 'reservations_reserved': 1,
                                                  'wallets_held': 7200, 'ledger_entries': 4})
        self.assertEqual({name: item['rows'] for name, item in record['files'].items()},
                         {'wallets.csv': 11, 'reservations.csv': 5, 'ledger_entries.csv': 4})
        self.assertEqual(record['seed'], {'balance': BALANCE, 'tenants': 10})
        self.assertNotIn('kubeconfig', json.dumps(record))
        self.assertEqual((run/'ledger_entries.csv').read_text(encoding='utf-8').count('row'), 4)

    def test_truncate_names_all_three_tables_without_cascade(self):
        self.assertIn('TRUNCATE TABLE accounting.ledger_entries, accounting.reservations, accounting.wallets',
                      reset_db.TRUNCATE_SQL)
        self.assertNotIn('CASCADE', reset_db.TRUNCATE_SQL.upper())
        kube = FakeKube()
        self.assertEqual(self.run_reset(kube, '--yes')[0], 0)
        [truncate] = [command for command in kube.commands if command[-1] == reset_db.TRUNCATE_SQL]
        self.assertEqual(truncate[-3:-1], ['-1', '-c'])

    def test_temporary_pods_follow_the_query_pod_pattern(self):
        kube = FakeKube(verify=[4, 1, 3, 0, 5000, 0, 0, 0])
        code, _, err = self.run_reset(kube, '--yes', '--balance', '5000', '--tenants', '3')
        self.assertEqual(code, 0, err)
        query, seed = kube.created
        spec = query['spec']
        self.assertEqual(spec['containers'][0]['image'], 'postgres:17')
        self.assertEqual(spec['containers'][0]['env'][0]['valueFrom'],
                         {'secretKeyRef': {'name': 'gateflow-runtime', 'key': 'METERING_DATABASE_URL'}})
        self.assertEqual(spec['containers'][0]['securityContext']['capabilities'], {'drop': ['ALL']})
        self.assertFalse(spec['automountServiceAccountToken'])
        self.assertTrue(spec['securityContext']['runAsNonRoot'])
        self.assertEqual(spec['nodeSelector'], {'gateflow.io/workload': 'services'})
        container = seed['spec']['containers'][0]
        self.assertEqual(container['image'], METERING_IMAGE)
        self.assertEqual(container['args'], ['seed', '--balance', '5000', '--tenants', '3'])
        self.assertEqual(container['env'], METERING_POD['containers'][0]['env'])
        self.assertEqual(container['securityContext'], METERING_POD['containers'][0]['securityContext'])
        self.assertNotIn('ports', container)
        self.assertNotIn('readinessProbe', container)
        self.assertNotIn('initContainers', seed['spec'])
        self.assertEqual(seed['spec']['restartPolicy'], 'Never')
        self.assertFalse(seed['spec']['automountServiceAccountToken'])
        self.assertEqual(seed['spec']['securityContext'], METERING_POD['securityContext'])
        self.assertEqual(seed['spec']['nodeSelector'], {'gateflow.io/workload': 'services'})

    def test_keda_is_paused_and_resumed_instead_of_scaling(self):
        kube = FakeKube(keda=True)
        code, out, err = self.run_reset(kube, '--yes')
        self.assertEqual(code, 0, err)
        self.assertNotIn('scale 0', kube.events)
        self.assertNotIn('scale 2', kube.events)
        self.assertLess(kube.events.index(f'annotate {reset_db.KEDA_PAUSE}=0 --overwrite'), kube.events.index('counts'))
        self.assertEqual(kube.events[-2], f'annotate {reset_db.KEDA_PAUSE}-')
        self.assertIn('KEDA ScaledObject gf-gateway paused', out)
        self.assertIn('KEDA resumed', out)

    def test_keda_annotation_already_present_is_restored(self):
        kube = FakeKube(keda=True, pause='3')
        code, _, err = self.run_reset(kube, '--yes')
        self.assertEqual(code, 0, err)
        self.assertEqual(kube.events[-1], f'annotate {reset_db.KEDA_PAUSE}=3 --overwrite')

    def test_truncate_failure_stops_cleans_up_and_restores(self):
        kube = FakeKube(fail={'truncate'})
        code, out, err = self.run_reset(kube, '--yes')
        self.assertEqual(code, 1)
        self.assertNotIn('create seed', kube.events)
        self.assert_cleaned_up(kube)
        self.assertIn('Recovery:', err)
        self.assertIn('pre-reset export is in', err)
        self.assertNotIn(URL_SECRET, out + err)
        self.assertIn('<redacted>', err)

    def test_seed_failure_deletes_both_pods_and_restores(self):
        kube = FakeKube(seed_phase='Failed')
        code, out, err = self.run_reset(kube, '--yes')
        self.assertEqual(code, 1)
        self.assertNotIn('verify', kube.events)
        self.assert_cleaned_up(kube)
        self.assertIn('metering seed failed', err)
        self.assertIn('no wallet exists', err)
        self.assertNotIn(URL_SECRET, out + err)

    def test_verification_failure_is_reported(self):
        kube = FakeKube(verify=[11, 1, 9, 0, 100_000_000, 0, 0, 0])
        code, _, err = self.run_reset(kube, '--yes')
        self.assertEqual(code, 1)
        self.assertIn('funded 9 (want 10)', err)
        self.assertIn(f'tenant_01 100000000 (want {BALANCE})', err)
        self.assertIn('did not verify', err)
        self.assert_cleaned_up(kube)

    def test_export_mismatch_stops_before_the_truncate(self):
        kube = FakeKube(csv_rows={'wallets': 11, 'reservations': 4, 'ledger_entries': 4})
        code, _, err = self.run_reset(kube, '--yes')
        self.assertEqual(code, 1)
        self.assertNotIn('truncate', kube.events)
        self.assertIn('database was not changed', err)
        self.assert_cleaned_up(kube)

    def test_query_pod_failure_still_deletes_it_and_restores(self):
        kube = FakeKube(fail={'wait query pod'})
        code, _, err = self.run_reset(kube, '--yes')
        self.assertEqual(code, 1)
        self.assertIn('delete db-query', kube.events)
        self.assertEqual(kube.replicas['gateway'], 2)
        self.assertIn('database was not changed', err)

    def test_failed_restore_prints_the_manual_command(self):
        kube = FakeKube(fail={'scale 2'})
        code, _, err = self.run_reset(kube, '--yes')
        self.assertEqual(code, 1)
        self.assertIn('restore it with: kubectl --context eks-test -n gateflow scale deployment gf-gateway '
                      '--replicas=2', err)
        self.assertNotIn('kubeconfig-path', err)

    def test_no_secret_reaches_arguments_or_output(self):
        kube = FakeKube()
        code, out, err = self.run_reset(kube, '--yes')
        self.assertEqual(code, 0, err)
        text = out + err + json.dumps(kube.commands) + json.dumps(kube.created)
        self.assertNotIn(URL_SECRET, text)
        self.assertNotIn('postgres://', text)

    def test_refuses_an_evidence_directory_git_does_not_ignore(self):
        kube = FakeKube()
        inside = common.ROOT/'deploy'/'aws'/'tests'/'not-ignored-evidence'
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = reset_db.main(['--kubeconfig', 'kubeconfig-path', '--kube-context', 'eks-test', '--yes',
                                  '--evidence-dir', str(inside)], runner=kube)
        self.assertEqual(code, 1)
        self.assertEqual([command[0] for command in kube.commands], ['git'])
        self.assertFalse(inside.exists())

    def test_runs_as_a_script_beside_secrets_py(self):
        # deploy/aws/secrets.py shadows the stdlib module for scripts run from deploy/aws.
        result = subprocess.run([sys.executable, str(AWS/'reset_db.py'), '--help'], capture_output=True, text=True,
                                encoding='utf-8', cwd=self.dir)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('--evidence-dir', result.stdout)


if __name__ == '__main__':
    unittest.main()
