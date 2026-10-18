"""Build the Part C data bundle of one cloud day from its run folders and evidence.

    python loadtest/bundle.py --manifest .local/cloud-day/day2-manifest.json [--rebuild]

Inputs:
- the manifest (see README.md, Build the data bundle): day, date, commit, the derived
  file, configurations, runs, and optional settings;
- each run's folder in --runs (default .local/loadtest/<run>/), as kube_run.py and
  prom_export.py leave it, or holding only ABORTED.txt for a run the operator stopped;
- in --evidence (default: the manifest's folder): each run's check_live.py output
  (live_pattern, default live-<run>.json), the derived file of derive.py, and the
  manifest's extra_files;
- <out>/day<N>-config/ from deploy/aws/collect_config.py: images.json (the gateway
  digest), nodes.json (the load generator's allocatable CPU) and tf-output.json (the
  RDS instance).

Writes to --out (default <evidence>/day<N>/), per run: results.json, run-record.json,
k6.json.gz, prom/, cloudwatch-rds.json, summary.md (Part B) and charts/; then
derived.json, day<N>-runs.csv and SHA256SUMS, and zips the folder to --zip (default:
next to --out). RDS values come from CloudWatch through the aws CLI (read-only).

Refuses, before anything is deleted:
- a run ID without the d<N>- prefix, a record that names another run, a record time
  not on the manifest's date, a phase outside the load window, another commit, a run
  with neither run-record.json nor ABORTED.txt, or a derived run that is not a calib
  run of the manifest;
- AWS not reachable, a live check that recorded problems, an auto run without min,
  max, T and D, or a CloudWatch query that failed: a value is never reported as "not
  measured" because its query failed;
- an existing bundle without --rebuild (day<N>-config is always kept).

Charts need matplotlib. Without it the bundle is still built, and the output and
every summary say that the charts were skipped. Exit codes: 0 built, 1 refused or
failed.
"""
import argparse
import csv
from datetime import datetime, timezone
import gzip
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess
import sys
import zipfile

ROOT = Path(__file__).resolve().parents[1]
REGION = 'ap-southeast-1'
# messages.js defaults (README.md, Settings), for the settings a run record leaves out.
DEFAULTS = {'LEVELS': '5,25,50', 'WARMUP_S': '60', 'RAMP_S': '60', 'HOLD_S': '180', 'STREAM_S': '25',
            'MAX_VUS': '4000', 'TENANTS': '10', 'PAIRED': '0', 'MODEL': 'mock-load', 'MAX_TOKENS': '256',
            'INPUT_BYTES': '1024', 'TTFB_DEADLINE_MS': '10000', 'TOTAL_DEADLINE_S': '60'}
SEED_KEY = 'ACCESS_SEED_SECRET'
GENERATOR = ('gateflow.io/workload', 'load-generator')
GENERATOR_SHARE = 0.8  # generator criterion: k6 plus mock CPU below 80% of the node's allocatable CPU
PASS_PER_MILLE = 995
AUTO_CONDITIONS = ('auto', 'burst')
RECORD_TIMES = ('time', 'op_start', 'load_start', 'load_end', 'op_end')
RDS_METRICS = (('CPUUtilization', 'Maximum', 60), ('DatabaseConnections', 'Maximum', 60),
               ('CPUCreditBalance', 'Minimum', 300), ('CPUSurplusCreditBalance', 'Maximum', 300),
               ('CPUSurplusCreditsCharged', 'Sum', 300))
FIELDS = ['run_id', 'day', 'condition', 'repeat', 'config_id', 'commit', 'gateway_digest', 'replicas_fixed',
          'autoscale_min', 'autoscale_max', 'T', 'D_s', 'levels', 'warmup_s', 'ramp_s', 'hold_s', 'tenants', 'paired',
          'model', 'max_tokens', 'input_bytes', 'ttfb_deadline_ms', 'total_deadline_s', 'start_utc', 'end_utc',
          'summarize_exit', 'valid', 'highest_passing_level_rps', 'first_violated_level_rps', 'boundary_type',
          'generator_ok', 'notes']
CHARTS_SKIPPED = 'charts not built: matplotlib is not installed (install it, then build again with --rebuild)'


class BundleError(Exception):
    pass


def read_json(path):
    try:
        return json.loads(Path(path).read_text(encoding='utf-8-sig'))
    except OSError as error:
        raise BundleError(f'cannot read {path}: {error.strerror or error}') from None
    except ValueError as error:
        raise BundleError(f'{path}: not JSON: {error}') from None


def ts(text):
    return datetime.fromisoformat(str(text).replace('Z', '+00:00')).timestamp()


def iso(seconds):
    return datetime.fromtimestamp(seconds, timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')


def cores(text):
    text = str(text)
    return float(text[:-1]) / 1000 if text.endswith('m') else float(text)


def passes(row):
    return row['completed'] * 1000 >= PASS_PER_MILLE * row['scheduled']


def fmt(value):
    return 'not measured' if value is None else f'{value:.1f}'


def load_pyplot():
    """matplotlib's pyplot with the file-only backend, or None when matplotlib is not installed."""
    try:
        import matplotlib
    except ImportError:
        return None
    matplotlib.use('Agg')
    import matplotlib.pyplot as pyplot
    return pyplot


# --- Prometheus exports --------------------------------------------------------------

def prom(folder, name):
    path = folder/'prom'/f'{name}.json'
    if not path.exists():
        return []
    document = read_json(path)
    return (document.get('data') or document).get('result', [])


def summed(series, prefix=None, key='pod'):
    """time -> sum of the matching series."""
    out = {}
    for item in series:
        if prefix and not item['metric'].get(key, '').startswith(prefix):
            continue
        for t, v in item['values']:
            if v != 'NaN':
                out[float(t)] = out.get(float(t), 0.0) + float(v)
    return dict(sorted(out.items()))


def peak_per_pod(series, prefix):
    best = None
    for item in series:
        if item['metric'].get('pod', '').startswith(prefix):
            for t, v in item['values']:
                if v != 'NaN' and (best is None or float(v) > best[0]):
                    best = (float(v), float(t), item['metric']['pod'])
    return best


def phase_at(phases, t):
    for phase in phases:
        if ts(phase['start']) <= t <= ts(phase['end']):
            return f"{phase['phase']} {phase['level_rps']:g}"
    return 'outside phases'


# --- the day ---------------------------------------------------------------------------

class Day:
    """The manifest of one day and the paths of its bundle."""

    def __init__(self, args):
        self.manifest_path = args.manifest
        self.manifest = read_json(args.manifest)
        missing = [key for key in ('day', 'date', 'commit', 'derived', 'configs', 'runs') if key not in self.manifest]
        if missing:
            raise BundleError(f'{args.manifest} lacks {", ".join(missing)}')
        self.day = str(self.manifest['day'])
        self.date = str(self.manifest['date'])
        self.commit = self.manifest['commit']
        self.configs = self.manifest['configs']
        self.runs = self.manifest['runs']
        self.evidence = args.evidence or args.manifest.parent
        self.run_root = args.runs
        self.out = args.out or self.evidence/f'day{self.day}'
        self.zip = args.zip or self.out.parent/f'{self.out.name}.zip'
        self.config = self.out/f'day{self.day}-config'
        self.region = args.region
        self.live_pattern = self.manifest.get('live_pattern', 'live-{run}.json')
        for run, entry in self.runs.items():
            lacking = [key for key in ('condition', 'repeat', 'config') if key not in entry]
            if lacking:
                raise BundleError(f'manifest run {run} lacks {", ".join(lacking)}')
            if entry['config'] not in self.configs:
                raise BundleError(f"manifest run {run}: config {entry['config']} is not in configs")

    def folder(self, run):
        return self.run_root/run

    def live(self, run):
        return self.evidence/self.live_pattern.format(run=run)


def check_manifest(day):
    """Refuses mixed-day input: every run belongs to this day, its date and commit match."""
    bad = []
    for run in day.runs:
        if not run.startswith(f'd{day.day}-'):
            bad.append(f'{run}: not a day {day.day} run id')
            continue
        path = day.folder(run)/'run-record.json'
        if path.exists():
            record = read_json(path)
            if record.get('name') != run:
                bad.append(f"{run}: the record names run {record.get('name')}")
            for key in RECORD_TIMES:
                if not str(record.get(key, '')).startswith(day.date):
                    bad.append(f'{run}: {key} {record.get(key)} is not on {day.date}')
            if record.get('load_start') and record.get('load_end'):
                low, high = ts(record['load_start']) - 1, ts(record['load_end']) + 1
                for phase in record.get('phases') or []:
                    if not low <= ts(phase['start']) <= ts(phase['end']) <= high:
                        bad.append(f"{run}: phase {phase['phase']} {phase['level_rps']} outside the load window")
                        break
            if record.get('commit') != day.commit:
                bad.append(f"{run}: commit {record.get('commit')}")
        elif not (day.folder(run)/'ABORTED.txt').exists():
            bad.append(f'{run}: no run-record.json and no ABORTED.txt')
    derived = read_json(day.evidence/day.manifest['derived'])
    for item in derived.get('runs', []):
        if item.get('name') not in day.runs or day.runs[item['name']]['condition'] != 'calib':
            bad.append(f"{day.manifest['derived']}: run {item.get('name')} is not a calib run of this manifest")
    if bad:
        raise BundleError('manifest check failed:\n' + '\n'.join(bad))
    return derived


def generator_cpu(day):
    """Allocatable cores of the load-generator node, from nodes.json; the manifest's value otherwise."""
    nodes = day.config/'nodes.json'
    if nodes.exists():
        for node in read_json(nodes):
            if node.get('labels', {}).get(GENERATOR[0]) == GENERATOR[1]:
                return cores(node['allocatable']['cpu'])
    if 'generator_allocatable_cpu' not in day.manifest:
        raise BundleError(f'no load-generator node in {nodes} and no generator_allocatable_cpu in the manifest')
    return float(day.manifest['generator_allocatable_cpu'])


def run_config(day, run):
    """Gateway mode and resources of one run: the saved check_live.py output first, the manifest otherwise."""
    entry = day.runs[run]
    resources = entry.get('gateway_resources') or day.manifest.get('gateway_resources')
    cfg = {'mode': 'auto' if entry['condition'] in AUTO_CONDITIONS else 'fixed', 'source': 'manifest',
           'gateway': dict(resources or {})}
    cfg.update(entry.get('autoscaling') or {})
    live = day.live(run)
    if live.exists():
        state = read_json(live)
        if state.get('errors') or state.get('problems'):
            raise BundleError(f'{live}: the live check recorded problems; the run is not a clean {run}')
        gateway = f"{state.get('release', 'gf')}-gateway"
        try:
            limits = state['deployments'][gateway]['resources'].get('limits', {})
        except (KeyError, TypeError, AttributeError):
            raise BundleError(f'{live}: no {gateway} Deployment') from None
        cfg['gateway'] = {'cpu': limits.get('cpu'), 'memory': limits.get('memory')}
        cfg['source'] = live.name
        if state.get('scaledobjects'):
            item = state['scaledobjects'][0]
            triggers = {t.get('name'): t for t in item.get('triggers', [])}
            demand = re.search(r'\) \* ([0-9.]+) ', triggers.get('eligible-demand', {}).get('metadata', {}).get('query', ''))
            if not demand:
                raise BundleError(f'{live}: no eligible-demand query with a factor D')
            cfg.update({'min': item['minReplicaCount'], 'max': item['maxReplicaCount'],
                        'T': item['triggers'][0]['metadata']['threshold'], 'D': demand.group(1)})
    if cfg['mode'] == 'auto' and not all(key in cfg for key in ('min', 'max', 'T', 'D')):
        raise BundleError(f'{run}: an auto run needs min, max, T and D (live check or manifest)')
    if not cfg['gateway'].get('cpu') or not cfg['gateway'].get('memory'):
        raise BundleError(f'{run}: no gateway CPU and memory (live check or manifest gateway_resources)')
    return cfg


# --- AWS -------------------------------------------------------------------------------

def aws(runner, *args):
    try:
        return runner(['aws', *args, '--output', 'json'], capture_output=True, text=True, encoding='utf-8',
                      errors='replace')
    except OSError as error:
        raise BundleError(f'aws could not start: {error.strerror}') from None


def check_aws(runner):
    result = aws(runner, 'sts', 'get-caller-identity')
    if result.returncode:
        raise BundleError(f'AWS not reachable for CloudWatch (exit {result.returncode}): '
                          f'{(result.stderr or "").strip()[:200]}; sign in and rerun')


def database(day):
    outputs = read_json(day.config/'tf-output.json')
    try:
        return outputs['database_address']['value'].split('.')[0]
    except (KeyError, TypeError, AttributeError):
        raise BundleError(f"{day.config/'tf-output.json'} has no database_address") from None


def rds(runner, day, ident, start, end):
    """RDS CloudWatch values over the run, and the raw requests and datapoints."""
    values, raw = {}, {}
    for metric, stat, period in RDS_METRICS:
        args = ['--namespace', 'AWS/RDS', '--metric-name', metric, '--dimensions', f'Name=DBInstanceIdentifier,Value={ident}',
                '--start-time', iso(start - period), '--end-time', iso(end + period), '--period', str(period),
                '--statistics', stat]
        result = aws(runner, 'cloudwatch', 'get-metric-statistics', '--region', day.region, *args)
        if result.returncode:  # never "not measured" because a query failed: sign in again and rebuild
            raise BundleError(f'CloudWatch {metric} query failed (exit {result.returncode}): '
                              f'{(result.stderr or "").strip()[:200]}')
        try:
            points = sorted(json.loads(result.stdout).get('Datapoints', []), key=lambda p: p['Timestamp'])
            numbers = [float(p[stat]) for p in points]
        except (ValueError, AttributeError, KeyError, TypeError):
            raise BundleError(f'CloudWatch {metric}: unreadable answer') from None
        raw[metric] = {'request': args, 'exit_code': result.returncode, 'datapoints': points}
        values[metric] = ({'Maximum': max, 'Minimum': min, 'Sum': sum}[stat](numbers)) if numbers else None
    return values, raw


# --- charts ----------------------------------------------------------------------------

def auto_chart(plt, folder, run, rec, cfg, out):
    """Time-aligned autoscaling view (template B5): offered level, arrivals, non-completed results,
    active, desired and ready replicas, gateway first-output p95."""
    t0 = ts(rec['load_start'])

    def m(t):
        return (t - t0) / 60
    fig, (ax1, ax2, ax3) = plt.subplots(3, 1, figsize=(10, 9), sharex=True)
    xs, ys, prev = [], [], 0
    for p in rec['phases']:
        xs += [m(ts(p['start'])), m(ts(p['end']))]
        ys += [prev if p['phase'] == 'ramp' else p['level_rps'], p['level_rps']]
        prev = p['level_rps']
    ax1.plot(xs, ys, color='black', lw=1, label='offered level (req/s)')
    arrivals = summed(prom(folder, 'arrivals'))
    ax1.plot([m(t) for t in arrivals], list(arrivals.values()), label='eligible arrivals (req/s)')
    bad = {}
    for item in prom(folder, 'results'):
        if item['metric'].get('result') != 'completed':
            for x, v in item['values']:
                if v != 'NaN':
                    bad[float(x)] = bad.get(float(x), 0) + float(v)
    ax1.plot([m(t) for t in sorted(bad)], [bad[t] for t in sorted(bad)], color='red', label='non-completed results (req/s)')
    ax1.set_ylabel('req/s')
    ax1.legend(fontsize=7)
    active = summed(prom(folder, 'active'))
    ax2.plot([m(t) for t in active], list(active.values()), label='active admitted requests')
    ax2.set_ylabel('active')
    ax2b = ax2.twinx()
    for name, style in (('hpa_desired', '--'), ('replicas', '-')):
        series = summed(prom(folder, name))
        ax2b.step([m(t) for t in series], list(series.values()), style, where='post', label=f'{name} (replicas)')
    ax2b.set_ylabel('replicas')
    ax2b.set_ylim(0, int(cfg['max']) + 1)
    ax2.legend(fontsize=7, loc='upper left')
    ax2b.legend(fontsize=7, loc='upper right')
    first = summed(prom(folder, 'first_output_p95'))
    ax3.plot([m(t) for t in first], list(first.values()), label='gateway first output p95 (s; buckets end at 10 s)')
    ax3.set_ylabel('s')
    ax3.set_xlabel('minutes from load start')
    ax3.legend(fontsize=7)
    for ax in (ax1, ax2, ax3):
        for p in rec['phases']:
            ax.axvline(m(ts(p['start'])), color='grey', lw=0.4)
    fig.suptitle(f'{run}: autoscaling, time-aligned')
    fig.tight_layout()
    fig.savefig(out/'autoscaling-timeline.png', dpi=110)
    plt.close(fig)
    return 'autoscaling-timeline.png'


def charts(plt, folder, run, res, rec, cfg, out):
    holds = [p for p in res['phases'] if p['phase'] == 'hold']
    if not holds:
        return []
    out.mkdir(exist_ok=True)
    levels = [p['level_rps'] for p in holds]
    fig, ax = plt.subplots(figsize=(7, 4))
    ax.plot(levels, [100 * p['completed'] / p['scheduled'] for p in holds], marker='o', label='completion %')
    ax.axhline(99.5, color='grey', ls='--', lw=0.8, label='99.5% pass line')
    ax.set_xlabel('hold level (req/s)')
    ax.set_ylabel('completion (%)')
    ax2 = ax.twinx()
    ax2.plot(levels, [p['throughput_rps'] for p in holds], marker='s', color='tab:orange', label='throughput')
    ax2.set_ylabel('throughput (req/s)')
    fig.legend(loc='lower left')
    ax.set_title(f'{run}: completion and throughput per hold level')
    fig.tight_layout()
    fig.savefig(out/'completion-throughput.png', dpi=110)
    plt.close(fig)
    fig, ax = plt.subplots(figsize=(7, 4))
    for q in ('p50', 'p95'):
        ax.plot(levels, [p['ttfb_ms'].get(q) for p in holds], marker='o', label=f'TTFB {q}')
    ax.set_xlabel('hold level (req/s)')
    ax.set_ylabel('ms')
    ax.set_title(f'{run}: TTFB per hold level')
    ax.legend()
    fig.tight_layout()
    fig.savefig(out/'ttfb.png', dpi=110)
    plt.close(fig)
    fig, ax = plt.subplots(figsize=(9, 4))
    t0 = ts(rec['load_start'])
    limit = cores(cfg['gateway']['cpu'])
    for item in prom(folder, 'cpu_by_pod'):
        if item['metric'].get('pod', '').startswith(f'{rec.get("release", "gf")}-gateway'):
            ax.plot([(float(t) - t0) / 60 for t, _ in item['values']], [float(v) for _, v in item['values']],
                    label=item['metric']['pod'])
    for p in rec['phases']:
        ax.axvline((ts(p['start']) - t0) / 60, color='grey', lw=0.5)
        ax.text((ts(p['start']) - t0) / 60, limit * 1.02, f"{p['phase'][0]}{p['level_rps']:g}", fontsize=7,
                rotation=90, va='bottom')
    ax.axhline(limit, color='red', ls='--', lw=0.8, label=f"{cfg['gateway']['cpu']} CPU request = limit")
    ax.set_xlabel('minutes from load start')
    ax.set_ylabel('cores')
    ax.set_ylim(0, limit * 1.25)
    ax.set_title(f'{run}: gateway CPU per Pod', pad=28)
    ax.legend(fontsize=7, loc='lower right')
    fig.tight_layout()
    fig.savefig(out/'gateway-cpu.png', dpi=110)
    plt.close(fig)
    names = ['completion-throughput.png', 'ttfb.png', 'gateway-cpu.png']
    return names + [auto_chart(plt, folder, run, rec, cfg, out)] if cfg['mode'] == 'auto' else names


# --- one run ---------------------------------------------------------------------------

def summary(day, run, prepared, out, gen_cpu, plt):
    """Writes summary.md (Part B) and the charts of one run; returns its outcome."""
    entry, rec, res, cfg, rd = (prepared[key] for key in ('entry', 'record', 'results', 'cfg', 'rds'))
    folder = day.folder(run)
    env = {**DEFAULTS, **{k: v for k, v in rec['env'].items() if k != SEED_KEY}}
    checks = res['checks']
    release = rec.get('release', 'gf')
    gateway = f'{release}-gateway'
    valid = (rec.get('summarize_exit_code') == 0 and rec.get('k6_exit_code') == 0 and not rec.get('error')
             and all(v == 0 for v in checks.values()) and res.get('sent_source') == 'gateflow_sent')
    holds = [p for p in res['phases'] if p['phase'] == 'hold']
    highest, violated, btype = None, None, ''
    for p in holds:
        if passes(p):
            if violated is None:
                highest = p['level_rps']
        elif violated is None:
            violated = p['level_rps']
            btype = 'completion' + (' + generator' if p.get('dropped') else '')
    k6_max_vus = max((p.get('max_active_vus') or 0) for p in res['phases'])
    cpu = prom(folder, 'cpu_by_pod')
    generator, mock = summed(cpu, 'gateflow-k6'), summed(cpu, f'{release}-mock')
    gen_peak = max((generator.get(t, 0) + mock.get(t, 0) for t in generator), default=0)
    limit = GENERATOR_SHARE * gen_cpu
    generator_ok = checks['dropped_iterations'] == 0 and k6_max_vus < int(env['MAX_VUS']) and gen_peak < limit
    phases = rec['phases']

    def peak(label, series, prefix, scale=1.0, unit='', note=''):
        best = peak_per_pod(prom(folder, series), prefix)
        if not best:
            return f'| {label} | not exported | `{series}` | {note} |'
        return f'| {label} | {best[0] / scale:.3g}{unit} at {phase_at(phases, best[1])} ({best[2]}) | `{series}` | {note} |'
    active, arrivals = summed(prom(folder, 'active')), summed(prom(folder, 'arrivals'))
    results = {}
    for item in prom(folder, 'results'):
        value = max((float(y) for _, y in item['values'] if y != 'NaN'), default=0)
        if value > 0:
            results[item['metric'].get('result')] = value
    meter = {item['metric'].get('route', '?'): max((float(y) for _, y in item['values'] if y != 'NaN'), default=0)
             for item in prom(folder, 'metering_p95')}
    first = summed(prom(folder, 'first_output_p95'))
    if cfg['mode'] == 'fixed':
        replicas = (f"| Gateway replicas | fixed {rec['gateway']['replicas']} (after: {rec['gateway_after']['replicas']}); "
                    f"resources {cfg['gateway']} ({cfg['source']}) |")
    else:
        replicas = (f"| Gateway autoscaling | KEDA min {cfg['min']}, max {cfg['max']}, T {cfg['T']}, D {cfg['D']} s; "
                    f"resources {cfg['gateway']} ({cfg['source']}); ready at start {rec['gateway']['ready_replicas']}, "
                    f"at end {rec['gateway_after']['ready_replicas']}; ready over the run in B3 and the timeline chart |")
    lines = [
        f"### Run {run} (day {day.day}, condition {entry['condition']}, repeat {entry['repeat']})", '',
        '#### B1. Run record', '', '| Field | Value |', '|---|---|',
        f"| Purpose and expected outcome | {entry.get('purpose', '')} |",
        f"| Times (UTC) | op_start {rec['op_start']}, load_start {rec['load_start']}, plan_end {rec['plan_end']}, "
        f"load_end {rec['load_end']}, op_end {rec['op_end']}; phases: "
        + '; '.join(f"{p['phase']} {p['level_rps']:g} {p['start'][11:19]}-{p['end'][11:19]}" for p in phases) + ' |',
        f"| Config ID; differences from the daily configuration | {entry['config']}: {day.configs[entry['config']]} |",
        replicas,
        '| k6 settings | ' + ', '.join(f'{k}={env[k]}' for k in DEFAULTS) + ' |',
        f'| Where k6 ran | in-cluster Pod on the load-generator node, target `http://{gateway}:8080` |',
        f"| `kube_run.py` record | Pod {rec['pod']}, image {rec['k6_image']}, k6 exit {rec['k6_exit_code']}, "
        f"resources {rec['resources']}, raw output k6.json.gz (SHA-256 in SHA256SUMS) |",
        f"| Database reset before this run | {day.manifest.get('database_reset', 'not recorded in the manifest')} |",
        f"| `summarize.py` exit code; `sent_source` | {rec['summarize_exit_code']}; {res.get('sent_source')} |",
        '| Checks | ' + ', '.join(f'{k} {v}' for k, v in checks.items()) + ' |',
        f"| Valid (Part 0.2) | {'yes' if valid else 'no'} |", '', '#### B2. Results', '',
        (folder/'summary.md').read_text(encoding='utf-8').strip(), '',
        '#### B3. Resources and dependencies (peaks over the run window)', '',
        '| Signal | Peak (and level where reached) | Source | Notes |', '|---|---|---|---|',
        peak('Gateway CPU per Pod (cores)', 'cpu_by_pod', gateway, note=f"{cfg['gateway']['cpu']} CPU request = limit"),
        peak('Gateway memory per Pod', 'mem_by_pod', gateway, 2**20, ' MiB', note=f"{cfg['gateway']['memory']} limit"),
        peak('Gateway network per Pod (bytes/s)', 'net_by_pod', gateway),
        peak('Access CPU (cores)', 'cpu_by_pod', f'{release}-access'),
        peak('Metering CPU (cores)', 'cpu_by_pod', f'{release}-metering'),
        peak('k6 CPU (cores)', 'cpu_by_pod', 'gateflow-k6'),
        peak('k6 memory', 'mem_by_pod', 'gateflow-k6', 2**20, ' MiB'),
        peak('Mock CPU (cores)', 'cpu_by_pod', f'{release}-mock'),
        peak('Mock memory', 'mem_by_pod', f'{release}-mock', 2**20, ' MiB'),
        f"| Active admitted requests; eligible arrival rate | {max(active.values(), default=0):.0f}; "
        f"{max(arrivals.values(), default=0):.1f} req/s | `active`, `arrivals` | |",
        '| Gateway results by class (peak rate) | ' + ', '.join(f'{k} {v:.3g}/s' for k, v in sorted(results.items()))
        + ' | `results` | |',
        '| Metering p95 per route (s) | ' + ', '.join(f'{k} {v:.3g}' for k, v in sorted(meter.items()))
        + ' | `metering_p95` | |',
        f"| RDS CPU max; connections max; credit balance min; surplus balance max; surplus charged | "
        f"{fmt(rd['CPUUtilization'])} %; {fmt(rd['DatabaseConnections'])}; {fmt(rd['CPUCreditBalance'])}; "
        f"{fmt(rd['CPUSurplusCreditBalance'])}; {fmt(rd['CPUSurplusCreditsCharged'])} | CloudWatch, `cloudwatch-rds.json` "
        '| CPU and connections 1-min; credit metrics 5-min (db.t4g runs in Unlimited mode) |',
        f"| Gateway-side first output p95 (s) | {max(first.values(), default=0):.3g} | `first_output_p95` | buckets end at 10 s |",
        f"| Ready replicas | {sorted({int(float(v)) for s in prom(folder, 'replicas') for _, v in s['values']})} "
        '| `replicas` | |', '',
        f"Generator criterion met: {'yes' if generator_ok else 'no'} (dropped_iterations {checks['dropped_iterations']}, "
        f"max VUs {k6_max_vus} of {env['MAX_VUS']}, k6 + mock CPU peak {gen_peak:.2f} of {limit:.2f} cores).", '',
        '#### B4. Outcome', '', f"- Valid run: {'yes' if valid else 'no'}.",
        '- Completion at each hold level: ' + '; '.join(
            f"{p['level_rps']:g}: {p['completed']}/{p['scheduled']} {'pass' if passes(p) else 'fail'}" for p in holds),
        f"- Highest passing hold level: {highest if highest is not None else '-'}; first violated: "
        f"{violated if violated is not None else '-'} {btype}".rstrip(),
        '- Added TTFB p95: n/a (not a paired run).', f"- Anomalies and notes: {entry.get('notes') or 'none'}", '',
        '#### B5. Charts', '']
    if plt is None:
        lines.append(f'- {CHARTS_SKIPPED}')
    else:
        lines += [f'- `charts/{name}`' for name in charts(plt, folder, run, res, rec, cfg, out/'charts')]
    (out/'summary.md').write_text('\n'.join(lines) + '\n', encoding='utf-8')
    return {'valid': valid, 'highest': highest, 'violated': violated, 'btype': btype, 'generator_ok': generator_ok,
            'env': env}


def row(day, run, entry, derived, prepared=None, outcome=None, digest=''):
    base = {'run_id': run, 'day': day.day, 'condition': entry['condition'], 'repeat': entry['repeat'],
            'config_id': entry['config'], 'notes': entry.get('notes', '')}
    if prepared is None:
        return base | {'valid': 'no'}
    rec, cfg, env = prepared['record'], prepared['cfg'], outcome['env']
    auto = cfg['mode'] == 'auto'
    calib = entry['condition'] == 'calib' and outcome['valid']
    return base | {
        'commit': rec['commit'], 'gateway_digest': digest,
        'replicas_fixed': '' if auto else rec['gateway']['replicas'],
        'autoscale_min': cfg.get('min', ''), 'autoscale_max': cfg.get('max', ''),
        'T': cfg['T'] if auto else (derived.get('T') if calib else ''),
        'D_s': cfg['D'] if auto else (derived.get('D_s') if calib else ''),
        'levels': env['LEVELS'], 'warmup_s': env['WARMUP_S'], 'ramp_s': env['RAMP_S'], 'hold_s': env['HOLD_S'],
        'tenants': env['TENANTS'], 'paired': env['PAIRED'], 'model': env['MODEL'], 'max_tokens': env['MAX_TOKENS'],
        'input_bytes': env['INPUT_BYTES'], 'ttfb_deadline_ms': env['TTFB_DEADLINE_MS'],
        'total_deadline_s': env['TOTAL_DEADLINE_S'], 'start_utc': rec['load_start'], 'end_utc': rec['load_end'],
        'summarize_exit': rec['summarize_exit_code'], 'valid': 'yes' if outcome['valid'] else 'no',
        'highest_passing_level_rps': outcome['highest'], 'first_violated_level_rps': outcome['violated'],
        'boundary_type': outcome['btype'], 'generator_ok': 'yes' if outcome['generator_ok'] else 'no'}


# --- build -----------------------------------------------------------------------------

def prepare(day, runner, rebuild):
    """Every input of every run, read and checked, and every CloudWatch query done, before anything is deleted."""
    derived = check_manifest(day)
    check_aws(runner)
    built = [child for child in day.out.iterdir() if child.name != day.config.name] if day.out.exists() else []
    if built and not rebuild:
        raise BundleError(f'{day.out} already holds a built bundle; pass --rebuild to replace it '
                          f'({day.config.name} is kept)')
    images = read_json(day.config/'images.json')
    digest = next((item.get('imageID', '') for item in images if 'gateflow-gateway' in item.get('image', '')), '')
    gen_cpu = generator_cpu(day)
    ident = database(day)
    prepared = {}
    for run, entry in day.runs.items():
        folder = day.folder(run)
        if (folder/'ABORTED.txt').exists():
            continue
        rec, res = read_json(folder/'run-record.json'), read_json(folder/'run.results.json')
        for name in ('run.json', 'summary.md'):
            if not (folder/name).exists():
                raise BundleError(f'{run}: no {name}')
        cfg = run_config(day, run)
        values, raw = rds(runner, day, ident, ts(rec['load_start']), ts(rec['load_end']))
        prepared[run] = {'entry': entry, 'record': rec, 'results': res, 'cfg': cfg, 'rds': values, 'raw': raw}
    return derived, built, digest, gen_cpu, prepared


def sha256(path):
    digest = hashlib.sha256()
    with open(path, 'rb') as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b''):
            digest.update(chunk)
    return digest.hexdigest()


def build(day, runner, rebuild):
    derived, built, digest, gen_cpu, prepared = prepare(day, runner, rebuild)
    plt = load_pyplot()
    if plt is None:
        print(f'note: {CHARTS_SKIPPED}')
    for child in built:
        shutil.rmtree(child) if child.is_dir() else child.unlink()
    day.config.mkdir(parents=True, exist_ok=True)
    shutil.copy2(day.evidence/day.manifest['derived'], day.out/'derived.json')
    shutil.copy2(day.manifest_path, day.config/'manifest.json')
    for name in day.manifest.get('extra_files', []):
        if (day.evidence/name).exists():
            shutil.copy2(day.evidence/name, day.config/Path(name).name)
        else:
            print(f'note: extra file {name} not found in {day.evidence}; not copied')
    rows = []
    for run, entry in day.runs.items():
        source, out = day.folder(run), day.out/run
        out.mkdir()
        if run not in prepared:
            shutil.copy2(source/'ABORTED.txt', out/'ABORTED.txt')
            rows.append(row(day, run, entry, derived))
            print('aborted', run)
            continue
        shutil.copy2(source/'run.results.json', out/'results.json')
        shutil.copy2(source/'run-record.json', out/'run-record.json')
        if (source/'prom').exists():
            shutil.copytree(source/'prom', out/'prom')
        with open(source/'run.json', 'rb') as fin, gzip.open(out/'k6.json.gz', 'wb', compresslevel=6) as fout:
            shutil.copyfileobj(fin, fout, 1 << 20)
        (out/'cloudwatch-rds.json').write_text(json.dumps(prepared[run]['raw'], indent=2), encoding='utf-8')
        outcome = summary(day, run, prepared[run], out, gen_cpu, plt)
        rows.append(row(day, run, entry, derived, prepared[run], outcome, digest))
        print('built', run, 'valid' if outcome['valid'] else 'invalid')
    with open(day.out/f'day{day.day}-runs.csv', 'w', newline='', encoding='utf-8') as stream:
        writer = csv.DictWriter(stream, fieldnames=FIELDS)
        writer.writeheader()
        writer.writerows(rows)
    (day.config/'configs.json').write_text(json.dumps(day.configs, indent=2), encoding='utf-8')
    files = sorted(path for path in day.out.rglob('*') if path.is_file() and path.name != 'SHA256SUMS')
    (day.out/'SHA256SUMS').write_text(''.join(f'{sha256(path)}  {path.relative_to(day.out).as_posix()}\n'
                                              for path in files), encoding='utf-8')
    with zipfile.ZipFile(day.zip, 'w', zipfile.ZIP_STORED) as archive:
        for path in sorted(path for path in day.out.rglob('*') if path.is_file()):
            archive.write(path, f'day{day.day}/' + path.relative_to(day.out).as_posix())
    print('zip', day.zip, day.zip.stat().st_size // 2**20, 'MiB')


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--manifest', type=Path, required=True, help='the day manifest, for example day2-manifest.json')
    parser.add_argument('--runs', type=Path, default=ROOT/'.local'/'loadtest', help='run folders (default: .local/loadtest)')
    parser.add_argument('--evidence', type=Path, help="live checks, derived and extra files (default: the manifest's folder)")
    parser.add_argument('--out', type=Path, help='bundle folder (default: <evidence>/day<N>)')
    parser.add_argument('--zip', type=Path, help='zip file (default: <out>.zip)')
    parser.add_argument('--region', default=REGION, help=f'region of the RDS instance (default: {REGION})')
    parser.add_argument('--rebuild', action='store_true', help='replace an existing bundle; day<N>-config is kept')
    return parser.parse_args(argv)


def main(argv=None, runner=subprocess.run):
    args = parse_args(argv)
    try:
        build(Day(args), runner, args.rebuild)
    except BundleError as error:
        print(f'error: {error}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
