import contextlib
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
import urllib.parse

from loader import AWS, completed, load

conditions = load('conditions')

KUBE = ['kubectl', '--kubeconfig', 'kc', '--context', 'ctx']
CLUSTER = ['--kubeconfig', 'kc', '--kube-context', 'ctx']
NOW = 1_792_000_000.0
JOB = '{job="gateflow-gateway"}'


def poll(active=None, up=None, age=5.0, fail=None):
    """One Prometheus state: instance -> active gauge, instance -> up value (default: all up)."""
    active = {'a:8080': 0, 'b:8080': 0} if active is None else active
    return {'active': active, 'up': {instance: 1 for instance in active} if up is None else up, 'age': age,
            'fail': fail}


class Clock:
    def __init__(self):
        self.now, self.sleeps = 0.0, []

    def __call__(self):
        return self.now

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.now += seconds


class FakePrometheus:
    """kubectl exec into Prometheus with wget: each poll's answers come from the next state."""

    def __init__(self, clock, *states):
        self.clock, self.states, self.queries = clock, states, []

    def state(self):
        return self.states[min(len(self.clock.sleeps), len(self.states) - 1)]

    def __call__(self, command, **kwargs):
        assert command[:14] == [*KUBE, '-n', 'monitoring', 'exec', 'deploy/prometheus-server', '-c',
                                'prometheus-server', '--', 'wget', '-qO-'], command
        url = urllib.parse.urlsplit(command[-1])
        assert url.netloc == 'localhost:9090' and url.path == '/api/v1/query', url
        expression = urllib.parse.parse_qs(url.query)['query'][0]
        self.queries.append(expression)
        state = self.state()
        fail = state['fail']
        if fail == 'exec':
            return completed(command, 1, stderr='wget: server returned error: HTTP/1.1 503 Service Unavailable')
        if fail == 'json':
            return completed(command, 0, 'Service Unavailable')
        if fail == 'status':
            return completed(command, 0, json.dumps({'status': 'error', 'errorType': 'bad_data', 'error': 'parse error'}))
        if fail == 'matrix':
            return completed(command, 0, json.dumps({'status': 'success', 'data': {'resultType': 'matrix', 'result': []}}))
        if expression == f'up{JOB}':
            values = state['up'].items()
        elif expression == f'timestamp(up{JOB})':
            values = [(instance, NOW - state['age']) for instance in state['up']]
        elif expression == f'{conditions.ACTIVE}{JOB}':
            values = state['active'].items()
            if fail == 'nan':
                values = [(instance, 'NaN') for instance, _ in values]
        else:
            raise AssertionError(expression)
        result = [{'metric': {'instance': instance, 'job': 'gateflow-gateway'}, 'value': [NOW, str(value)]}
                  for instance, value in values]
        return completed(command, 0, json.dumps({'status': 'success', 'data': {'resultType': 'vector', 'result': result}}))


class Main(unittest.TestCase):
    def main(self, *argv, runner, clock=None):
        clock = clock or Clock()
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = conditions.main(list(argv), runner=runner, sleep=clock.sleep, clock=clock)
        self.stdout, self.stderr = stdout.getvalue(), stderr.getvalue()
        return code


class DrainTests(Main):
    def drain(self, *states, extra=()):
        self.clock = Clock()
        self.prometheus = FakePrometheus(self.clock, *states)
        return self.main('drain', *CLUSTER, *extra, runner=self.prometheus, clock=self.clock)

    def test_drained_when_every_target_reports_zero(self):
        self.assertEqual(self.drain(poll()), 0)
        self.assertEqual(self.clock.sleeps, [])
        self.assertIn('drained: 0 active admitted requests on 2 gateway targets', self.stdout)
        self.assertEqual(self.prometheus.queries, [f'up{JOB}', f'timestamp(up{JOB})', f'{conditions.ACTIVE}{JOB}'])

    def test_waits_while_requests_are_active_then_drains(self):
        self.assertEqual(self.drain(poll({'a:8080': 3, 'b:8080': 1}), poll()), 0)
        self.assertEqual(self.clock.sleeps, [10])
        self.assertIn('waiting: 4 active admitted requests', self.stderr)

    def test_still_active_at_the_timeout_exits_1(self):
        self.assertEqual(self.drain(poll({'a:8080': 2, 'b:8080': 0})), 1)
        self.assertEqual(sum(self.clock.sleeps), 300)
        self.assertIn('FAIL drain: still active after 300s: 2 active admitted requests', self.stderr)

    def test_query_failure_exits_2_at_once_never_drained_or_waited_on(self):
        for fail in ('exec', 'json', 'status', 'matrix', 'nan'):
            with self.subTest(fail=fail):
                self.assertEqual(self.drain(poll(fail=fail)), 2)
                self.assertEqual(self.clock.sleeps, [])
                self.assertIn('FAIL drain: query failed', self.stderr)
                self.assertNotIn('drained', self.stdout)

    def test_query_failure_after_active_readings_still_exits_2(self):
        self.assertEqual(self.drain(poll({'a:8080': 5}), poll(fail='exec')), 2)
        self.assertEqual(self.clock.sleeps, [10])
        self.assertIn('HTTP/1.1 503', self.stderr)

    def test_gateway_without_pods_is_no_target_not_still_active(self):
        # Day 2: the gateway stayed at 0 Pods after the reset, so the query was empty.
        self.assertEqual(self.drain(poll({})), 3)
        self.assertEqual(sum(self.clock.sleeps), 60)
        self.assertIn(f'no gateway target: count(up{JOB}) = 0, so no gateway Pod is scraped', self.stderr)
        self.assertIn('the drain cannot be judged', self.stderr)
        self.assertNotIn('still active', self.stderr)

    def test_no_target_then_a_target_that_reports_zero_drains(self):
        self.assertEqual(self.drain(poll({}), poll({}), poll({'a:8080': 0})), 0)
        self.assertEqual(self.clock.sleeps, [10, 10])

    def test_target_wait_restarts_after_a_measurable_reading(self):
        states = [poll({})] * 5 + [poll({'a:8080': 1})] + [poll({})] * 6 + [poll()]
        self.assertEqual(self.drain(*states), 0)

    def test_down_stale_or_incomplete_targets_are_not_measurable(self):
        cases = [(poll(up={'a:8080': 1, 'b:8080': 0}, active={'a:8080': 0}), '1 of 2 gateway targets down: b:8080'),
                 (poll(age=120), 'the oldest gateway scrape is 120s old, more than 60s'),
                 (poll(active={'a:8080': 0}, up={'a:8080': 1, 'b:8080': 1}),
                  f'{conditions.ACTIVE} is missing on b:8080')]
        for state, message in cases:
            with self.subTest(message=message):
                self.assertEqual(self.drain(state, extra=('--target-wait', '20')), 3)
                self.assertEqual(sum(self.clock.sleeps), 20)
                self.assertIn(message, self.stderr)

    def test_targets_option_requires_exactly_n_targets(self):
        self.assertEqual(self.drain(poll({'a:8080': 0}), extra=('--targets', '2')), 3)
        self.assertIn(f'count(up{JOB}) = 1, expected 2 gateway targets', self.stderr)
        self.assertEqual(self.drain(poll({'a:8080': 0}), extra=('--targets', '1')), 0)

    def test_job_and_prometheus_options(self):
        clock = Clock()
        commands = []

        def runner(command, **kwargs):
            commands.append(command)
            return completed(command, 1)
        self.main('drain', *CLUSTER, '--job', 'other', '--monitoring-namespace', 'mon', '--prometheus', 'prom',
                  '--prometheus-container', 'server', runner=runner, clock=clock)
        self.assertEqual(commands[0][5:12], ['-n', 'mon', 'exec', 'deploy/prom', '-c', 'server', '--'])
        query = urllib.parse.parse_qs(urllib.parse.urlsplit(commands[0][-1]).query)['query']
        self.assertEqual(query, ['up{job="other"}'])


class FakeKube:
    """The gateway Deployment and Pods; each poll uses the next (ready, pods) pair."""

    def __init__(self, clock, *states, fail=(), minimum=2):
        self.clock, self.states, self.fail, self.minimum = clock, states, set(fail), minimum
        self.commands = []

    def __call__(self, command, **kwargs):
        self.commands.append(command)
        if command[0] != 'kubectl':
            return completed(command, 1 if command[0] in self.fail or any(f in command for f in self.fail) else 0)
        words = command[7:]
        if words[0] in self.fail or (len(words) > 1 and words[1] in self.fail):
            return completed(command, 1, stderr='Unable to connect to the server')
        ready, pods = self.states[min(len(self.clock.sleeps), len(self.states) - 1)]
        if words[:2] == ['get', 'deployment']:
            return completed(command, 0, json.dumps({'spec': {'replicas': 0}, 'status': {
                'readyReplicas': ready, 'conditions': [{'type': 'Available', 'status': 'False', 'reason': 'MinimumReplicasUnavailable'}]}}))
        if words[:2] == ['get', 'pods']:
            names = [f'gf-gateway-{i}' for i in range(pods)] + ['gf-access-0', 'gf-metering-0']
            return completed(command, 0, json.dumps({'items': [{'metadata': {'name': n}} for n in names]}))
        if words[:2] == ['get', 'scaledobject.keda.sh']:
            return completed(command, 0, json.dumps({'spec': {'minReplicaCount': self.minimum}}))
        if words[:2] == ['get', 'horizontalpodautoscalers']:
            return completed(command, 0, json.dumps({'items': [{'metadata': {'name': 'keda-hpa-gf-gateway'},
                                                                'status': {'currentReplicas': 0, 'desiredReplicas': 0}}]}))
        if words[:2] == ['get', 'scaledobjects.keda.sh']:
            return completed(command, 0, json.dumps({'items': [{'metadata': {'name': 'gf-gateway', 'annotations': {}},
                                                                'status': {'conditions': [{'type': 'Ready', 'status': 'False',
                                                                                           'reason': 'ScalerFailed'}]}}]}))
        if words[:2] == ['get', 'events']:
            return completed(command, 0, 'LAST SEEN TYPE REASON\n1m Warning KEDAScalerFailed\n')
        if words[:2] == ['rollout', 'status']:
            return completed(command, 0)
        raise AssertionError(command)


class SettleTests(Main):
    def test_settled_when_ready_and_pods_both_match(self):
        clock = Clock()
        code = self.main('settle', *CLUSTER, '--replicas', '2', runner=FakeKube(clock, (2, 3), (2, 2)), clock=clock)
        self.assertEqual(code, 0)
        self.assertEqual(clock.sleeps, [10])  # a third, terminating Pod still counted
        self.assertIn('settled at 2: 2 ready, 2 gateway Pods', self.stdout)

    def test_not_settled_prints_why(self):
        clock = Clock()
        code = self.main('settle', *CLUSTER, '--replicas', '2', '--settle-timeout', '30', runner=FakeKube(clock, (0, 0)),
                         clock=clock)
        self.assertEqual(code, 1)
        self.assertEqual(sum(clock.sleeps), 30)
        for text in ('not settled at 2 after 30s: 0 ready, 0 gateway Pods', 'Deployment gf-gateway: spec.replicas 0',
                     'MinimumReplicasUnavailable', 'horizontalpodautoscalers keda-hpa-gf-gateway: current 0, desired 0',
                     'scaledobjects.keda.sh gf-gateway: paused-replicas None; Ready=False ScalerFailed',
                     'latest events:', 'KEDAScalerFailed'):
            self.assertIn(text, self.stderr)

    def test_query_failure_exits_2(self):
        clock = Clock()
        code = self.main('settle', *CLUSTER, '--replicas', '2', runner=FakeKube(clock, (2, 2), fail=['pods']), clock=clock)
        self.assertEqual((code, clock.sleeps), (2, []))
        self.assertIn('FAIL settle: query failed: get pods', self.stderr)


class LevelsTests(Main):
    def levels(self, *argv):
        code = self.main('levels', *argv, runner=None)
        return code, self.stdout.strip()

    def test_fixed_ladders_scale_with_the_replicas(self):
        self.assertEqual(self.levels('fixed', '--replicas', '1', '--level', '40'), (0, '32,40,46,52'))
        self.assertEqual(self.levels('fixed', '--replicas', '2', '--level', '40'), (0, '64,80,92,104'))
        self.assertEqual(self.levels('fixed', '--replicas', '4', '--level', '40'), (0, '128,160,184,208'))
        self.assertEqual(self.levels('fixed', '--replicas', '1', '--level', '40', '--factors', '1,2'), (0, '40,80'))

    def test_auto_ladder_lowers_repeated_levels(self):
        self.assertEqual(self.levels('auto', '--target', '575', '--duration', '21.0072'), (0, '41,68,96,67,40'))

    def test_auto_ladder_without_a_positive_level_fails(self):
        self.assertEqual(self.levels('auto', '--target', '10', '--duration', '20')[0], 2)
        self.assertIn('no positive level', self.stderr)


class UpgradeTests(Main):
    def upgrade(self, *argv, fail=(), states=((2, 2),)):
        clock = Clock()
        self.kube = FakeKube(clock, *states, fail=fail)
        return self.main('upgrade', *CLUSTER, *argv, runner=self.kube, clock=clock)

    def test_fixed_refreshes_the_secrets_upgrades_and_waits_for_the_rollout(self):
        code = self.upgrade('--terraform-output', 'tf.json', '--values', 'a.yaml', '--values', 'b.yaml', '--fixed', '4')
        self.assertEqual(code, 0)
        refresh, helm, rollout = self.kube.commands
        self.assertEqual(refresh[:2], [sys.executable, str(AWS/'secrets.py')])
        self.assertEqual(refresh[-3:], ['--terraform-output', 'tf.json', '--refresh-master-only'])
        self.assertEqual(helm, ['helm', '--kubeconfig', 'kc', '--kube-context', 'ctx', 'upgrade', 'gf',
                                str(AWS.parents[1]/'deploy'/'helm'/'gateflow'), '-n', 'gateflow', '-f', 'a.yaml',
                                '-f', 'b.yaml', '--reuse-values', '--set', 'autoscaling.enabled=false', '--set',
                                'services.gateway.replicas=4', '--wait', '--wait-for-jobs', '--timeout', '15m'])
        self.assertEqual(rollout[5:], ['-n', 'gateflow', 'rollout', 'status', 'deploy/gf-gateway', '--timeout=600s'])

    def test_auto_sets_t_and_d_as_written_and_settles_at_the_minimum(self):
        code = self.upgrade('--auto', '--target', '575', '--duration', '21.0072', states=((1, 1), (2, 2)))
        self.assertEqual(code, 0)
        helm = self.kube.commands[0]
        self.assertEqual(helm[helm.index('-f') + 1], str(AWS.parents[1]/'deploy'/'helm'/'values-aws.yaml'))
        self.assertIn('autoscaling.targetPerReplica=575', helm)
        self.assertIn('autoscaling.requestDurationSeconds=21.0072', helm)
        self.assertIn('settled at 2', self.stdout)

    def test_failed_step_stops_the_upgrade(self):
        self.assertEqual(self.upgrade('--terraform-output', 'tf.json', '--fixed', '1', fail=[sys.executable]), 1)
        self.assertEqual(len(self.kube.commands), 1)
        self.assertEqual(self.upgrade('--fixed', '1', fail=['helm']), 1)
        self.assertEqual(len(self.kube.commands), 1)
        self.assertIn('FAIL helm upgrade: exited with 1', self.stderr)

    def test_auto_needs_t_and_d(self):
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            conditions.parse_args(['upgrade', *CLUSTER, '--auto', '--target', '575'])
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            conditions.parse_args(['upgrade', *CLUSTER, '--auto', '--target', '575', '--duration', '0'])


class RunTests(Main):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.tmp = Path(directory.name)

    def run_load(self, mode, *extra, kube_run=0, export=0, record=True):
        commands = []

        def runner(command, **kwargs):
            commands.append(command)
            if command[1].endswith('kube_run.py'):
                folder = Path(command[command.index('--out-dir') + 1])
                if record:
                    folder.mkdir(parents=True, exist_ok=True)
                    (folder/'run-record.json').write_text('{}', encoding='utf-8')
                    (folder/'run.results.json').write_text(json.dumps({'checks': {'dropped_iterations': 0}, 'phases': [
                        {'phase': 'hold', 'level_rps': 40.0, 'scheduled': 1000, 'completed': 995, 'ttfb_ms': {'p95': 12.5},
                         'max_active_vus': 900},
                        {'phase': 'hold', 'level_rps': 46.0, 'scheduled': 1000, 'completed': 994, 'ttfb_ms': {}}]}),
                        encoding='utf-8')
                kwargs['stdout'].write('k6 exit code 0, summarize exit code 0, cleanup deleted\n')
                return completed(command, kube_run)
            return completed(command, export)
        code = self.main('run', *CLUSTER, '--name', 'd3-rep1-r1', '--mode', mode, '--levels', '32,40', '--runs',
                         str(self.tmp/'runs'), '--log', str(self.tmp/'d3-rep1-r1.log'), *extra, runner=runner)
        return code, commands

    def test_fixed_run_exports_without_hpa_queries_and_prints_the_verdicts(self):
        code, (kube_run, export) = self.run_load('fixed', '--', '--k6-cpu', '4', '--env', 'MAX_VUS=12000')
        self.assertEqual(code, 0)
        self.assertEqual(kube_run[-6:], ['--env', 'LEVELS=32,40', '--k6-cpu', '4', '--env', 'MAX_VUS=12000'])
        self.assertIn(str(self.tmp/'runs'/'d3-rep1-r1'), kube_run)
        self.assertEqual(export[-4:], ['--skip', 'hpa_desired', '--skip', 'hpa_conditions'])
        self.assertIn('kube_run.py exit 0: k6 exit code 0, summarize exit code 0, cleanup deleted', self.stdout)
        self.assertIn(' hold 40 995/1000 pass ttfb_p95 12.5 vus 900', self.stdout)
        self.assertIn(' hold 46 994/1000 FAIL ttfb_p95 None vus None', self.stdout)

    def test_auto_run_keeps_the_hpa_queries(self):
        code, (_, export) = self.run_load('auto')
        self.assertEqual(code, 0)
        self.assertNotIn('--skip', export)

    def test_exit_code_is_kube_runs_then_prom_exports(self):
        self.assertEqual(self.run_load('fixed', kube_run=2, export=1)[0], 2)
        self.assertEqual(self.run_load('fixed', export=2)[0], 2)

    def test_no_record_means_no_export(self):
        code, commands = self.run_load('fixed', kube_run=1, record=False)
        self.assertEqual((code, len(commands)), (1, 1))
        self.assertIn('nothing to export', self.stderr)


if __name__ == '__main__':
    unittest.main()
