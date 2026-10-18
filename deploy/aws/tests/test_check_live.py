import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest

from loader import AWS, completed, load

check_live = load('check_live')

TAG = '0123456789abcdef0123456789abcdef01234567'
EXPECT_250M = AWS/'expect'/'gateway-250m.json'
EXPECT_1VCPU = AWS/'expect'/'gateway-1vcpu.json'
FIXED = ('--replicas', '1')
AUTO = ('--autoscale', '2', '4', '--target', '2437', '--duration', '22.48')


def deployment(name, replicas, want, tag=TAG):
    resources = {kind: dict(want[kind]) for kind in ('requests', 'limits') if kind in want}
    return {'metadata': {'name': name}, 'spec': {'replicas': replicas, 'template': {'spec': {'containers': [
        {'image': f'ghcr.io/bibi45c/{name.replace("gf-", "gateflow-")}:{tag}', 'resources': resources}]}}},
        'status': {'readyReplicas': replicas, 'updatedReplicas': replicas, 'availableReplicas': replicas}}


def pod(name, restarts=0):
    return {'metadata': {'name': name}, 'status': {'phase': 'Running', 'containerStatuses': [{'restartCount': restarts}]}}


def scaledobject(threshold='2437', duration='22.48', trigger2='eligible-demand'):
    def trigger(name, query):
        return {'type': 'prometheus', 'name': name, 'metricType': 'AverageValue', 'metadata': {
            'serverAddress': 'http://prometheus-server.monitoring.svc.cluster.local', 'query': query,
            'threshold': threshold, 'ignoreNullValues': 'false'}}
    return {'metadata': {'name': 'gf-gateway'}, 'spec': {
        'scaleTargetRef': {'name': 'gf-gateway'}, 'minReplicaCount': 2, 'maxReplicaCount': 4,
        'advanced': {'horizontalPodAutoscalerConfig': {'behavior': {'scaleDown': {'stabilizationWindowSeconds': 60}}}},
        'triggers': [trigger('active-admitted', 'sum(gateflow_gateway_active_admitted_requests{job="gateflow-gateway"}) and on() x'),
                     trigger(trigger2, 'sum(rate(gateflow_gateway_eligible_arrivals_total{job="gateflow-gateway"}[1m]))'
                                       f' * {duration} and on() x')]}}


def cluster(gateway=1, auto=False, expect=EXPECT_250M):
    """A release that matches the expect file, with the given number of gateway Pods."""
    wanted = json.loads(expect.read_text(encoding='utf-8'))['deployments']
    deployments, pods = [], []
    for service, want in wanted.items():
        replicas = gateway if service == 'gateway' else want['replicas']
        deployments.append(deployment(f'gf-{service}', replicas, want))
        pods += [pod(f'gf-{service}-{i}') for i in range(replicas)]
    return {'deployments': deployments, 'pods': pods, 'scaledobjects.keda.sh': [scaledobject()] if auto else []}


class FakeKubectl:
    """Serves one scenario: {kind: items}, and {'fail': {kind: stderr}} or {'garbled': [kind]}."""

    def __init__(self, scenario):
        self.scenario, self.commands = scenario, []

    def __call__(self, command, **kwargs):
        self.commands.append(command)
        assert command[:7] == ['kubectl', '--kubeconfig', 'kc', '--context', 'ctx', '-n', 'gateflow'], command
        kind = command[command.index('get') + 1]
        if kind in self.scenario.get('fail', {}):
            return completed(command, 1, stderr=self.scenario['fail'][kind])
        if kind in self.scenario.get('garbled', []):
            return completed(command, 0, stdout='<html>proxy error</html>')
        return completed(command, 0, stdout=json.dumps({'items': self.scenario[kind]}))


class CheckLive(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.tmp = Path(directory.name)

    def run_check(self, scenario, *args, expect=EXPECT_250M, out='out.json'):
        out = self.tmp/out
        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(io.StringIO()):
            code = check_live.main(['--kubeconfig', 'kc', '--kube-context', 'ctx', '--out', str(out),
                                    '--expect', str(expect), '--image-tag', TAG, *args], runner=FakeKubectl(scenario))
        self.stdout = stdout.getvalue()
        return code, json.loads(out.read_text(encoding='utf-8')) if out.exists() else None

    def test_correct_fixed_and_auto_pass(self):
        self.assertEqual(self.run_check(cluster(), *FIXED)[0], 0)
        self.assertEqual(self.run_check(cluster(4), '--replicas', '4')[0], 0)
        self.assertEqual(self.run_check(cluster(2, auto=True), *AUTO)[0], 0)
        self.assertIn('live configuration as expected', self.stdout)

    def test_both_expect_examples_match_their_configuration(self):
        code, out = self.run_check(cluster(expect=EXPECT_1VCPU), *FIXED, expect=EXPECT_1VCPU)
        self.assertEqual((code, out['problems']), (0, []))
        self.assertEqual(out['deployments']['gf-gateway']['resources']['limits'], {'cpu': '1', 'memory': '2Gi'})
        # The 1 vCPU configuration's dependencies are not the 0.25 vCPU one's.
        code, out = self.run_check(cluster(expect=EXPECT_1VCPU), *FIXED, expect=EXPECT_250M)
        self.assertEqual(code, 1)
        self.assertIn('gf-access: spec.replicas 1 != 3', out['problems'])

    def test_query_failures_block(self):  # R1
        for kind in ('scaledobjects.keda.sh', 'pods', 'deployments'):
            s = cluster()
            s['fail'] = {kind: 'Error from server (Forbidden)'}
            code, out = self.run_check(s, *FIXED)
            self.assertEqual(code, 2, kind)
            self.assertTrue(out['errors'])
            self.assertEqual(out['problems'], ['query failed; nothing was checked'])

    def test_unparsable_answer_is_a_query_failure_not_no_objects(self):
        s = cluster()
        s['garbled'] = ['scaledobjects.keda.sh']
        code, out = self.run_check(s, *FIXED)
        self.assertEqual(code, 2)
        self.assertIn('unparsable output', out['errors'][0])

    def test_wrong_dependency_replicas_block(self):  # R2 counterexample 1
        s = cluster()
        for d in s['deployments']:
            if d['metadata']['name'] in ('gf-access', 'gf-metering'):
                d['spec']['replicas'] = 1
                d['status'] = {'readyReplicas': 1, 'updatedReplicas': 1, 'availableReplicas': 1}
        self.assertEqual(self.run_check(s, *FIXED)[0], 1)

    def test_wrong_image_tag_blocks(self):  # R2 counterexample 2
        s = cluster()
        for d in s['deployments']:
            c = d['spec']['template']['spec']['containers'][0]
            c['image'] = c['image'].rsplit(':', 1)[0] + ':' + 'f' * 40
        self.assertEqual(self.run_check(s, *FIXED)[0], 1)

    def test_wrong_scaling_parameters_block(self):  # R2 counterexample 3
        for so in (scaledobject(threshold='999999'), scaledobject(trigger2='other'), scaledobject(duration='20')):
            s = cluster(2, auto=True)
            s['scaledobjects.keda.sh'] = [so]
            self.assertEqual(self.run_check(s, *AUTO)[0], 1)

    def test_fixed_run_with_scaledobject_or_wrong_resources_blocks(self):
        self.assertEqual(self.run_check(cluster(1, auto=True), *FIXED)[0], 1)
        s = cluster()
        s['deployments'][0]['spec']['template']['spec']['containers'][0]['resources']['limits']['cpu'] = '1'
        self.assertEqual(self.run_check(s, *FIXED)[0], 1)

    def test_missing_deployment_and_rollout_in_progress_block(self):
        s = cluster()
        s['deployments'] = [d for d in s['deployments'] if d['metadata']['name'] != 'gf-metering']
        code, out = self.run_check(s, *FIXED)
        self.assertEqual(code, 1)
        self.assertIn('gf-metering: missing', out['problems'])
        s = cluster()
        s['deployments'][0]['status']['updatedReplicas'] = 0
        self.assertEqual(self.run_check(s, *FIXED)[0], 1)

    def test_auto_not_settled_blocks(self):
        s = cluster(3, auto=True)
        self.assertEqual(self.run_check(s, *AUTO)[0], 1)

    def test_restart_growth_against_baseline_blocks(self):
        code, before = self.run_check(cluster(), *FIXED, out='before.json')
        self.assertEqual(code, 0)
        baseline = ('--baseline', str(self.tmp/'before.json'))
        s = cluster()
        s['pods'][0] = pod('gf-gateway-0', restarts=1)
        self.assertEqual(self.run_check(s, *FIXED, *baseline)[0], 1)
        self.assertEqual(self.run_check(cluster(), *FIXED, *baseline)[0], 0)

    def test_replaced_pods_block_a_fixed_run_only(self):
        self.run_check(cluster(), *FIXED, out='before.json')
        s = cluster()
        s['pods'][0] = pod('gf-gateway-new')
        code, out = self.run_check(s, *FIXED, '--baseline', str(self.tmp/'before.json'))
        self.assertEqual(code, 1)
        self.assertIn("Pods replaced since baseline: ['gf-gateway-0']", out['problems'])

    def test_unreadable_expect_file_exits_2_without_querying(self):
        runner = FakeKubectl(cluster())
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            code = check_live.main(['--kubeconfig', 'kc', '--kube-context', 'ctx', '--out', str(self.tmp/'out.json'),
                                    '--expect', str(self.tmp/'missing.json'), '--image-tag', TAG, *FIXED], runner=runner)
        self.assertEqual((code, runner.commands), (2, []))

    def test_autoscale_needs_target_and_duration(self):
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            check_live.parse_args(['--kubeconfig', 'kc', '--kube-context', 'ctx', '--out', 'o', '--expect', 'e',
                                   '--image-tag', TAG, '--autoscale', '2', '4'])

    def test_state_records_release_config_and_no_local_path(self):
        code, out = self.run_check(cluster(), *FIXED)
        self.assertEqual((out['expect'], out['release'], out['namespace']), ('gateway-250m.json', 'gf', 'gateflow'))
        self.assertNotIn(str(self.tmp), json.dumps(out))


if __name__ == '__main__':
    unittest.main()
