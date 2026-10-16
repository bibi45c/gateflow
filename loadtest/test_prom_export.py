"""Unit tests of prom_export.py. A fake port-forward and a fake Prometheus stand in for kubectl and the cluster."""
import contextlib
from datetime import datetime, timedelta, timezone
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
import urllib.error
import urllib.parse

import prom_export

LOAD_START = datetime(2026, 10, 16, 12, 0, 5, 123000, tzinfo=timezone.utc)
LOAD_END = datetime(2026, 10, 16, 12, 13, 50, tzinfo=timezone.utc)
PORT = 54321
BASE = f'http://127.0.0.1:{PORT}'


class Process:
    """A fake kubectl port-forward."""

    def __init__(self, exit_code=None, stops=True):
        self.exit_code, self.stops = exit_code, stops
        self.terminated = self.killed = False

    def poll(self):
        return self.exit_code

    def terminate(self):
        self.terminated = True
        if self.stops:
            self.exit_code = -15

    def kill(self):
        self.killed, self.exit_code = True, -9

    def wait(self, timeout=None):
        if self.exit_code is None:
            raise subprocess.TimeoutExpired('kubectl', timeout)
        return self.exit_code


class Popen:
    def __init__(self, process=None, stderr=b''):
        self.process, self.stderr = process or Process(), stderr
        self.calls = []

    def __call__(self, command, **kwargs):
        self.calls.append((command, kwargs))
        kwargs['stderr'].write(self.stderr)
        return self.process


def answer(result):
    return json.dumps({'status': 'success', 'data': {'resultType': 'matrix', 'result': result}}).encode('utf-8')


SERIES = [{'metric': {}, 'values': [[1792152005.123, '1'], [1792152020.123, '2']]}]


class Prometheus:
    """A fake Prometheus behind the port-forward: every query returns SERIES unless told otherwise."""

    def __init__(self, empty=(), fail=None, unready=0, interrupt=None):
        self.empty, self.fail, self.unready, self.interrupt = set(empty), fail, unready, interrupt
        self.queries = []

    def name(self, query):
        by_query = {query: name for name, query in prom_export.queries('gateflow', 'gf')}
        return by_query.get(query, query)

    def __call__(self, url, timeout):
        self.test_base(url)
        if url.endswith('/-/ready'):
            if self.unready:
                self.unready -= 1
                raise urllib.error.URLError('connection refused')
            return b'Prometheus Server is Ready.\n'
        params = urllib.parse.parse_qs(urllib.parse.urlsplit(url).query)
        name = self.name(params['query'][0])
        self.queries.append((name, params))
        if name == self.interrupt:
            raise KeyboardInterrupt
        if name == self.fail:
            raise urllib.error.HTTPError(url, 422, 'unprocessable', {}, io.BytesIO(b'{"error":"bad_data"}'))
        return answer([] if name in self.empty else SERIES)

    def test_base(self, url):
        assert url.startswith(BASE + '/'), url


class Clock:
    def __init__(self):
        self.now, self.sleeps = 0.0, []

    def __call__(self):
        return self.now

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.now += seconds


class Base(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.folder = Path(directory.name)
        self.kubeconfig = self.folder/'secret-cluster'/'kubeconfig'
        self.run_dir = self.folder/'calib-1'
        self.run_dir.mkdir()
        self.record = self.run_dir/'run-record.json'
        self.write_record()

    def write_record(self, **fields):
        record = {'name': 'calib-1', 'namespace': 'gateflow', 'release': 'gf',
                  'load_start': '2026-10-16T12:00:05.123Z', 'load_end': '2026-10-16T12:13:50.000Z', **fields}
        self.record.write_text(json.dumps(record), encoding='utf-8')

    def argv(self, *extra):
        return ['--kubeconfig', str(self.kubeconfig), '--kube-context', 'eks', *extra]


class ArgumentTests(Base):
    def refused(self, *argv):
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr), self.assertRaises(SystemExit):
            prom_export.parse_args(list(argv))
        return stderr.getvalue()

    def test_record_gives_the_window_namespace_release_and_folder(self):
        args = prom_export.parse_args(self.argv('--record', str(self.record)))
        self.assertEqual((args.start, args.end), (LOAD_START, LOAD_END))
        self.assertEqual((args.namespace, args.release, args.run), ('gateflow', 'gf', 'calib-1'))
        self.assertEqual(args.out_dir, self.run_dir/'prom')
        self.assertEqual((args.monitoring_namespace, args.service, args.tail_seconds, args.step),
                         ('monitoring', 'prometheus-server', 120, '15s'))

    def test_start_and_end_need_an_offset_and_an_out_dir(self):
        args = prom_export.parse_args(self.argv('--start', '2026-10-16T12:00:00Z', '--end', '2026-10-16T20:10:00+08:00',
                                                '--out-dir', str(self.folder/'p')))
        self.assertEqual(args.end - args.start, timedelta(minutes=10))
        self.assertEqual((args.namespace, args.release), ('gateflow', 'gf'))
        self.assertIn('no offset', self.refused(*self.argv('--start', '2026-10-16T12:00:00', '--end',
                                                            '2026-10-16T12:10:00Z', '--out-dir', 'p')))
        self.assertIn('--out-dir is required', self.refused(*self.argv('--start', '2026-10-16T12:00:00Z',
                                                                        '--end', '2026-10-16T12:10:00Z')))

    def test_window_is_given_exactly_once_and_runs_forward(self):
        self.assertIn('not both', self.refused(*self.argv('--record', str(self.record), '--start', '2026-10-16T12:00:00Z')))
        self.assertIn('give --record', self.refused(*self.argv('--start', '2026-10-16T12:00:00Z')))
        self.assertIn('give --record', self.refused(*self.argv()))
        self.assertIn('ends before', self.refused(*self.argv('--start', '2026-10-16T12:10:00Z', '--end',
                                                             '2026-10-16T12:00:00Z', '--out-dir', 'p')))

    def test_record_without_a_timeline_is_refused(self):
        self.write_record(load_start=None, timeline_problem='run.json has no gateflow_start point with a time')
        self.assertIn('no gateflow_start point', self.refused(*self.argv('--record', str(self.record))))
        self.assertIn('cannot read --record', self.refused(*self.argv('--record', str(self.folder/'missing.json'))))

    def test_bad_values_are_refused(self):
        record = ('--record', str(self.record))
        for extra in (('--step', '0s'), ('--step', '15 s'), ('--step', '-15s'), ('--tail-seconds', '-1'),
                      ('--service', 'Prometheus'), ('--monitoring-namespace', 'a_b'), ('--skip', 'unknown'),
                      ('--namespace', 'gateflow",x="')):
            with self.subTest(extra=extra):
                self.refused(*self.argv(*record, *extra))
        for argv in (['--kube-context', 'eks', *record], ['--kubeconfig', 'config', *record]):
            with self.subTest(argv=argv):
                self.refused(*argv)

    def test_queries_hold_the_template_list_and_the_raw_inputs_of_d(self):
        listed = dict(prom_export.queries('gf-ns', 'lt'))
        self.assertEqual(tuple(listed), prom_export.NAMES)
        self.assertEqual(listed['gateway_duration_sum'],
                         'sum(gateflow_http_request_duration_seconds_sum{service="gateway",route="/v1/messages"})')
        self.assertEqual(listed['gateway_duration_count'],
                         'sum(gateflow_http_request_duration_seconds_count{service="gateway",route="/v1/messages"})')
        self.assertIn('{service="access"}', listed['access_results'])
        self.assertIn('{service="metering"}', listed['metering_results'])
        self.assertEqual(listed['replicas'], 'kube_deployment_status_replicas_ready{namespace="gf-ns",deployment="lt-gateway"}')
        self.assertEqual(listed['active'], 'sum(gateflow_gateway_active_admitted_requests)')


class ExportTests(Base):
    def main(self, prometheus=None, popen=None, now=LOAD_END + timedelta(minutes=5), *extra):
        self.prometheus, self.popen, self.clock = prometheus or Prometheus(), popen or Popen(), Clock()
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = prom_export.main(self.argv('--record', str(self.record), *extra), self.popen, self.prometheus,
                                    self.clock, self.clock.sleep, lambda: now, PORT)
        self.stdout, self.stderr = stdout.getvalue(), stderr.getvalue()
        self.out = self.run_dir/'prom'
        index = self.out/'index.json'
        return code, json.loads(index.read_text(encoding='utf-8')) if index.exists() else None

    def test_writes_every_query_and_the_index(self):
        code, index = self.main()
        self.assertEqual(code, 0)
        self.assertEqual([q['name'] for q in index['queries']], list(prom_export.NAMES))
        for name in prom_export.NAMES:
            self.assertEqual((self.out/f'{name}.json').read_bytes(), answer(SERIES))
        entry = index['queries'][0]
        self.assertEqual(entry, {'name': 'active', 'query': 'sum(gateflow_gateway_active_admitted_requests)',
                                 'file': 'active.json', 'series': 1, 'samples': 2, 'empty': False})
        self.assertEqual(index['window'], {'start': '2026-10-16T12:00:05.123Z', 'end': '2026-10-16T12:13:50.000Z',
                                           'tail_seconds': 120, 'query_end': '2026-10-16T12:15:50.000Z', 'step': '15s'})
        self.assertEqual(index['prometheus'], {'namespace': 'monitoring', 'service': 'prometheus-server', 'port': 80})
        self.assertEqual((index['empty'], index['error'], index['run']), ([], None, 'calib-1'))
        _, params = self.prometheus.queries[0]
        self.assertEqual((params['start'], params['end'], params['step']),
                         (['2026-10-16T12:00:05.123Z'], ['2026-10-16T12:15:50.000Z'], ['15s']))
        self.assertEqual(self.clock.sleeps, [])
        self.assertNotIn(str(self.kubeconfig), (self.out/'index.json').read_text(encoding='utf-8'))

    def test_port_forward_uses_the_given_kubeconfig_and_service_port_80_and_is_stopped(self):
        self.main()
        (command, kwargs), = self.popen.calls
        self.assertEqual(command, ['kubectl', '--kubeconfig', str(self.kubeconfig.resolve()), '--context', 'eks',
                                   '-n', 'monitoring', 'port-forward', 'svc/prometheus-server', f'{PORT}:80',
                                   '--address', '127.0.0.1'])
        self.assertEqual(kwargs['stdout'], subprocess.DEVNULL)
        self.assertTrue(self.popen.process.terminated)

    def test_waits_until_the_tail_has_been_scraped(self):
        # 30 s after the load ended, the 120 s tail is 90 s away.
        code, _ = self.main(None, None, LOAD_END + timedelta(seconds=30))
        self.assertEqual(code, 0)
        self.assertEqual(self.clock.sleeps, [90])
        self.assertIn('Waiting 90 s until 2026-10-16T12:15:50.000Z', self.stdout)
        code, _ = self.main(None, None, LOAD_END + timedelta(seconds=119.2), '--overwrite')
        self.assertEqual(self.clock.sleeps, [1])
        code, _ = self.main(None, None, LOAD_END + timedelta(seconds=120), '--overwrite')
        self.assertEqual(self.clock.sleeps, [])

    def test_empty_result_exits_2_and_writes_the_rest(self):
        code, index = self.main(Prometheus(empty={'hpa_desired', 'hpa_conditions'}))
        self.assertEqual(code, 2)
        self.assertEqual(index['empty'], ['hpa_desired', 'hpa_conditions'])
        flags = {q['name']: q['empty'] for q in index['queries']}
        self.assertTrue(flags['hpa_desired'])
        self.assertFalse(flags['active'])
        self.assertEqual(len(index['queries']), len(prom_export.NAMES))
        self.assertTrue((self.out/'hpa_desired.json').exists())
        self.assertIn('no series for hpa_desired, hpa_conditions', self.stderr)
        self.assertTrue(self.popen.process.terminated)

    def test_skipped_queries_are_not_run_and_are_listed(self):
        code, index = self.main(Prometheus(empty={'hpa_desired', 'hpa_conditions'}), None,
                                LOAD_END + timedelta(minutes=5), '--skip', 'hpa_desired', '--skip', 'hpa_conditions')
        self.assertEqual(code, 0)
        self.assertEqual(index['skipped'], ['hpa_desired', 'hpa_conditions'])
        self.assertNotIn('hpa_desired', [name for name, _ in self.prometheus.queries])
        self.assertFalse((self.out/'hpa_desired.json').exists())

    def test_http_error_exits_1_and_stops_the_port_forward(self):
        code, index = self.main(Prometheus(fail='arrivals'))
        self.assertEqual(code, 1)
        self.assertIn('arrivals: HTTP 422', index['error'])
        self.assertEqual([q['name'] for q in index['queries']], ['active', 'active_by_pod'])
        self.assertTrue(self.popen.process.terminated)

    def test_port_forward_that_exits_is_an_error_without_the_kubeconfig_path(self):
        process = Process(exit_code=1)
        stderr = f'error: open {self.kubeconfig}: no such file\n'.encode('utf-8')
        code, index = self.main(None, Popen(process, stderr))
        self.assertEqual(code, 1)
        self.assertIn('port-forward exited with 1', index['error'])
        self.assertIn('<kubeconfig>', index['error'])
        self.assertNotIn(str(self.kubeconfig), index['error'] + self.stderr)
        self.assertEqual(self.prometheus.queries, [])

    def test_port_forward_is_retried_until_ready_and_then_given_up(self):
        code, _ = self.main(Prometheus(unready=2))
        self.assertEqual((code, self.clock.sleeps), (0, [0.5, 0.5]))
        code, index = self.main(Prometheus(unready=10_000), None, LOAD_END + timedelta(minutes=5), '--overwrite')
        self.assertEqual(code, 1)
        self.assertIn('did not answer', index['error'])
        self.assertTrue(self.popen.process.terminated)

    def test_port_forward_is_stopped_on_interrupt(self):
        code, index = self.main(Prometheus(interrupt='results'))
        self.assertEqual((code, index['error']), (130, 'interrupted'))
        self.assertTrue(self.popen.process.terminated)

    def test_port_forward_that_ignores_terminate_is_killed(self):
        process = Process(stops=False)
        code, _ = self.main(None, Popen(process))
        self.assertEqual(code, 0)
        self.assertTrue(process.terminated and process.killed)

    def test_earlier_export_is_kept_unless_overwrite(self):
        (self.run_dir/'prom').mkdir()
        (self.run_dir/'prom'/'active.json').write_text('earlier\n', encoding='utf-8')
        code, index = self.main()
        self.assertEqual((code, index), (1, None))
        self.assertIn('already has active.json', self.stderr)
        self.assertEqual(self.popen.calls, [])
        self.assertEqual((self.run_dir/'prom'/'active.json').read_text(encoding='utf-8'), 'earlier\n')
        code, _ = self.main(None, None, LOAD_END + timedelta(minutes=5), '--overwrite')
        self.assertEqual(code, 0)

    def test_answer_that_is_not_success_is_an_error(self):
        prometheus = Prometheus()
        original = prometheus.__call__

        def broken(url, timeout):
            body = original(url, timeout)
            return b'{"status":"error","error":"query timed out"}' if 'query_range' in url else body
        code, index = self.main(broken)
        self.assertEqual(code, 1)
        self.assertIn('query timed out', index['error'])


if __name__ == '__main__':
    unittest.main()
