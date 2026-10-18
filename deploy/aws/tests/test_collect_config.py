import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
import urllib.error

from loader import AWS, completed, load

collect_config = load('collect_config')

HOST = 'k8s-gateflow-gf-0123456789-0123456789.ap-southeast-1.elb.amazonaws.com'
PASSWORD = 'helm-value-password-must-not-leak'
HELM_VALUES = {'environment': 'aws', 'secretName': 'gateflow-runtime', 'database': {'password': PASSWORD},
               'note': f'postgres://admin:{PASSWORD}@db:5432/x', 'ingress': {'inboundCidrs': ['203.0.113.0/24']},
               'services': {'gateway': {'resources': {'limits': {'cpu': '1'}}}}, 'apiKey': PASSWORD}
NODES = {'items': [{'metadata': {'name': 'generator-node-1', 'labels': {
    'gateflow.io/workload': 'load-generator', 'node.kubernetes.io/instance-type': 'c6i.xlarge',
    'topology.kubernetes.io/zone': 'ap-southeast-1a', 'kubernetes.io/hostname': 'generator-node-1'}},
    'spec': {'taints': [{'key': 'gateflow.io/workload', 'value': 'load-generator', 'effect': 'NoSchedule'}]},
    'status': {'allocatable': {'cpu': '3920m', 'memory': '7Gi'}, 'conditions': [{'type': 'Ready', 'status': 'True'}]}}]}
PODS = {'items': [{'metadata': {'name': 'gf-gateway-abc'}, 'spec': {'nodeName': 'n1'}, 'status': {
    'initContainerStatuses': [{'name': 'wait', 'state': {'terminated': {'exitCode': 0}}}],
    'containerStatuses': [{'image': 'ghcr.io/bibi45c/gateflow-gateway:abc', 'imageID': 'sha256:1', 'restartCount': 2}]}}]}
BALANCERS = {'LoadBalancers': [{'DNSName': HOST, 'SecurityGroups': ['sg-0aaaaaaaaaaaaaaa1']},
                               {'DNSName': 'other.example', 'SecurityGroups': ['sg-0bbbbbbbbbbbbbbb9']}]}
RULES = [{'name': 'k8s-gateflow-gf', 'inbound': [{'IpProtocol': 'tcp', 'FromPort': 80, 'ToPort': 80,
                                                   'IpRanges': [{'CidrIp': '203.0.113.0/24'}]}]}]


class FakeTools:
    def __init__(self, fail=(), host=HOST):
        self.fail, self.host, self.commands = set(fail), host, []

    def __call__(self, command, **kwargs):
        self.commands.append(command)
        words = [word for word in command if not word.startswith('-')]
        text = ' '.join(command)
        for marker in self.fail:
            if marker in text:
                return completed(command, 1, stderr=f'{marker} failed')
        if command[0] == 'helm':
            return completed(command, 0, json.dumps(HELM_VALUES) if 'values' in words else 'NAME NAMESPACE\ngf gateflow\n')
        if command[0] == 'aws':
            return completed(command, 0, json.dumps(BALANCERS if 'describe-load-balancers' in words else RULES))
        assert command[:5] == ['kubectl', '--kubeconfig', 'kc', '--context', 'ctx'], command
        if 'go-template' in text:
            assert '-o json' not in text
            return completed(command, 0, 'gateflow-runtime ACCESS_SEED_SECRET METERING_DATABASE_URL\ngateflow-bootstrap\n')
        if 'jsonpath' in text:
            return completed(command, 0, self.host)
        if 'nodes' in words and 'json' in command:
            return completed(command, 0, json.dumps(NODES))
        if 'pods' in words and 'json' in command:
            return completed(command, 0, json.dumps(PODS))
        return completed(command, 0, f'table of {" ".join(words[1:])}\n')


class Response:
    status = 200

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


def urlopen(url, timeout):
    if url.endswith('/metrics'):
        raise urllib.error.HTTPError(url, 403, 'Forbidden', {}, io.BytesIO(b''))
    return Response()


class CollectTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.tmp = Path(directory.name)
        self.source = self.tmp/'cloud-day'
        (self.source/'reset-d2-rep1'/'20261018T000000Z').mkdir(parents=True)
        (self.source/'reset-d2-rep1'/'20261018T000000Z'/'counts.json').write_text('{}', encoding='utf-8')
        (self.source/'live-d2-a.json').write_text('{}', encoding='utf-8')
        (self.source/'live-d1-a.json').write_text('{}', encoding='utf-8')
        (self.source/'tf-output.json').write_text('{"vpc_id": {"value": "vpc-0123456789abcdef0"}}', encoding='utf-8')
        self.out = self.source/'day2'/'day2-config'

    def main(self, *extra, runner=None):
        self.runner = runner or FakeTools()
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = collect_config.main(['--kubeconfig', 'kc', '--kube-context', 'ctx', '--out', str(self.out),
                                        '--from', str(self.source), *extra], runner=self.runner, urlopen=urlopen)
        self.stderr = stderr.getvalue()
        return code

    def read(self, name):
        return json.loads((self.out/name).read_text(encoding='utf-8'))

    def test_collects_every_file(self):
        code = self.main('--terraform-output', str(self.source/'tf-output.json'))
        self.assertEqual(code, 0, self.stderr)
        self.assertEqual(self.read('collect-errors.json'), {})
        for name in ('helm-values-gf.json', 'helm-values-keda.json', 'helm-list.txt', 'nodes.json', 'nodes.txt',
                     'pods.txt', 'images.json', 'init-and-restarts.json', 'events-gateflow.txt', 'secret-keys.json',
                     'ingress.txt', 'alb-security-groups.json', 'alb-status-codes.json', 'tf-output.json'):
            self.assertTrue((self.out/name).exists(), name)
        self.assertEqual(self.read('nodes.json')[0]['labels'], {
            'gateflow.io/workload': 'load-generator', 'node.kubernetes.io/instance-type': 'c6i.xlarge',
            'topology.kubernetes.io/zone': 'ap-southeast-1a'})
        self.assertEqual(self.read('images.json'), [{'image': 'ghcr.io/bibi45c/gateflow-gateway:abc', 'imageID': 'sha256:1'}])
        self.assertEqual(self.read('init-and-restarts.json')['gf-gateway-abc'],
                         {'node': 'n1', 'init_exit_codes': {'wait': 0}, 'restarts': 2})
        self.assertEqual(self.read('alb-status-codes.json'), {'/metrics': 403, '/healthz': 200, '/readyz': 200})
        self.assertEqual(self.read('alb-security-groups.json'), RULES)
        groups = [c for c in self.runner.commands if 'describe-security-groups' in c][0]
        self.assertIn('sg-0aaaaaaaaaaaaaaa1', groups)
        self.assertNotIn('sg-0bbbbbbbbbbbbbbb9', groups)
        self.assertEqual(groups[groups.index('--region') + 1], 'ap-southeast-1')

    def test_secret_values_never_written(self):
        self.main()
        self.assertEqual(self.read('secret-keys.json'), {'gateflow-runtime': ['ACCESS_SEED_SECRET', 'METERING_DATABASE_URL'],
                                                         'gateflow-bootstrap': []})
        values = self.read('helm-values-gf.json')
        self.assertEqual((values['database']['password'], values['apiKey'], values['secretName']),
                         ('<removed>', '<removed>', '<removed>'))
        self.assertEqual(values['note'], 'postgres://<redacted>@db:5432/x')
        self.assertEqual(values['services'], HELM_VALUES['services'])
        for path in self.out.iterdir():
            self.assertNotIn(PASSWORD, path.read_text(encoding='utf-8'), path.name)

    def test_failed_step_is_recorded_and_the_others_still_run(self):
        code = self.main(runner=FakeTools(fail=['describe-load-balancers', 'get values keda']))
        self.assertEqual(code, 1)
        errors = self.read('collect-errors.json')
        self.assertEqual(sorted(errors), ['alb', 'helm'])
        self.assertIn('describe-load-balancers failed', errors['alb'])
        self.assertTrue((self.out/'nodes.json').exists())
        self.assertTrue((self.out/'secret-keys.json').exists())

    def test_ingress_without_hostname_fails_ingress_and_alb(self):
        code = self.main(runner=FakeTools(host=''))
        self.assertEqual(code, 1)
        self.assertEqual(sorted(self.read('collect-errors.json')), ['alb', 'ingress'])

    def test_copy_globs_copy_files_and_folders_of_the_day_only(self):
        code = self.main('--copy', 'live-d2-*.json', '--copy', 'reset-d2-*', '--copy', 'day*')
        self.assertEqual(code, 1)  # 'day*' only matches the folder holding the output
        self.assertTrue((self.out/'live-d2-a.json').exists())
        self.assertFalse((self.out/'live-d1-a.json').exists())
        self.assertTrue((self.out/'reset-d2-rep1'/'20261018T000000Z'/'counts.json').exists())
        self.assertEqual(list(self.read('collect-errors.json')), ['copy day*'])

    def test_helm_release_option_replaces_the_default_list(self):
        self.main('--helm-release', 'gf:gateflow')
        self.assertEqual(sorted(p.name for p in self.out.glob('helm-values-*')), ['helm-values-gf.json'])

    def test_output_inside_the_repository_must_be_ignored(self):
        self.out = AWS/'collected-config'
        code = self.main(runner=lambda c, **k: completed(c, 1))
        self.assertEqual(code, 2)
        self.assertIn('not ignored by git', self.stderr)
        self.assertFalse(self.out.exists())


if __name__ == '__main__':
    unittest.main()
