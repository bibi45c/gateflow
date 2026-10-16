"""Derive L, C, D and T from single-Pod calibration runs (report template Part 0.3).

    python loadtest/derive.py <run folder> [<run folder> ...] --out <derived.json> [--level <req/s>]

Each run folder is one repeat, as kube_run.py and prom_export.py leave it:
run-record.json, run.results.json (or results.json, as in the data bundle) and
prom/ with active.json, gateway_duration_sum.json and gateway_duration_count.json.

- A run is valid when kube_run.py recorded no error, k6 and summarize.py exited with
  0, every summarize.py check is 0 and sent_source is gateflow_sent. Invalid runs
  are listed and left out.
- A hold level passes in a run when completed * 1000 >= 995 * scheduled, in integers.
  L is the highest level such that it and every lower hold level pass in every valid
  run; the next one is the first violated level.
- Over the hold window of L (or of --level), taken from the record's phases:
  D = increase(duration sum) / increase(duration count), from the samples of the two
  raw counters at the first and last step inside the window; C = floor(mean of the
  active samples inside the window). Across repeats, C is the floor of the median of
  the per-run C, and D the median of the per-run D. T = floor(0.7 * C).
- Cross-check: |C - level * D| <= 10% of C. It is approximate: the gateway's
  duration includes the work before admission, while active counts only admitted
  requests.

Prints the result as Markdown and writes it as JSON to --out. Exits with 1 when an
input is missing or unreadable, 2 when no run is valid or no level passes (and no
--level is given), and 0 otherwise, also when the cross-check is off by more than 10%.
Only the standard library is used.
"""
import argparse
from datetime import datetime, timezone
import json
import math
from pathlib import Path
import statistics
import sys

PASS_PER_MILLE = 995
CROSS_CHECK = 0.10
T_FACTOR = 0.7
CHECKS = ('wallet_empty_responses', 'invalid_key_responses', 'dropped_iterations', 'dropped_outside_plan',
          'unclassified_requests', 'excess_results')
RESULTS_FILES = ('run.results.json', 'results.json')


class DeriveError(Exception):
    pass


def read_json(path):
    try:
        return json.loads(path.read_text(encoding='utf-8'))
    except OSError as error:
        raise DeriveError(f'{path.parent.name}/{path.name}: {error.strerror or error}') from None
    except ValueError as error:
        raise DeriveError(f'{path.parent.name}/{path.name}: not JSON: {error}') from None


def parse_time(text):
    moment = datetime.fromisoformat(text.replace('Z', '+00:00'))
    if moment.tzinfo is None:
        raise DeriveError(f'time without an offset: {text}')
    return moment.timestamp()


def iso(seconds):
    return datetime.fromtimestamp(seconds, timezone.utc).isoformat(timespec='milliseconds').replace('+00:00', 'Z')


def passes(row):
    """Completion >= 99.50% of the scheduled requests, judged in integers."""
    return row['scheduled'] > 0 and row['completed'] * 1000 >= PASS_PER_MILLE * row['scheduled']


def validity(record, results):
    """The reasons a run is not valid; empty when it is valid."""
    reasons = []
    if record.get('error'):
        reasons.append(f"kube_run.py error: {record['error']}")
    if record.get('k6_exit_code') != 0:
        reasons.append(f"k6 exit code {record.get('k6_exit_code')}")
    if record.get('summarize_exit_code') != 0:
        reasons.append(f"summarize.py exit code {record.get('summarize_exit_code')}")
    if results.get('sent_source') != 'gateflow_sent':
        reasons.append(f"sent_source {results.get('sent_source')}")
    checks = results.get('checks') or {}
    reasons += [f'{name} {checks.get(name)}' for name in CHECKS if checks.get(name) != 0]
    return reasons


class Run:
    """One calibration repeat: its record, its results and its Prometheus exports."""

    def __init__(self, folder):
        self.folder = folder
        self.record = read_json(folder/'run-record.json')
        path = next((folder/name for name in RESULTS_FILES if (folder/name).exists()), None)
        if path is None:
            raise DeriveError(f'{folder.name}: no {" or ".join(RESULTS_FILES)}')
        self.results = read_json(path)
        self.name = self.record.get('name') or folder.name
        self.reasons = validity(self.record, self.results)
        self.holds = {float(row['level_rps']): row for row in self.results.get('phases', []) if row['phase'] == 'hold'}

    def window(self, level):
        """The (start, end) of the hold at level, in Unix seconds."""
        for phase in self.record.get('phases') or []:
            if phase.get('phase') == 'hold' and float(phase.get('level_rps')) == level:
                return parse_time(phase['start']), parse_time(phase['end'])
        row, start = self.holds.get(level), self.record.get('load_start')
        if row is None or not start:
            raise DeriveError(f'{self.name}: no hold window at {level:g} req/s: the record has no phases or load_start')
        return parse_time(start) + row['start_s'], parse_time(start) + row['end_s']

    def series(self, name):
        """The (time, value) samples of the single series of prom/<name>.json."""
        document = read_json(self.folder/'prom'/f'{name}.json')
        try:
            result = document['data']['result']
        except (KeyError, TypeError):
            raise DeriveError(f'{self.name}: prom/{name}.json is not a query_range answer') from None
        if len(result) != 1:
            raise DeriveError(f'{self.name}: prom/{name}.json has {len(result)} series, expected 1')
        samples = [(float(t), float(v)) for t, v in result[0].get('values') or []]
        return sorted((t, v) for t, v in samples if math.isfinite(v))


def inside(samples, start, end):
    return [(t, v) for t, v in samples if start <= t <= end]


def increase(samples):
    """The increase of a counter over its samples, counting a drop as a reset to zero."""
    total, resets = 0.0, 0
    for (_, before), (_, after) in zip(samples, samples[1:]):
        if after >= before:
            total += after - before
        else:
            total, resets = total + after, resets + 1
    return total, resets


def measure(run, level):
    """C and D of one run over its hold window at level."""
    start, end = run.window(level)
    active = inside(run.series('active'), start, end)
    if not active:
        raise DeriveError(f'{run.name}: no active samples in the hold window at {level:g} req/s')
    mean = statistics.fmean(v for _, v in active)
    inputs = {}
    for name in ('gateway_duration_sum', 'gateway_duration_count'):
        samples = inside(run.series(name), start, end)
        if len(samples) < 2:
            raise DeriveError(f'{run.name}: fewer than two {name} samples in the hold window at {level:g} req/s')
        value, resets = increase(samples)
        inputs[name] = {'first': iso(samples[0][0]), 'last': iso(samples[-1][0]), 'increase': value, 'resets': resets}
    count = inputs['gateway_duration_count']['increase']
    if count <= 0:
        raise DeriveError(f'{run.name}: no gateway request ended in the hold window at {level:g} req/s')
    row = run.holds.get(level) or {}
    p50 = (row.get('duration_ms') or {}).get('p50')
    return {'window': {'start': iso(start), 'end': iso(end)}, 'active_samples': len(active),
            'active_mean': round(mean, 3), 'C': math.floor(mean),
            'D_s': inputs['gateway_duration_sum']['increase'] / count, 'D_inputs': inputs,
            'k6_duration_p50_s': p50 / 1000 if p50 is not None else None}


def highest_level(valid):
    """L, the first violated level, and per level whether every valid run passes."""
    levels = sorted({level for run in valid for level in run.holds})
    table, highest, violated = [], None, None
    for level in levels:
        verdicts = {run.name: passes(run.holds[level]) if level in run.holds else None for run in valid}
        ok = all(verdicts.values())
        table.append({'level_rps': level, 'passes_in_every_valid_run': ok, 'runs': verdicts})
        if violated is None:
            if ok:
                highest = level
            else:
                violated = level
    return highest, violated, table


def derive(folders, level=None):
    runs = [Run(folder) for folder in folders]
    names = [run.name for run in runs]
    if len(set(names)) != len(names):
        raise DeriveError(f'the same run is given twice: {", ".join(sorted({n for n in names if names.count(n) > 1}))}')
    valid = [run for run in runs if not run.reasons]
    highest, violated, table = highest_level(valid)
    out = {'runs': [{'name': run.name, 'valid': not run.reasons, 'invalid_reasons': run.reasons,
                     'holds': [{'level_rps': lv, 'scheduled': row['scheduled'], 'completed': row['completed'],
                                'completion_rate': row.get('completion_rate'), 'passes': passes(row)}
                               for lv, row in sorted(run.holds.items())]} for run in runs],
           'levels': table, 'L_rps': highest, 'first_violated_level_rps': violated,
           'level_rps': level if level is not None else highest,
           'C': None, 'T': None, 'D_s': None, 'k6_duration_p50_s': None, 'cross_check': None,
           'aggregation': 'C = floor(median of per-run floor(mean active)); D = median of per-run D', 'warnings': []}
    target = out['level_rps']
    if not valid or target is None:
        return out
    if level is not None and not any(row['level_rps'] == level and row['passes_in_every_valid_run'] for row in table):
        out['warnings'].append(f'{level:g} req/s does not pass in every valid run')
    for run, item in zip(runs, out['runs']):
        if not run.reasons:
            item.update(measure(run, target))
        replicas = [(run.record.get(key) or {}).get('replicas') for key in ('gateway', 'gateway_after')]
        if replicas != [1, 1]:
            out['warnings'].append(f'{run.name}: gateway replicas before/after {replicas}, not a single-Pod run')
    measured = [item for item in out['runs'] if item['valid']]
    out['C'] = math.floor(statistics.median(item['C'] for item in measured))
    out['T'] = math.floor(T_FACTOR * out['C'])
    out['D_s'] = round(statistics.median(item['D_s'] for item in measured), 4)
    p50 = [item['k6_duration_p50_s'] for item in measured if item['k6_duration_p50_s'] is not None]
    out['k6_duration_p50_s'] = round(statistics.median(p50), 4) if p50 else None
    expected = target * out['D_s']
    deviation = abs(out['C'] - expected) / out['C'] if out['C'] else None
    out['cross_check'] = {'level_times_D': round(expected, 2),
                          'deviation': round(deviation, 4) if deviation is not None else None,
                          'within_10_percent': deviation is not None and deviation <= CROSS_CHECK}
    for item in measured:
        item['D_s'] = round(item['D_s'], 4)
    return out


def markdown(out):
    def value(number, unit=''):
        return '-' if number is None else f'{number:g}{unit}'
    lines = ['| Run | Valid | Hold levels: completed / scheduled | Hold window (UTC) | Mean active | C | D (s) '
             '| k6 duration p50 (s) |', '|---|---|---|---|---|---|---|---|']
    for run in out['runs']:
        holds = ', '.join(f"{h['level_rps']:g}: {h['completed']}/{h['scheduled']} {'pass' if h['passes'] else 'FAIL'}"
                          for h in run['holds'])
        window = f"{run['window']['start']} to {run['window']['end']}" if 'window' in run else '-'
        valid = 'yes' if run['valid'] else 'no: ' + '; '.join(run['invalid_reasons'])
        lines.append(f"| {run['name']} | {valid} | {holds} | {window} | {value(run.get('active_mean'))} "
                     f"| {value(run.get('C'))} | {value(run.get('D_s'))} | {value(run.get('k6_duration_p50_s'))} |")
    check = out['cross_check']
    lines += ['', f"- L (highest hold level passing in every valid run): {value(out['L_rps'], ' req/s')}",
              f"- First violated hold level: {value(out['first_violated_level_rps'], ' req/s')}",
              f"- Level used for C and D: {value(out['level_rps'], ' req/s')}",
              f"- D: {value(out['D_s'], ' s')} (k6 duration p50 of the same holds: {value(out['k6_duration_p50_s'], ' s')})",
              f"- C: {value(out['C'])}; T = floor(0.7 x C): {value(out['T'])}",
              f"- Aggregation: {out['aggregation']}"]
    if check:
        verdict = 'within 10%' if check['within_10_percent'] else 'OFF BY MORE THAN 10%: record why'
        deviation = '-' if check['deviation'] is None else f"{check['deviation']:.1%}"
        lines.append(f"- Cross-check C ~ level x D: {check['level_times_D']:g} vs C {out['C']}, "
                     f"deviation {deviation}, {verdict}")
    lines += [f'- Warning: {warning}' for warning in out['warnings']]
    return '\n'.join(lines)


def level_value(text):
    number = float(text)
    if not math.isfinite(number) or number <= 0:
        raise argparse.ArgumentTypeError('a positive request rate')
    return number


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split('\n', 1)[0])
    parser.add_argument('runs', nargs='+', type=Path, help='run folders, one per calibration repeat')
    parser.add_argument('--out', type=Path, required=True, help='JSON file to write')
    parser.add_argument('--level', type=level_value, help='hold level (req/s) for C and D (default: L)')
    args = parser.parse_args(argv)
    try:
        out = derive(args.runs, args.level)
    except (DeriveError, KeyError, TypeError, ValueError) as error:
        print(f'error: {error}', file=sys.stderr)
        return 1
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(out, indent=2, ensure_ascii=False) + '\n', encoding='utf-8', newline='\n')
    print(markdown(out))
    print(f'\nWrote {args.out}')
    if not any(run['valid'] for run in out['runs']):
        print('error: no valid run', file=sys.stderr)
        return 2
    if out['level_rps'] is None:
        print('error: no hold level passes in every valid run; give --level to measure one', file=sys.stderr)
        return 2
    return 0


if __name__ == '__main__':
    sys.exit(main())
