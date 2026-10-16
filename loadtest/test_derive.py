"""Unit tests of derive.py, on run folders built here."""
import contextlib
from datetime import datetime, timezone
import io
import json
from pathlib import Path
import tempfile
import unittest

import derive

START = datetime(2026, 10, 16, 12, 0, 0, tzinfo=timezone.utc).timestamp()
# LEVELS=5,25,50 with WARMUP_S=60, RAMP_S=60, HOLD_S=180.
PLAN = [('warmup', 5, 0, 60), ('hold', 5, 60, 240), ('ramp', 25, 240, 300), ('hold', 25, 300, 480),
        ('ramp', 50, 480, 540), ('hold', 50, 540, 720)]
CHECKS = {name: 0 for name in derive.CHECKS}


def iso(offset):
    return derive.iso(START + offset)


def matrix(values):
    return {'status': 'success', 'data': {'resultType': 'matrix', 'result': [
        {'metric': {}, 'values': [[START + t, str(v)] for t, v in values]}]}}


def steps(function, until=900, step=15):
    return [(t, function(t)) for t in range(0, until + 1, step)]


class Folders(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.folder = Path(directory.name)

    def run_folder(self, name, completed=None, active=12.6, duration=0.5, rate=25, phases=True, record=None,
                   checks=None, results_name='run.results.json', count=None):
        """A repeat whose holds complete as given; by default 5 and 25 req/s pass and 50 fails."""
        completed = {5: 1000, 25: 1000, 50: 990, **(completed or {})}
        folder = self.folder/name
        (folder/'prom').mkdir(parents=True)
        rows = [{'phase': phase, 'level_rps': float(level), 'start_s': float(start), 'end_s': float(end),
                 'scheduled': 1000, 'completed': completed[level] if phase == 'hold' else 1000,
                 'completion_rate': None, 'duration_ms': {'p50': 480.0}}
                for phase, level, start, end in PLAN]
        results = {'phases': rows, 'checks': {**CHECKS, **(checks or {})}, 'sent_source': 'gateflow_sent'}
        (folder/results_name).write_text(json.dumps(results), encoding='utf-8')
        data = {'name': name, 'error': None, 'k6_exit_code': 0, 'summarize_exit_code': 0,
                'gateway': {'replicas': 1}, 'gateway_after': {'replicas': 1}, 'load_start': iso(0),
                'phases': [{'phase': phase, 'level_rps': float(level), 'start_s': float(start), 'end_s': float(end),
                            'start': iso(start), 'end': iso(end)} for phase, level, start, end in PLAN] if phases else None,
                **(record or {})}
        (folder/'run-record.json').write_text(json.dumps(data), encoding='utf-8')
        count = count or (lambda t: rate * t)
        self.write(folder, 'active', steps(lambda t: active if 300 <= t <= 480 else 0))
        self.write(folder, 'gateway_duration_count', steps(count))
        self.write(folder, 'gateway_duration_sum', steps(lambda t: duration * count(t)))
        return folder

    def write(self, folder, name, values):
        (folder/'prom'/f'{name}.json').write_text(json.dumps(matrix(values)), encoding='utf-8')

    def main(self, *argv):
        out = self.folder/'derived.json'
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = derive.main([*map(str, argv), '--out', str(out)])
        self.stdout, self.stderr = stdout.getvalue(), stderr.getvalue()
        return code, json.loads(out.read_text(encoding='utf-8')) if out.exists() else None


class PassTests(unittest.TestCase):
    def test_level_passes_at_99_5_percent_judged_in_integers(self):
        self.assertTrue(derive.passes({'scheduled': 200, 'completed': 199}))
        self.assertTrue(derive.passes({'scheduled': 1000, 'completed': 995}))
        self.assertFalse(derive.passes({'scheduled': 1000, 'completed': 994}))
        self.assertFalse(derive.passes({'scheduled': 201, 'completed': 199}))
        self.assertFalse(derive.passes({'scheduled': 0, 'completed': 0}))

    def test_increase_counts_a_drop_as_a_reset(self):
        self.assertEqual(derive.increase([(0, 10.0), (15, 20.0), (30, 5.0), (45, 8.0)]), (10.0 + 5.0 + 3.0, 1))
        self.assertEqual(derive.increase([(0, 3.0)]), (0.0, 0))


class DeriveTests(Folders):
    def test_l_c_d_and_t_from_three_repeats(self):
        runs = [self.run_folder('calib-r1', completed={50: 1000}), self.run_folder('calib-r2', active=13.2),
                self.run_folder('calib-r3', completed={50: 1000}, active=14.9)]
        code, out = self.main(*runs)
        self.assertEqual(code, 0)
        # 50 req/s fails in r2, so it fails for the condition.
        self.assertEqual((out['L_rps'], out['first_violated_level_rps'], out['level_rps']), (25.0, 50.0, 25.0))
        self.assertEqual([run['C'] for run in out['runs']], [12, 13, 14])
        self.assertEqual((out['C'], out['T'], out['D_s']), (13, 9, 0.5))
        self.assertEqual(out['k6_duration_p50_s'], 0.48)
        self.assertEqual(out['cross_check'], {'level_times_D': 12.5, 'deviation': round(0.5/13, 4),
                                              'within_10_percent': True})
        first = out['runs'][0]
        self.assertEqual(first['window'], {'start': '2026-10-16T12:05:00.000Z', 'end': '2026-10-16T12:08:00.000Z'})
        self.assertEqual(first['active_samples'], 13)
        self.assertEqual(first['D_inputs']['gateway_duration_count'],
                         {'first': '2026-10-16T12:05:00.000Z', 'last': '2026-10-16T12:08:00.000Z',
                          'increase': 4500.0, 'resets': 0})
        self.assertEqual(out['warnings'], [])
        self.assertIn('- L (highest hold level passing in every valid run): 25 req/s', self.stdout)
        self.assertIn('- C: 13; T = floor(0.7 x C): 9', self.stdout)
        self.assertIn('within 10%', self.stdout)

    def test_even_number_of_repeats_takes_the_floor_of_the_median(self):
        code, out = self.main(self.run_folder('r1', active=12.2), self.run_folder('r2', active=13.7))
        self.assertEqual(code, 0)
        self.assertEqual((out['C'], out['T']), (12, 8))

    def test_invalid_runs_are_listed_and_left_out(self):
        bad = self.run_folder('r2', completed={25: 900}, checks={'dropped_iterations': 3})
        code, out = self.main(self.run_folder('r1'), bad, self.run_folder('r3', record={'summarize_exit_code': 2}))
        self.assertEqual(code, 0)
        self.assertEqual([run['valid'] for run in out['runs']], [True, False, False])
        self.assertEqual(out['runs'][1]['invalid_reasons'], ['dropped_iterations 3'])
        self.assertEqual(out['runs'][2]['invalid_reasons'], ['summarize.py exit code 2'])
        # Counted, r2's failure at 25 req/s would have lowered L to 5.
        self.assertEqual(out['L_rps'], 25.0)
        self.assertNotIn('C', out['runs'][1])

    def test_l_stops_at_the_first_level_that_fails(self):
        code, out = self.main(self.run_folder('r1', completed={25: 994}))
        self.assertEqual((code, out['L_rps'], out['first_violated_level_rps']), (0, 5.0, 25.0))
        code, out = self.main(self.run_folder('r2', completed={5: 0}))
        self.assertEqual((code, out['L_rps'], out['C']), (2, None, None))
        self.assertIn('no hold level passes', self.stderr)

    def test_level_option_measures_another_hold(self):
        code, out = self.main(self.run_folder('r1', completed={50: 0}), '--level', '50')
        self.assertEqual((code, out['L_rps'], out['level_rps']), (0, 25.0, 50.0))
        # active is 0 outside the 25 req/s hold.
        self.assertEqual(out['C'], 0)
        self.assertEqual(out['cross_check']['within_10_percent'], False)
        self.assertEqual(out['warnings'], ['50 req/s does not pass in every valid run'])

    def test_cross_check_off_by_more_than_10_percent_is_flagged_not_failed(self):
        code, out = self.main(self.run_folder('r1', duration=0.8))
        self.assertEqual(code, 0)
        self.assertEqual(out['cross_check'], {'level_times_D': 20.0, 'deviation': round(8/12, 4),
                                              'within_10_percent': False})
        self.assertIn('OFF BY MORE THAN 10%', self.stdout)

    def test_counter_reset_in_the_window_is_counted(self):
        code, out = self.main(self.run_folder('r1', count=lambda t: 25 * (t if t < 390 else t - 390)))
        self.assertEqual(code, 0)
        inputs = out['runs'][0]['D_inputs']
        self.assertEqual(inputs['gateway_duration_count']['resets'], 1)
        self.assertEqual(out['D_s'], 0.5)

    def test_window_falls_back_to_the_results_offsets(self):
        code, out = self.main(self.run_folder('r1', phases=False, results_name='results.json'))
        self.assertEqual(code, 0)
        self.assertEqual(out['runs'][0]['window']['start'], '2026-10-16T12:05:00.000Z')

    def test_not_a_single_pod_run_is_warned(self):
        code, out = self.main(self.run_folder('r1', record={'gateway_after': {'replicas': 2}}))
        self.assertEqual(code, 0)
        self.assertEqual(out['warnings'], ['r1: gateway replicas before/after [1, 2], not a single-Pod run'])

    def test_missing_or_ambiguous_inputs_are_errors(self):
        folder = self.run_folder('r1')
        (folder/'prom'/'active.json').unlink()
        self.assertEqual(self.main(folder)[0], 1)
        self.assertIn('active.json', self.stderr)
        folder = self.run_folder('r2')
        document = matrix(steps(lambda t: 1))
        document['data']['result'] *= 2
        (folder/'prom'/'active.json').write_text(json.dumps(document), encoding='utf-8')
        self.assertEqual(self.main(folder)[0], 1)
        self.assertIn('2 series, expected 1', self.stderr)
        folder = self.run_folder('r3', record={'phases': None, 'load_start': None})
        self.assertEqual(self.main(folder)[0], 1)
        self.assertIn('no hold window', self.stderr)
        self.assertEqual(self.main(self.folder/'missing')[0], 1)

    def test_no_valid_run_exits_2(self):
        code, out = self.main(self.run_folder('r1', record={'k6_exit_code': 1}))
        self.assertEqual((code, out['C']), (2, None))
        self.assertIn('no valid run', self.stderr)

    def test_same_run_twice_is_refused(self):
        folder = self.run_folder('r1')
        self.assertEqual(self.main(folder, folder)[0], 1)


if __name__ == '__main__':
    unittest.main()
