"""Unit tests of bundle.py, on a day of run folders built here; no AWS call, no local data."""
import contextlib
import csv
from datetime import datetime, timezone
import gzip
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock
import zipfile

import bundle

DATE = '2026-10-19'
START = datetime(2026, 10, 19, 10, 0, 0, tzinfo=timezone.utc).timestamp()
COMMIT = '0123456789abcdef' * 2 + '01234567'
PLAN = [('warmup', 10, 0, 60), ('hold', 10, 60, 240), ('ramp', 20, 240, 300), ('hold', 20, 300, 480)]
CHECKS = {'wallet_empty_responses': 0, 'invalid_key_responses': 0, 'dropped_iterations': 0, 'dropped_outside_plan': 0,
          'unclassified_requests': 0, 'excess_results': 0}
RUNS = {'d9-rep1-r1': {'condition': 'rep1', 'repeat': 1, 'config': 'cfg-D', 'purpose': 'fixed fixture', 'notes': ''},
        'd9-auto-r1': {'condition': 'auto', 'repeat': 1, 'config': 'cfg-D', 'purpose': 'auto fixture', 'notes': ''},
        'd9-calib-r1': {'condition': 'calib', 'repeat': 1, 'config': 'cfg-D', 'purpose': 'calibration',
                        'notes': 'aborted by the operator'}}
RDS = {'CPUUtilization': 41.3, 'DatabaseConnections': 8.0, 'CPUCreditBalance': 100.0, 'CPUSurplusCreditBalance': 0.0,
       'CPUSurplusCreditsCharged': 2.0}
HAS_MATPLOTLIB = importlib.util.find_spec('matplotlib') is not None


def iso(offset):
    return datetime.fromtimestamp(START + offset, timezone.utc).isoformat(timespec='milliseconds').replace('+00:00', 'Z')


def matrix(series):
    """{labels: value} -> a query_range answer with a constant value every 15 s of the run."""
    return {'status': 'success', 'data': {'resultType': 'matrix', 'result': [
        {'metric': dict(labels), 'values': [[START + t, str(value)] for t in range(0, 541, 15)]}
        for labels, value in series]}}


def live(cpu, auto):
    so = [{'name': 'gf-gateway', 'scaleTargetRef': {'name': 'gf-gateway'}, 'minReplicaCount': 2, 'maxReplicaCount': 4,
           'triggers': [{'name': 'active-admitted', 'metadata': {'threshold': '420', 'query': 'sum(x) and on() y'}},
                        {'name': 'eligible-demand', 'metadata': {'threshold': '420',
                                                                 'query': 'sum(rate(z[1m])) * 22 and on() y'}}]}]
    return {'errors': [], 'problems': [], 'release': 'gf', 'scaledobjects': so if auto else [],
            'deployments': {'gf-gateway': {'resources': {'limits': {'cpu': cpu, 'memory': '1Gi'}}}}}


class FakeAws:
    """sts and CloudWatch; a metric in fail, or 'sts', exits non-zero."""

    def __init__(self, fail=()):
        self.fail, self.commands = set(fail), []

    def __call__(self, command, **kwargs):
        self.commands.append(command)
        assert command[0] == 'aws' and command[-2:] == ['--output', 'json'], command
        if command[1:3] == ['sts', 'get-caller-identity']:
            if 'sts' in self.fail:
                return subprocess.CompletedProcess(command, 255, '', 'Unable to locate credentials')
            return subprocess.CompletedProcess(command, 0, '{"UserId": "fake"}', '')
        assert command[1:3] == ['cloudwatch', 'get-metric-statistics'], command
        metric, stat = command[command.index('--metric-name') + 1], command[command.index('--statistics') + 1]
        if metric in self.fail:
            return subprocess.CompletedProcess(command, 254, '', 'An error occurred (ExpiredToken)')
        value = RDS[metric]
        points = [{'Timestamp': '2026-10-19T10:02:00Z', stat: value / 2}, {'Timestamp': '2026-10-19T10:01:00Z', stat: value}]
        return subprocess.CompletedProcess(command, 0, json.dumps({'Datapoints': points}), '')


class Day(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.tmp = Path(directory.name)
        self.evidence, self.runs = self.tmp/'cloud-day', self.tmp/'loadtest'
        self.out = self.evidence/'day9'
        config = self.out/'day9-config'
        config.mkdir(parents=True)
        self.write(config/'images.json', [{'image': 'ghcr.io/bibi45c/gateflow-gateway:x', 'imageID': 'sha256:gw'}])
        self.write(config/'nodes.json', [{'name': 'gen', 'labels': {'gateflow.io/workload': 'load-generator'},
                                          'taints': [], 'allocatable': {'cpu': '7910m'}, 'ready': True}])
        self.write(config/'tf-output.json', {'database_address': {'value': 'gateflow-s3.example.ap-southeast-1.rds.amazonaws.com'}})
        self.run_folder('d9-rep1-r1', auto=False)
        self.run_folder('d9-auto-r1', auto=True)
        (self.runs/'d9-calib-r1').mkdir(parents=True)
        (self.runs/'d9-calib-r1'/'ABORTED.txt').write_text('stopped by the operator\n', encoding='utf-8')
        self.write(self.evidence/'live-d9-rep1-r1.json', live('250m', auto=False))
        self.write(self.evidence/'live-d9-auto-r1.json', live('250m', auto=True))
        self.write(self.evidence/'derived-day9.json', {'runs': [], 'T': None, 'D_s': None})
        self.write(self.evidence/'inventory.json', {})
        self.manifest = {'day': '9', 'date': DATE, 'commit': COMMIT, 'derived': 'derived-day9.json',
                         'generator_allocatable_cpu': 0, 'gateway_resources': {'cpu': '1', 'memory': '2Gi'},
                         'configs': {'cfg-D': 'fixture'}, 'extra_files': ['inventory.json', 'leftovers-day9.txt'],
                         'database_reset': '`deploy/aws/reset_db.py --balance 1000000000000` before each condition',
                         'runs': json.loads(json.dumps(RUNS))}
        self.write_manifest()

    def write(self, path, data):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(data), encoding='utf-8')

    def write_manifest(self):
        self.write(self.evidence/'day9-manifest.json', self.manifest)

    def run_folder(self, name, auto):
        folder = self.runs/name
        replicas = 2 if auto else 1
        phases = [{'phase': phase, 'level_rps': float(level), 'start_s': float(start), 'end_s': float(end),
                   'start': iso(start), 'end': iso(end)} for phase, level, start, end in PLAN]
        self.write(folder/'run-record.json', {
            'name': name, 'time': iso(-120)[:19] + 'Z', 'op_start': iso(-120), 'load_start': iso(0),
            'plan_end': iso(480), 'load_end': iso(545), 'op_end': iso(600), 'phases': phases, 'commit': COMMIT,
            'release': 'gf', 'namespace': 'gateflow', 'pod': f'gateflow-k6-{name}', 'k6_image': 'grafana/k6@sha256:' + '0' * 64,
            'resources': {'cpu': '4', 'memory': '16Gi'}, 'k6_exit_code': 0, 'summarize_exit_code': 0, 'error': None,
            'env': {'LEVELS': '10,20', 'MAX_VUS': '12000', 'ACCESS_SEED_SECRET': 'from Secret gateflow-runtime'},
            'gateway': {'replicas': replicas, 'ready_replicas': replicas, 'image': 'gw'},
            'gateway_after': {'replicas': replicas, 'ready_replicas': replicas, 'image': 'gw'}})
        rows = [{'phase': phase, 'level_rps': float(level), 'scheduled': 1000,
                 'completed': 990 if (phase, level, auto) == ('hold', 20, False) else 1000, 'throughput_rps': 5.0,
                 'ttfb_ms': {'p50': 10.0, 'p95': 20.0}, 'max_active_vus': 300, 'dropped': 0}
                for phase, level, _, _ in PLAN]
        self.write(folder/'run.results.json', {'phases': rows, 'checks': CHECKS, 'sent_source': 'gateflow_sent'})
        (folder/'summary.md').write_text('| phase | level |\n|---|---|\n| hold | 10 |\n', encoding='utf-8')
        (folder/'run.json').write_text('{"metric": "gateflow_start"}\n', encoding='utf-8')
        prom = {
            'cpu_by_pod': [({'pod': 'gf-gateway-a'}, 0.2), ({'pod': 'gateflow-k6-x'}, 2.0),
                           ({'pod': 'gf-mock-upstream-y'}, 1.0), ({'pod': 'gf-access-z'}, 0.1), ({'pod': 'gf-metering-w'}, 0.05)],
            'mem_by_pod': [({'pod': 'gf-gateway-a'}, 500 * 2**20), ({'pod': 'gateflow-k6-x'}, 4 * 2**30)],
            'net_by_pod': [({'pod': 'gf-gateway-a'}, 1e6)], 'active': [({}, 30)], 'arrivals': [({}, 10)],
            'results': [({'result': 'completed'}, 9.9), ({'result': 'timed_out'}, 0.1)],
            'metering_p95': [({'route': '/v1/reserve'}, 0.05)], 'first_output_p95': [({}, 0.03)],
            'replicas': [({}, replicas)]}
        if auto:
            prom['hpa_desired'] = [({}, 2)]
        for series, values in prom.items():
            self.write(folder/'prom'/f'{series}.json', matrix(values))

    def build(self, *argv, runner=None, pyplot=None):
        self.aws = runner or FakeAws()
        stdout, stderr = io.StringIO(), io.StringIO()
        with mock.patch.object(bundle, 'load_pyplot', return_value=pyplot) if pyplot is not True else contextlib.nullcontext(), \
                contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = bundle.main(['--manifest', str(self.evidence/'day9-manifest.json'), '--runs', str(self.runs),
                                '--evidence', str(self.evidence), *argv], runner=self.aws)
        self.stdout, self.stderr = stdout.getvalue(), stderr.getvalue()
        return code

    def summary(self, run):
        return (self.out/run/'summary.md').read_text(encoding='utf-8')

    def rows(self):
        with open(self.out/'day9-runs.csv', encoding='utf-8') as stream:
            return {item['run_id']: item for item in csv.DictReader(stream)}


class BuildTests(Day):
    def test_build_succeeds_with_every_output(self):
        self.assertEqual(self.build(), 0, self.stderr)
        for run in ('d9-rep1-r1', 'd9-auto-r1'):
            for name in ('results.json', 'run-record.json', 'summary.md', 'cloudwatch-rds.json', 'prom/active.json'):
                self.assertTrue((self.out/run/name).exists(), f'{run}/{name}')
            with gzip.open(self.out/run/'k6.json.gz', 'rt', encoding='utf-8') as stream:
                self.assertEqual(stream.read(), '{"metric": "gateflow_start"}\n')
        self.assertEqual(sorted(p.name for p in (self.out/'d9-calib-r1').iterdir()), ['ABORTED.txt'])
        for name in ('manifest.json', 'configs.json', 'inventory.json', 'nodes.json'):
            self.assertTrue((self.out/'day9-config'/name).exists(), name)
        self.assertIn('extra file leftovers-day9.txt not found', self.stdout)
        sums = (self.out/'SHA256SUMS').read_text(encoding='utf-8').splitlines()
        self.assertIn('d9-rep1-r1/summary.md', [line.split('  ')[1] for line in sums])
        with zipfile.ZipFile(self.evidence/'day9.zip') as archive:
            names = archive.namelist()
        self.assertIn('day9/d9-auto-r1/summary.md', names)
        self.assertIn('day9/SHA256SUMS', names)

    def test_csv_fixed_and_auto_semantics(self):
        self.assertEqual(self.build(), 0, self.stderr)
        rows = self.rows()
        fixed, auto, aborted = rows['d9-rep1-r1'], rows['d9-auto-r1'], rows['d9-calib-r1']
        self.assertEqual(fixed['replicas_fixed'], '1')
        self.assertEqual((fixed['autoscale_min'], fixed['autoscale_max']), ('', ''))
        self.assertEqual(auto['replicas_fixed'], '')
        self.assertEqual((auto['autoscale_min'], auto['autoscale_max'], auto['T'], auto['D_s']), ('2', '4', '420', '22'))
        self.assertEqual((fixed['highest_passing_level_rps'], fixed['first_violated_level_rps'], fixed['boundary_type']),
                         ('10.0', '20.0', 'completion'))
        self.assertEqual((fixed['commit'], fixed['gateway_digest'], fixed['levels']), (COMMIT, 'sha256:gw', '10,20'))
        self.assertEqual((aborted['valid'], aborted['notes'], aborted['commit']), ('no', 'aborted by the operator', ''))

    def test_summaries_carry_live_resources_not_the_manifest_default(self):
        self.assertEqual(self.build(), 0, self.stderr)
        for run in ('d9-rep1-r1', 'd9-auto-r1'):
            text = self.summary(run)
            self.assertIn('250m CPU request = limit', text)
            self.assertIn('1Gi limit', text)
            self.assertNotIn('1-vCPU', text)
            self.assertNotIn('2 GiB', text)
            self.assertNotIn('2Gi limit', text)
            self.assertIn(f'live-{run}.json', text)
            self.assertIn('target `http://gf-gateway:8080`', text)
            self.assertIn('| Database reset before this run | `deploy/aws/reset_db.py --balance 1000000000000`', text)
        auto = self.summary('d9-auto-r1')
        self.assertIn('KEDA min 2, max 4, T 420, D 22 s', auto)
        self.assertNotIn('| Gateway replicas | fixed', auto)
        self.assertIn('| Gateway replicas | fixed 1 (after: 1)', self.summary('d9-rep1-r1'))

    def test_live_pattern_and_manifest_fallback(self):
        self.manifest['live_pattern'] = 'live-day9-{run}.json'
        self.manifest['runs']['d9-rep1-r1']['gateway_resources'] = {'cpu': '500m', 'memory': '1Gi'}
        self.manifest['runs']['d9-auto-r1']['autoscaling'] = {'min': 2, 'max': 3, 'T': '100', 'D': '20'}
        self.write_manifest()
        self.assertEqual(self.build(), 0, self.stderr)
        self.assertIn('500m CPU request = limit', self.summary('d9-rep1-r1'))
        self.assertIn('(manifest)', self.summary('d9-rep1-r1'))
        self.assertIn('KEDA min 2, max 3, T 100, D 20 s', self.summary('d9-auto-r1'))

    def test_generator_allocatable_from_nodes(self):
        self.assertEqual(self.build(), 0, self.stderr)
        text = self.summary('d9-rep1-r1')
        self.assertIn('of 6.33 cores', text)  # 0.8 x 7.91
        self.assertIn('k6 + mock CPU peak 3.00', text)
        self.assertIn('Generator criterion met: yes', text)

    def test_rds_values_come_from_cloudwatch(self):
        self.assertEqual(self.build(), 0, self.stderr)
        self.assertIn('| 41.3 %; 8.0; 50.0; 0.0; 3.0 | CloudWatch', self.summary('d9-rep1-r1'))
        raw = json.loads((self.out/'d9-rep1-r1'/'cloudwatch-rds.json').read_text(encoding='utf-8'))
        request = raw['CPUUtilization']['request']
        self.assertIn('Name=DBInstanceIdentifier,Value=gateflow-s3', request)
        self.assertEqual(request[request.index('--start-time') + 1], '2026-10-19T09:59:00Z')
        region = [c for c in self.aws.commands if 'cloudwatch' in c][0]
        self.assertEqual(region[region.index('--region') + 1], 'ap-southeast-1')

    def test_charts_are_skipped_with_a_note_without_matplotlib(self):
        self.assertEqual(self.build(pyplot=None), 0, self.stderr)
        self.assertIn(f'note: {bundle.CHARTS_SKIPPED}', self.stdout)
        self.assertIn(f'- {bundle.CHARTS_SKIPPED}', self.summary('d9-auto-r1'))
        self.assertFalse((self.out/'d9-auto-r1'/'charts').exists())

    @unittest.skipUnless(HAS_MATPLOTLIB, 'matplotlib is not installed')
    def test_charts(self):
        self.assertEqual(self.build(pyplot=True), 0, self.stderr)
        self.assertTrue((self.out/'d9-auto-r1'/'charts'/'autoscaling-timeline.png').exists())
        self.assertFalse((self.out/'d9-rep1-r1'/'charts'/'autoscaling-timeline.png').exists())
        self.assertTrue((self.out/'d9-rep1-r1'/'charts'/'gateway-cpu.png').exists())
        self.assertIn('- `charts/gateway-cpu.png`', self.summary('d9-rep1-r1'))


class RefusalTests(Day):
    def assertRefused(self, *argv, message='manifest check failed', runner=None):
        code = self.build(*argv, runner=runner)
        self.assertEqual(code, 1, self.stdout)
        self.assertIn(message, self.stderr)

    def change_record(self, run, **changes):
        path = self.runs/run/'run-record.json'
        record = json.loads(path.read_text(encoding='utf-8'))
        record.update(changes)
        path.write_text(json.dumps(record), encoding='utf-8')

    def test_rebuild_guard(self):
        self.assertEqual(self.build(), 0, self.stderr)
        self.assertRefused(message='--rebuild')
        self.assertEqual([c[1:3] for c in self.aws.commands], [['sts', 'get-caller-identity']])  # nothing queried
        self.assertTrue((self.out/'d9-rep1-r1'/'summary.md').exists())
        self.assertEqual(self.build('--rebuild'), 0, self.stderr)
        self.assertTrue((self.out/'day9-config'/'tf-output.json').exists())

    def test_renamed_run_is_refused(self):
        self.change_record('d9-auto-r1', name='d9-auto-r9')
        self.assertRefused()
        self.assertIn('d9-auto-r1: the record names run d9-auto-r9', self.stderr)

    def test_record_on_another_date_is_refused(self):
        for key in ('load_start', 'time'):
            with self.subTest(key=key):
                self.setUp()
                self.change_record('d9-auto-r1', **{key: '2026-10-16T07:02:05.883Z'})
                self.assertRefused()
                self.assertIn(f'{key} 2026-10-16T07:02:05.883Z is not on {DATE}', self.stderr)

    def test_run_of_another_day_is_refused(self):
        self.manifest['runs']['d8-rep1-r1'] = dict(RUNS['d9-rep1-r1'])
        self.write_manifest()
        self.assertRefused()
        self.assertIn('d8-rep1-r1: not a day 9 run id', self.stderr)

    def test_other_commit_phase_outside_the_window_and_missing_record_are_refused(self):
        self.change_record('d9-rep1-r1', commit='f' * 40)
        self.change_record('d9-auto-r1', load_end=iso(400))
        (self.runs/'d9-calib-r1'/'ABORTED.txt').unlink()
        self.assertRefused()
        for text in (f"d9-rep1-r1: commit {'f' * 40}", 'd9-auto-r1: phase hold 20.0 outside the load window',
                     'd9-calib-r1: no run-record.json and no ABORTED.txt'):
            self.assertIn(text, self.stderr)

    def test_derived_run_that_is_not_a_calib_run_is_refused(self):
        self.write(self.evidence/'derived-day9.json', {'runs': [{'name': 'd9-rep1-r1'}]})
        self.assertRefused()
        self.assertIn('derived-day9.json: run d9-rep1-r1 is not a calib run of this manifest', self.stderr)

    def test_aws_not_reachable_is_refused_before_anything_is_deleted(self):
        self.assertEqual(self.build(), 0, self.stderr)
        self.assertRefused('--rebuild', message='AWS not reachable for CloudWatch', runner=FakeAws(fail=['sts']))
        self.assertTrue((self.out/'d9-rep1-r1'/'summary.md').exists())

    def test_failed_cloudwatch_query_is_refused_and_keeps_the_bundle(self):
        self.assertEqual(self.build(), 0, self.stderr)
        self.assertRefused('--rebuild', message='CloudWatch CPUSurplusCreditsCharged query failed (exit 254)',
                           runner=FakeAws(fail=['CPUSurplusCreditsCharged']))
        self.assertTrue((self.out/'d9-auto-r1'/'summary.md').exists())
        self.assertNotIn('not measured', self.summary('d9-auto-r1'))

    def test_live_check_with_problems_is_refused(self):
        state = live('250m', auto=False)
        state['problems'] = ['gateway Pods running 2 != 1']
        self.write(self.evidence/'live-d9-rep1-r1.json', state)
        self.assertRefused(message='the live check recorded problems')

    def test_auto_run_without_t_and_d_is_refused(self):
        (self.evidence/'live-d9-auto-r1.json').unlink()
        self.assertRefused(message='d9-auto-r1: an auto run needs min, max, T and D')

    def test_manifest_without_a_required_key_is_refused(self):
        del self.manifest['commit']
        self.write_manifest()
        self.assertRefused(message='lacks commit')


if __name__ == '__main__':
    unittest.main()
