"""Unit tests of kube_run.py. They use a fake runner and need neither kubectl nor a cluster."""
import contextlib
from datetime import datetime, timezone
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import kube_run

NOW = datetime(2026, 10, 16, 12, 0, 0, tzinfo=timezone.utc)
RUN_JSON = b'{"type":"Point","metric":"gateflow_start"}\n'
GATEWAY_IMAGE = 'ghcr.io/bibi45c/gateflow-gateway@sha256:' + 'a' * 64
DEPLOYMENT = {'spec': {'replicas': 1, 'template': {'spec': {'containers': [{'name': 'gateway', 'image': GATEWAY_IMAGE}]}}},
              'status': {'readyReplicas': 1}}


def done(stdout='', returncode=0, stderr=''):
    return subprocess.CompletedProcess([], returncode, stdout, stderr)


class Cluster:
    """A fake runner answering the git, kubectl and summarize calls of one run."""

    def __init__(self, kubeconfig, waiting=1, phase='Running', status=None, fail=None, interrupt=None,
                 remote_size=None, summarize_code=0, run_json=RUN_JSON):
        self.kubeconfig, self.waiting, self.phase, self.status = str(kubeconfig), waiting, phase, status or {}
        self.fail, self.summarize_code, self.run_json = fail, summarize_code, run_json
        # The steps that raise KeyboardInterrupt, as if Ctrl-C were pressed during them.
        self.interrupt = {interrupt} if isinstance(interrupt, str) else set(interrupt or ())
        self.remote_size = len(run_json) if remote_size is None else remote_size
        self.commands, self.created, self.deleted = [], [], []
        self.creates = 0

    def __call__(self, command, **kwargs):
        self.commands.append((command, kwargs))
        if command[0] == 'git':
            return done('0123456789abcdef\n')
        if command[0] == sys.executable:
            if 'summarize' in self.interrupt:
                raise KeyboardInterrupt
            (Path(kwargs['cwd'])/'run.results.json').write_text('{}\n', encoding='utf-8')
            return done('| Phase | Level (req/s) |\n|---|---|\n| hold | 2 |\n\nWrote run.results.json\n',
                        self.summarize_code)
        verb = command[7]
        step = verb if verb not in ('get', 'exec') else f'{verb} {command[8] if verb == "get" else command[-1]}'
        if step in self.interrupt:
            raise KeyboardInterrupt
        if step == self.fail or (self.fail == 'create pod' and verb == 'create' and self.creates == 1):
            self.creates += verb == 'create'
            return done(returncode=1, stderr=f'error: open {self.kubeconfig}: AlreadyExists')
        if verb == 'create':
            self.creates += 1
            self.created.append(json.loads(kwargs['input']))
            return done()
        if step == 'get deployment':
            return done(json.dumps(DEPLOYMENT))
        if step == 'get pod':
            return done(json.dumps({'status': {'phase': self.phase, **self.status}}))
        if step == 'exec /out/exit':
            self.waiting -= 1
            return done('0\n') if self.waiting < 0 else done(returncode=1, stderr='No such file or directory')
        if step == 'exec /out/run.json':
            return done(f'{self.remote_size} /out/run.json\n')
        if verb == 'cp':
            (Path(kwargs['cwd'])/command[-1]).write_bytes(b'0\n' if command[-1] == 'exit' else self.run_json)
            return done()
        if verb == 'logs':
            return done('k6 summary\n')
        if verb == 'delete':
            self.deleted.append(command[8:])
            return done()
        raise AssertionError(f'unexpected command {command}')

    def kubectl(self, verb):
        return [(c, k) for c, k in self.commands if c[0] == 'kubectl' and c[7] == verb]


class Clock:
    def __init__(self):
        self.now, self.sleeps = 0.0, []

    def __call__(self):
        return self.now

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.now += seconds


def parse(*argv):
    return kube_run.parse_args(['--kubeconfig', 'config', '--kube-context', 'eks', *argv], NOW)


def point(metric, time, tags=None, value=1):
    return json.dumps({'type': 'Point', 'metric': metric,
                       'data': {'time': time, 'value': value, 'tags': tags or {}}})


def plan_point(phase, level, start, end, time='2026-10-16T12:00:04.9Z'):
    return point('gateflow_plan', time, {'phase': phase, 'level': level, 'start_s': start, 'end_s': end})


# A short plan, LEVELS=2,4 WARMUP_S=5 RAMP_S=5 HOLD_S=20: warm-up 0-5 s, hold 5-25 s,
# ramp 25-30 s, hold 30-50 s. k6 prints nanoseconds; the time zero is 12:00:05.123456789.
TIMED_RUN_JSON = '\n'.join([
    json.dumps({'type': 'Metric', 'metric': 'gateflow_plan', 'data': {'type': 'counter'}}),
    plan_point('warmup', '2', '0', '5'), plan_point('hold', '2', '5', '25'),
    plan_point('hold', '4', '30', '50'), plan_point('ramp', '4', '25', '30'),
    point('gateflow_start', '2026-10-16T12:00:05.123456789Z'),
    point('gateflow_sent', '2026-10-16T12:00:05.2Z', {'phase': 'warmup', 'level': '2'}),
    point('gateflow_completed', '2026-10-16T20:01:02.5+08:00', {'phase': 'hold', 'level': '4'}),
    point('vus', '2026-10-16T12:00:30Z'),
    '',
]).encode('utf-8')
TIMED_ENV = ('--env', 'LEVELS=2,4', '--env', 'WARMUP_S=5', '--env', 'RAMP_S=5', '--env', 'HOLD_S=20')


class Wall:
    """A fake UTC clock that moves on one minute at each reading."""

    def __init__(self):
        self.readings = 0

    def __call__(self):
        self.readings += 1
        return NOW.replace(minute=self.readings)


class SettingsTests(unittest.TestCase):
    def refused(self, *argv):
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr), self.assertRaises(SystemExit):
            parse(*argv)
        return stderr.getvalue()

    def test_only_messages_js_settings_are_accepted(self):
        args = parse('--env', 'LEVELS=5,25', '--env', 'PAIRED=0', '--env', 'MOCK_FIRST_EVENT_DELAY_MS=0')
        self.assertEqual(args.env, {'LEVELS': '5,25', 'PAIRED': '0', 'MOCK_FIRST_EVENT_DELAY_MS': '0'})
        for setting in ('FOO=1', 'levels=5', 'LEVELS', 'K6_OUT=json=x', 'GATEWAY_URL=http://alb.example',
                        'MOCK_URL=http://127.0.0.1:8090'):
            with self.subTest(setting=setting):
                self.refused('--env', setting)

    def test_seed_secret_is_refused_without_echoing_it(self):
        message = self.refused('--env', 'ACCESS_SEED_SECRET=hunter2-value')
        self.assertIn('gateflow-runtime Secret', message)
        self.assertNotIn('hunter2-value', message)

    def test_repeated_settings_and_bad_names_are_refused(self):
        self.refused('--env', 'LEVELS=5', '--env', 'LEVELS=25')
        for name in ('Upper', 'under_score', '-dash', 'x' * 52):
            with self.subTest(name=name):
                self.refused('--name', name)
        self.refused('--k6-cpu', 'two')
        self.refused('--timeout', '0')
        self.refused('--env', 'HOLD_S=-1')

    def test_defaults_follow_the_time_and_the_plan(self):
        args = parse()
        self.assertEqual((args.namespace, args.release, args.name), ('gateflow', 'gf', '20261016-120000'))
        self.assertEqual(args.out_dir, kube_run.ROOT/'.local'/'loadtest'/'20261016-120000')
        # 60 warm-up, two 60 s ramps, three 180 s holds and the 65 s graceful stop.
        self.assertEqual(args.timeout, 785 + kube_run.MARGIN_S)
        short = parse('--env', 'LEVELS=2', '--env', 'WARMUP_S=5', '--env', 'RAMP_S=5', '--env', 'HOLD_S=20')
        self.assertEqual(short.timeout, 90 + kube_run.MARGIN_S)
        self.assertEqual(parse('--timeout', '60').timeout, 60)

    def test_stages_end_with_the_last_hold_and_the_plan_adds_the_graceful_stop(self):
        self.assertEqual(kube_run.stage_seconds({}), 720)
        self.assertEqual(kube_run.stage_seconds(parse(*TIMED_ENV).env), 50)
        self.assertEqual(kube_run.plan_seconds(parse(*TIMED_ENV).env), 50 + 60 + 5)
        with self.assertRaises(ValueError):
            kube_run.stage_seconds({'HOLD_S': 'nan'})


class TimelineTests(unittest.TestCase):
    def write(self, data):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        path = Path(directory.name)/'run.json'
        path.write_bytes(data)
        return path

    def test_reads_the_time_zero_the_last_point_and_the_plan_in_order(self):
        start, last, plan = kube_run.read_timeline(self.write(TIMED_RUN_JSON))
        self.assertEqual(kube_run.iso(start), '2026-10-16T12:00:05.123Z')
        # The +08:00 point is the latest once both are in UTC.
        self.assertEqual(kube_run.iso(last), '2026-10-16T12:01:02.500Z')
        self.assertEqual(plan, [('warmup', '2', 0.0, 5.0), ('hold', '2', 5.0, 25.0),
                                ('ramp', '4', 25.0, 30.0), ('hold', '4', 30.0, 50.0)])

    def test_phases_are_placed_after_the_time_zero(self):
        fields = kube_run.timeline(self.write(TIMED_RUN_JSON), parse(*TIMED_ENV).env)
        self.assertEqual((fields['load_start'], fields['plan_end'], fields['load_end']),
                         ('2026-10-16T12:00:05.123Z', '2026-10-16T12:00:55.123Z', '2026-10-16T12:01:02.500Z'))
        self.assertEqual(fields['phases'][1], {'phase': 'hold', 'level_rps': 2.0, 'start_s': 5.0, 'end_s': 25.0,
                                               'start': '2026-10-16T12:00:10.123Z',
                                               'end': '2026-10-16T12:00:30.123Z'})
        self.assertEqual([p['phase'] for p in fields['phases']], ['warmup', 'hold', 'ramp', 'hold'])
        self.assertEqual(fields['phases'][-1]['end'], fields['plan_end'])

    def test_output_without_a_time_zero_has_no_phase_times(self):
        data = (plan_point('hold', '2', '0', '5') + '\n').encode('utf-8')
        fields = kube_run.timeline(self.write(data), {})
        self.assertEqual(fields, {'load_start': None, 'plan_end': None, 'load_end': '2026-10-16T12:00:04.900Z',
                                  'phases': None})

    def test_lines_that_are_not_json_are_refused(self):
        with self.assertRaisesRegex(ValueError, 'line 2'):
            kube_run.read_timeline(self.write((point('vus', '2026-10-16T12:00:30Z') + '\n{"type":').encode('utf-8')))

    def test_kubeconfig_and_context_are_required(self):
        for argv in (['--kube-context', 'eks'], ['--kubeconfig', 'config']):
            with self.subTest(argv=argv), contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                kube_run.parse_args(argv, NOW)


class ManifestTests(unittest.TestCase):
    def test_pod_runs_pinned_k6_on_the_generator_node_with_the_seed_from_the_secret(self):
        args = parse('--name', 'calibration', '--release', 'lt', '--env', 'LEVELS=5')
        pod = kube_run.pod(args)
        spec = pod['spec']
        container, = spec['containers']
        self.assertEqual(pod['metadata']['name'], 'gateflow-k6-calibration')
        self.assertEqual(pod['metadata']['labels']['app.kubernetes.io/name'], 'k6')
        self.assertEqual(container['image'], kube_run.K6_IMAGE)
        self.assertRegex(container['image'], r'^grafana/k6:2\.3\.0@sha256:[0-9a-f]{64}$')
        self.assertEqual(spec['nodeSelector'], {'gateflow.io/workload': 'load-generator'})
        self.assertEqual(spec['tolerations'], [{'key': 'gateflow.io/workload', 'operator': 'Equal',
                                               'value': 'load-generator', 'effect': 'NoSchedule'}])
        self.assertIs(spec['automountServiceAccountToken'], False)
        self.assertEqual(spec['restartPolicy'], 'Never')
        self.assertIs(spec['securityContext']['runAsNonRoot'], True)
        self.assertNotEqual(spec['securityContext']['runAsUser'], 0)
        self.assertEqual(container['securityContext'],
                         {'allowPrivilegeEscalation': False, 'capabilities': {'drop': ['ALL']}})
        env = {e['name']: e for e in container['env']}
        self.assertEqual(env['GATEWAY_URL'], {'name': 'GATEWAY_URL', 'value': 'http://lt-gateway:8080'})
        self.assertEqual(env['MOCK_URL']['value'], 'http://lt-mock-upstream:8090')
        self.assertEqual(env['LEVELS']['value'], '5')
        self.assertEqual(env['ACCESS_SEED_SECRET'], {'name': 'ACCESS_SEED_SECRET', 'valueFrom': {
            'secretKeyRef': {'name': 'gateflow-runtime', 'key': 'ACCESS_SEED_SECRET'}}})
        self.assertEqual(container['resources']['requests'], {'cpu': '2', 'memory': '4Gi'})
        self.assertEqual(container['resources']['limits'], container['resources']['requests'])
        self.assertEqual(container['command'][:2], ['sh', '-c'])
        self.assertTrue(container['command'][2].startswith(
            'k6 run --no-usage-report --quiet --out json=/out/run.json /scripts/messages.js; echo $? > /out/exit; sleep '))
        volumes = {v['name']: v for v in spec['volumes']}
        self.assertEqual(volumes['out'], {'name': 'out', 'emptyDir': {}})
        self.assertEqual(volumes['script']['configMap'], {'name': 'gateflow-k6-calibration'})
        self.assertGreater(spec['activeDeadlineSeconds'], args.timeout)

    def test_config_map_holds_messages_js(self):
        args = parse('--name', 'calibration')
        script = kube_run.SCRIPT.read_text(encoding='utf-8')
        document = kube_run.config_map(args, script)
        self.assertEqual(document['metadata']['name'], 'gateflow-k6-calibration')
        self.assertEqual(document['data'], {'messages.js': script})


class RunTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.folder = Path(directory.name)
        self.kubeconfig = self.folder/'secret-cluster'/'kubeconfig'
        self.out = self.folder/'out'
        self.clock = Clock()

    def main(self, cluster, *argv, kubeconfig=None, wall=kube_run.utc_now):
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = kube_run.main(['--kubeconfig', str(kubeconfig or self.kubeconfig), '--kube-context', 'kind-gateflow71',
                                  '--name', 'test', '--out-dir', str(self.out), *argv],
                                 cluster, NOW, self.clock, self.clock.sleep, wall)
        self.stdout = stdout.getvalue()
        self.record_text = (self.out/'run-record.json').read_text(encoding='utf-8')
        return code, json.loads(self.record_text)

    def assert_deleted(self, cluster, *refs):
        self.assertEqual(cluster.deleted, [[*refs, '--ignore-not-found', '--wait=false']] if refs else [])

    def test_fetches_the_output_summarizes_and_cleans_up(self):
        cluster = Cluster(self.kubeconfig, waiting=2)
        code, record = self.main(cluster, '--env', 'LEVELS=2')
        self.assertEqual(code, 0)
        self.assertEqual([d['kind'] for d in cluster.created], ['ConfigMap', 'Pod'])
        self.assertEqual(self.clock.sleeps, [kube_run.POLL_S] * 2)
        copies = cluster.kubectl('cp')
        self.assertEqual([c[-1] for c, _ in copies], ['exit', 'run.json'])
        self.assertEqual([c[-2] for c, _ in copies], ['gateflow-k6-test:/out/exit', 'gateflow-k6-test:/out/run.json'])
        self.assertTrue(all(k['cwd'] == self.out for _, k in copies))
        self.assertEqual((self.out/'run.json').read_bytes(), RUN_JSON)
        self.assertEqual((self.out/'exit').read_bytes(), b'0\n')
        self.assertIn('| hold | 2 |', (self.out/'summary.md').read_text(encoding='utf-8'))
        self.assertIn('| hold | 2 |', self.stdout)
        self.assert_deleted(cluster, 'pod/gateflow-k6-test', 'configmap/gateflow-k6-test')
        summarize, = [(c, k) for c, k in cluster.commands if c[0] == sys.executable]
        self.assertEqual((summarize[0][1:], summarize[1]['cwd']), ([str(kube_run.HERE/'summarize.py'), 'run.json'], self.out))
        for command, _ in cluster.commands:
            if command[0] == 'kubectl':
                self.assertEqual(command[1:7], ['--kubeconfig', str(self.kubeconfig.resolve()), '--context', 'kind-gateflow71',
                                                '-n', 'gateflow'])
        self.assertEqual((record['k6_exit_code'], record['summarize_exit_code'], record['error']), (0, 0, None))
        self.assertEqual(record['cleanup'], 'deleted')
        self.assertEqual(record['gateway'], {'replicas': 1, 'ready_replicas': 1, 'image': GATEWAY_IMAGE})
        self.assertEqual(record['gateway_after'], record['gateway'])
        self.assertEqual(record['k6_image'], kube_run.K6_IMAGE)
        self.assertEqual(record['commit'], '0123456789abcdef')
        self.assertEqual(record['env']['LEVELS'], '2')
        self.assertEqual(record['env']['GATEWAY_URL'], 'http://gf-gateway:8080')
        self.assertEqual(record['script']['path'], 'loadtest/messages.js')
        self.assertEqual(set(record['outputs']), {'exit', 'run.json', 'k6.log', 'run.results.json', 'summary.md',
                                                  'run-record.json'})

    def test_record_places_the_operation_the_load_and_each_phase_in_utc(self):
        wall = Wall()
        cluster = Cluster(self.kubeconfig, run_json=TIMED_RUN_JSON)
        code, record = self.main(cluster, *TIMED_ENV, wall=wall)
        self.assertEqual((code, record['error'], record['timeline_problem']), (0, None, None))
        self.assertEqual(record['op_start'], '2026-10-16T12:00:00.000Z')
        self.assertEqual(record['time'], '2026-10-16T12:00:00Z')
        self.assertEqual(record['load_start'], '2026-10-16T12:00:05.123Z')
        self.assertEqual(record['plan_end'], '2026-10-16T12:00:55.123Z')
        self.assertEqual(record['load_end'], '2026-10-16T12:01:02.500Z')
        # The wall clock is read once, when the cleanup has ended.
        self.assertEqual((wall.readings, record['op_end']), (1, '2026-10-16T12:01:00.000Z'))
        holds = [(p['level_rps'], p['start'], p['end']) for p in record['phases'] if p['phase'] == 'hold']
        self.assertEqual(holds, [(2.0, '2026-10-16T12:00:10.123Z', '2026-10-16T12:00:30.123Z'),
                                 (4.0, '2026-10-16T12:00:35.123Z', '2026-10-16T12:00:55.123Z')])
        self.assertEqual(json.loads(self.record_text)['phases'], record['phases'])

    def test_output_without_times_is_still_summarized(self):
        code, record = self.main(Cluster(self.kubeconfig))
        self.assertEqual((code, record['error']), (0, None))
        self.assertEqual(record['timeline_problem'], 'run.json has no gateflow_start point with a time')
        self.assertEqual((record['load_start'], record['plan_end'], record['load_end'], record['phases']),
                         (None, None, None, None))
        self.assertIsNotNone(record['op_end'])
        self.assertEqual(record['summarize_exit_code'], 0)

    def test_unreadable_output_is_a_timeline_problem_not_a_run_error(self):
        code, record = self.main(Cluster(self.kubeconfig, run_json=b'not json\n'))
        self.assertEqual((code, record['error']), (0, None))
        self.assertIn('line 1: not JSON', record['timeline_problem'])
        self.assertEqual(record['summarize_exit_code'], 0)

    def test_interrupt_while_reading_the_timeline_stops_before_summarize(self):
        cluster = Cluster(self.kubeconfig, run_json=TIMED_RUN_JSON)
        with mock.patch.object(kube_run, 'read_timeline', side_effect=KeyboardInterrupt):
            code, record = self.main(cluster)
        self.assertEqual((code, record['error']), (130, kube_run.TIMELINE_INTERRUPTED))
        self.assertEqual(record['cleanup'], 'deleted')
        self.assertFalse(any(c[0] == sys.executable for c, _ in cluster.commands))

    def test_failed_run_still_records_when_its_cleanup_ended(self):
        wall = Wall()
        code, record = self.main(Cluster(self.kubeconfig, fail='get deployment'), wall=wall)
        self.assertEqual(code, 1)
        self.assertEqual(record['op_end'], '2026-10-16T12:01:00.000Z')
        self.assertIsNone(record['load_start'])
        self.assertIsNone(record['timeline_problem'])

    def test_relative_kubeconfig_is_resolved_for_every_kubectl_call(self):
        # kubectl cp runs in the output folder, where a relative kubeconfig would name another file.
        cwd = os.getcwd()
        os.chdir(self.folder)
        self.addCleanup(os.chdir, cwd)
        relative = os.path.join('secret-cluster', 'kubeconfig')
        cluster = Cluster(relative)
        code, record = self.main(cluster, kubeconfig=relative)
        self.assertEqual((code, record['error']), (0, None))
        kubectl = [(c, k) for c, k in cluster.commands if c[0] == 'kubectl']
        self.assertTrue(kubectl)
        for command, _ in kubectl:
            with self.subTest(command=command[7]):
                self.assertEqual(command[1], '--kubeconfig')
                self.assertTrue(Path(command[2]).is_absolute())
                self.assertEqual(Path(command[2]), self.kubeconfig.resolve())
        copies = cluster.kubectl('cp')
        self.assertEqual(len(copies), 2)
        for command, kwargs in copies:
            self.assertEqual(kwargs['cwd'], self.out)
            self.assertEqual((Path(kwargs['cwd'])/command[2]).resolve(), self.kubeconfig.resolve())
        self.assert_private(self.record_text)

    def test_summarize_and_k6_failures_set_the_exit_code(self):
        code, record = self.main(Cluster(self.kubeconfig, summarize_code=2))
        self.assertEqual((code, record['summarize_exit_code']), (2, 2))

    def test_incomplete_copy_is_an_error_and_is_not_summarized(self):
        cluster = Cluster(self.kubeconfig, remote_size=len(RUN_JSON) + 1)
        code, record = self.main(cluster)
        self.assertEqual(code, 1)
        self.assertIn('bytes of run.json', record['error'])
        self.assertIsNone(record['summarize_exit_code'])
        self.assertFalse(any(c[0] == sys.executable for c, _ in cluster.commands))
        self.assert_deleted(cluster, 'pod/gateflow-k6-test', 'configmap/gateflow-k6-test')

    def test_pod_that_ends_early_is_an_error_and_is_deleted(self):
        cluster = Cluster(self.kubeconfig, phase='Failed', status={'containerStatuses': [
            {'state': {'terminated': {'reason': 'OOMKilled'}}}]})
        code, record = self.main(cluster)
        self.assertEqual(code, 1)
        self.assertIn('OOMKilled', record['error'])
        self.assert_deleted(cluster, 'pod/gateflow-k6-test', 'configmap/gateflow-k6-test')

    def test_pod_that_cannot_start_fails_without_waiting(self):
        cluster = Cluster(self.kubeconfig, phase='Pending', status={'containerStatuses': [
            {'state': {'waiting': {'reason': 'CreateContainerConfigError',
                                   'message': 'secret "gateflow-runtime" not found'}}}]})
        code, record = self.main(cluster)
        self.assertEqual(code, 1)
        self.assertIn('CreateContainerConfigError', record['error'])
        self.assertEqual(self.clock.sleeps, [])
        self.assert_deleted(cluster, 'pod/gateflow-k6-test', 'configmap/gateflow-k6-test')

    def test_timeout_is_an_error_and_cleans_up(self):
        cluster = Cluster(self.kubeconfig, phase='Pending', status={'conditions': [
            {'type': 'PodScheduled', 'status': 'False', 'reason': 'Unschedulable', 'message': '0/3 nodes'}]})
        code, record = self.main(cluster, '--timeout', '30')
        self.assertEqual(code, 1)
        self.assertIn('did not finish within 30 seconds', record['error'])
        self.assertIn('Unschedulable', record['error'])
        self.assert_deleted(cluster, 'pod/gateflow-k6-test', 'configmap/gateflow-k6-test')

    def test_interrupt_cleans_up_and_stops(self):
        cluster = Cluster(self.kubeconfig, interrupt='exec /out/exit')
        code, record = self.main(cluster)
        self.assertEqual(code, 130)
        self.assertEqual(record['error'], 'interrupted')
        self.assert_deleted(cluster, 'pod/gateflow-k6-test', 'configmap/gateflow-k6-test')
        self.assertFalse(any(c[0] == sys.executable for c, _ in cluster.commands))
        self.assertEqual(cluster.kubectl('logs'), [])

    def test_second_interrupt_during_cleanup_records_how_to_delete(self):
        cluster = Cluster(self.kubeconfig, interrupt=('exec /out/exit', 'delete'))
        code, record = self.main(cluster)
        self.assertEqual(code, 130)
        self.assertEqual(record['error'], 'interrupted')
        self.assertEqual(record['cleanup'], 'interrupted; delete them with kubectl -n <namespace> delete '
                                            'pod/gateflow-k6-test configmap/gateflow-k6-test')
        self.assertEqual(len(cluster.kubectl('delete')), 1)
        self.assert_deleted(cluster)  # the only delete call was interrupted
        self.assertNotIn('--kubeconfig', record['cleanup'])
        self.assert_private(self.record_text)
        self.assertFalse(any(c[0] == sys.executable for c, _ in cluster.commands))

    def test_interrupted_summarize_is_recorded(self):
        cluster = Cluster(self.kubeconfig, interrupt='summarize')
        code, record = self.main(cluster)
        self.assertEqual(code, 130)
        self.assertEqual(record['error'], kube_run.SUMMARIZE_INTERRUPTED)
        self.assertIn('summarize.py', record['error'])
        self.assertEqual((record['k6_exit_code'], record['summarize_exit_code']), (0, None))
        self.assertEqual(record['cleanup'], 'deleted')
        self.assert_deleted(cluster, 'pod/gateflow-k6-test', 'configmap/gateflow-k6-test')
        self.assertEqual(record['outputs'], ['exit', 'run.json', 'k6.log', 'run-record.json'])
        self.assertEqual((self.out/'run.json').read_bytes(), RUN_JSON)
        self.assertFalse((self.out/'summary.md').exists())

    def test_interrupted_create_is_still_deleted(self):
        cluster = Cluster(self.kubeconfig, interrupt='create')
        code, _ = self.main(cluster)
        self.assertEqual(code, 130)
        self.assert_deleted(cluster, 'configmap/gateflow-k6-test')

    def test_failed_pod_create_deletes_only_the_config_map(self):
        cluster = Cluster(self.kubeconfig, fail='create pod')
        code, record = self.main(cluster)
        self.assertEqual(code, 1)
        self.assertIn('kubectl create exited with 1', record['error'])
        self.assert_deleted(cluster, 'configmap/gateflow-k6-test')

    def test_objects_of_another_run_are_never_deleted(self):
        cluster = Cluster(self.kubeconfig, fail='create')
        code, record = self.main(cluster)
        self.assertEqual(code, 1)
        self.assert_deleted(cluster)
        self.assertEqual(record['cleanup'], 'nothing created')

    def test_earlier_output_is_never_overwritten(self):
        self.out.mkdir()
        (self.out/'run.json').write_text('earlier\n', encoding='utf-8')
        cluster = Cluster(self.kubeconfig)
        code, record = self.main(cluster)
        self.assertEqual(code, 1)
        self.assertIn('already has run.json', record['error'])
        self.assertEqual(cluster.kubectl('create'), [])
        self.assertEqual((self.out/'run.json').read_text(encoding='utf-8'), 'earlier\n')

    def assert_private(self, text):
        for private in (str(self.kubeconfig), self.kubeconfig.as_posix(), 'secret-cluster', str(Path.home()),
                        Path.home().as_posix(), str(kube_run.ROOT), 'hunter2-value'):
            self.assertNotIn(private, text)

    def test_record_has_no_secret_value_or_local_path(self):
        with mock.patch.dict(os.environ, {'ACCESS_SEED_SECRET': 'hunter2-value'}):
            _, record = self.main(Cluster(self.kubeconfig))
        self.assertEqual(record['env']['ACCESS_SEED_SECRET'], 'from Secret gateflow-runtime, key ACCESS_SEED_SECRET')
        self.assert_private(self.record_text)

    def test_record_of_a_failure_has_no_local_path(self):
        cluster = Cluster(self.kubeconfig, fail='get deployment')
        code, record = self.main(cluster)
        self.assertEqual(code, 1)
        self.assertIn('<kubeconfig>', record['error'])
        self.assert_private(self.record_text)
        self.assertEqual(cluster.kubectl('create'), [])
        self.assert_deleted(cluster)


if __name__ == '__main__':
    unittest.main()
