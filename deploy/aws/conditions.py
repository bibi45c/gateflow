"""Operator steps of a load-test condition on EKS: ladders, upgrades, drain, settle and runs.

Operator-run (bibi45c only). Each subcommand is one gate of a cloud day and reports
through its exit code, so a calling script can stop at the first failed gate (see
README.md, Cloud day tooling, for a flow).

  levels fixed  LEVELS of a fixed-replica ladder: round(N x L x factor) per --factors.
  levels auto   LEVELS of an autoscaling ladder: round((k - 0.5) x T / D) per replica
                step k of --steps; a level already in the ladder is lowered by 1 until
                it is new, because k6 rejects a repeated level.
  upgrade       With --terraform-output, first refreshes the RDS master password in
                the Secrets (secrets.py --refresh-master-only). Then `helm upgrade
                --reuse-values` with the --values files, for a fixed gateway (--fixed N,
                then waits for the rollout) or for KEDA (--auto --target T --duration D,
                then settles at the ScaledObject's minReplicaCount).
  drain         Waits until no admitted request is active on the gateway, as Prometheus
                inside the cluster sees it.
  settle        Waits until the gateway Deployment has exactly N ready replicas and
                exactly N gateway Pods exist; on timeout prints the Deployment, HPA,
                ScaledObject and the latest events.
  run           One counted run: kube_run.py with LEVELS (output in --log), then
                prom_export.py of its window (without the HPA queries for a fixed run),
                then the verdict of every hold level.

drain reads three instant queries per poll, through `kubectl exec` into the Prometheus
Deployment: up{job=J}, timestamp(up{job=J}) and the active gauge of job J. Every
answer must be a successful vector; anything else is a query failure and ends the
drain at once with exit 2, never read as 0 (drained) or as still active. The reading
counts only when every gateway target is up, freshly scraped (--max-sample-age) and
carries the gauge, and, with --targets N, exactly N targets exist. No target at all
means no gateway Pod is scraped, which is a different state from "still active": it
is reported as such, with the count, and ends with exit 3 after --target-wait seconds.

Exit codes: 0 done; 1 not reached in time or a command failed (drain: requests still
active at --timeout; settle: not settled); 2 a query failed or an input is unusable;
3 (drain only) no measurable gateway target, so the drain cannot be judged. run
returns kube_run.py's exit code when it is not 0, and otherwise prom_export.py's.
"""
import argparse
import json
import math
from pathlib import Path
import subprocess
import sys
import time
import urllib.parse

from common import ROOT, OperatorError, kubectl

HERE = Path(__file__).resolve().parent
ACTIVE = 'gateflow_gateway_active_admitted_requests'
KEDA_PAUSE = 'autoscaling.keda.sh/paused-replicas'
DRAINED, ACTIVE_LEFT, UNMEASURABLE = 'drained', 'active', 'unmeasurable'
FACTORS = '0.8,1.0,1.15,1.3'
STEPS = '2,3,4,3,2'


class QueryError(Exception):
    """A query without a usable answer: never read as zero, nor as still active."""


def positive(text):
    value = float(text)
    if not math.isfinite(value) or value <= 0:
        raise argparse.ArgumentTypeError('a positive number')
    return value


def number_text(text):
    """A positive number, kept as written: the chart and check_live.py compare its text."""
    positive(text)
    return text


def numbers(text):
    try:
        return [positive(item) for item in text.split(',')]
    except (ValueError, argparse.ArgumentTypeError):
        raise argparse.ArgumentTypeError('comma-separated positive numbers') from None


# --- levels --------------------------------------------------------------------------

def fixed_levels(replicas, level, factors):
    return [round(replicas * level * factor) for factor in factors]


def auto_levels(target, duration, steps):
    levels = []
    for step in steps:
        value = round((step - 0.5) * target / duration)
        while value in levels:
            value -= 1
        if value <= 0:
            raise OperatorError(f'replica step {step:g} gives no positive level for T {target:g} and D {duration:g}')
        levels.append(value)
    return levels


def run_levels(args):
    if args.mode == 'fixed':
        levels = fixed_levels(args.replicas, args.level, args.factors)
    else:
        levels = auto_levels(args.target, args.duration, args.steps)
    print(','.join(map(str, levels)))
    return 0


# --- queries -------------------------------------------------------------------------

def json_from(runner, command, what):
    try:
        result = runner(command, capture_output=True, text=True, encoding='utf-8', errors='replace')
    except OSError as error:
        raise QueryError(f'{what}: {command[0]} could not start: {error.strerror}') from error
    if result.returncode:
        lines = (result.stderr or '').strip().splitlines()
        raise QueryError(f'{what}: {command[0]} exited with {result.returncode}: '
                         f'{lines[-1] if lines else "no error output"}')
    try:
        return json.loads(result.stdout)
    except ValueError:
        raise QueryError(f'{what}: the answer is not JSON') from None


def prometheus(runner, args):
    """Returns query(expression) -> [(labels, value, evaluation time)]; raises QueryError."""
    command = [*kubectl(args.kubeconfig, args.kube_context, args.monitoring_namespace), 'exec',
               f'deploy/{args.prometheus}', '-c', args.prometheus_container, '--', 'wget', '-qO-']

    def query(expression):
        url = f'{args.prometheus_url}/api/v1/query?{urllib.parse.urlencode({"query": expression})}'
        document = json_from(runner, [*command, url], expression)
        if not isinstance(document, dict) or document.get('status') != 'success':
            reason = document.get('error') if isinstance(document, dict) else None
            raise QueryError(f'{expression}: Prometheus did not answer success: {reason or "no status"}')
        data = document.get('data') or {}
        if data.get('resultType') != 'vector' or not isinstance(data.get('result'), list):
            raise QueryError(f'{expression}: the answer is not an instant vector')
        samples = []
        for item in data['result']:
            try:
                at, value = float(item['value'][0]), float(item['value'][1])
            except (KeyError, IndexError, TypeError, ValueError):
                raise QueryError(f'{expression}: unreadable sample {item!r}') from None
            if not math.isfinite(value):
                raise QueryError(f'{expression}: value {item["value"][1]} is not a number')
            samples.append((item.get('metric') or {}, value, at))
        return samples
    return query


def reading(query, job, targets=None, max_age=60):
    """(state, detail) of one drain poll; state is DRAINED, ACTIVE_LEFT or UNMEASURABLE."""
    selector = f'{{job="{job}"}}'
    up = query(f'up{selector}')
    if not up:
        return UNMEASURABLE, f'no gateway target: count(up{selector}) = 0, so no gateway Pod is scraped'
    instances = {labels.get('instance', '?') for labels, _, _ in up}
    down = sorted(labels.get('instance', '?') for labels, value, _ in up if value != 1)
    if down:
        return UNMEASURABLE, f'{len(down)} of {len(up)} gateway targets down: {", ".join(down)}'
    if targets is not None and len(up) != targets:
        return UNMEASURABLE, f'count(up{selector}) = {len(up)}, expected {targets} gateway targets'
    ages = [at - value for _, value, at in query(f'timestamp(up{selector})')]
    if len(ages) != len(up):
        return UNMEASURABLE, f'timestamp(up{selector}) has {len(ages)} series for {len(up)} targets'
    if max(ages) > max_age:
        return UNMEASURABLE, f'the oldest gateway scrape is {max(ages):.0f}s old, more than {max_age:g}s'
    series = query(f'{ACTIVE}{selector}')
    missing = sorted(instances - {labels.get('instance', '?') for labels, _, _ in series})
    if missing:
        return UNMEASURABLE, f'{ACTIVE} is missing on {", ".join(missing)}'
    active = sum(value for _, value, _ in series)
    detail = f'{active:g} active admitted requests on {len(up)} gateway targets'
    return (DRAINED if active == 0 else ACTIVE_LEFT), detail


def run_drain(args, runner, sleep, clock):
    query = prometheus(runner, args)
    start = clock()
    unmeasurable_since = None
    while True:
        try:
            state, detail = reading(query, args.job, args.targets, args.max_sample_age)
        except QueryError as error:
            print(f'FAIL drain: query failed: {error}', file=sys.stderr)
            return 2
        now = clock()
        if state == DRAINED:
            print(f'drained: {detail}')
            return 0
        deadline = start + args.timeout
        if state == UNMEASURABLE:
            unmeasurable_since = now if unmeasurable_since is None else unmeasurable_since
            deadline = min(deadline, unmeasurable_since + args.target_wait)
            if now >= deadline:
                print(f'FAIL drain: {detail}, for {now - unmeasurable_since:.0f}s; the drain cannot be judged',
                      file=sys.stderr)
                return 3
        else:
            unmeasurable_since = None
            if now >= deadline:
                print(f'FAIL drain: still active after {now - start:.0f}s: {detail}', file=sys.stderr)
                return 1
        print(f'waiting: {detail}', file=sys.stderr, flush=True)
        sleep(min(args.interval, deadline - now))


# --- settle --------------------------------------------------------------------------

def gateway_state(runner, args):
    kube = kubectl(args.kubeconfig, args.kube_context, args.namespace)
    deployment = json_from(runner, [*kube, 'get', 'deployment', f'{args.release}-gateway', '-o', 'json'],
                           'get gateway Deployment')
    pods = json_from(runner, [*kube, 'get', 'pods', '-o', 'json'], 'get pods')
    try:
        ready = (deployment.get('status') or {}).get('readyReplicas', 0)
        names = [pod['metadata']['name'] for pod in pods['items']
                 if pod['metadata']['name'].startswith(f'{args.release}-gateway-')]
    except (AttributeError, KeyError, TypeError):
        raise QueryError('the Deployment or Pod list has an unexpected shape') from None
    return ready, names


def conditions_text(item):
    return '; '.join(f"{c.get('type')}={c.get('status')} {c.get('reason', '')} {c.get('message', '')}".strip()
                     for c in (item.get('status') or {}).get('conditions') or []) or 'no conditions'


def diagnose(runner, args):
    """Best effort: why the gateway did not settle."""
    kube = kubectl(args.kubeconfig, args.kube_context, args.namespace)
    lines = []
    try:
        item = json_from(runner, [*kube, 'get', 'deployment', f'{args.release}-gateway', '-o', 'json'], 'Deployment')
        status = item.get('status') or {}
        lines.append(f"Deployment {args.release}-gateway: spec.replicas {item['spec'].get('replicas')}, status replicas "
                     f"{status.get('replicas', 0)}, updated {status.get('updatedReplicas', 0)}, ready "
                     f"{status.get('readyReplicas', 0)}, available {status.get('availableReplicas', 0)}; "
                     f'{conditions_text(item)}')
    except (QueryError, KeyError, AttributeError) as error:
        lines.append(f'Deployment: unavailable: {error}')
    for kind in ('horizontalpodautoscalers', 'scaledobjects.keda.sh'):
        try:
            for item in json_from(runner, [*kube, 'get', kind, '-o', 'json'], kind).get('items', []):
                status = item.get('status') or {}
                paused = (item['metadata'].get('annotations') or {}).get(KEDA_PAUSE)
                extra = (f"current {status.get('currentReplicas')}, desired {status.get('desiredReplicas')}"
                         if kind == 'horizontalpodautoscalers' else f'paused-replicas {paused}')
                lines.append(f"{kind} {item['metadata']['name']}: {extra}; {conditions_text(item)}")
        except (QueryError, KeyError, AttributeError) as error:
            lines.append(f'{kind}: unavailable: {error}')
    try:
        result = runner([*kube, 'get', 'events', '--sort-by=.lastTimestamp'], capture_output=True, text=True,
                        encoding='utf-8', errors='replace')
        lines += ['latest events:', *(result.stdout or '').strip().splitlines()[-args.events:]]
    except OSError as error:
        lines.append(f'events: unavailable: {error.strerror}')
    return lines


def settle(args, runner, sleep, clock, replicas):
    start = clock()
    while True:
        try:
            ready, pods = gateway_state(runner, args)
        except QueryError as error:
            print(f'FAIL settle: query failed: {error}', file=sys.stderr)
            return 2
        detail = f'{ready} ready, {len(pods)} gateway Pods'
        if ready == replicas and len(pods) == replicas:
            print(f'settled at {replicas}: {detail}')
            return 0
        now = clock()
        if now - start >= args.settle_timeout:
            print(f'FAIL settle: not settled at {replicas} after {now - start:.0f}s: {detail}', file=sys.stderr)
            for line in diagnose(runner, args):
                print(f'  {line}', file=sys.stderr)
            return 1
        print(f'waiting for {replicas}: {detail}', file=sys.stderr, flush=True)
        sleep(min(args.interval, start + args.settle_timeout - now))


def run_settle(args, runner, sleep, clock):
    return settle(args, runner, sleep, clock, args.replicas)


# --- upgrade -------------------------------------------------------------------------

def step(runner, command, what):
    """Runs command with its output passed through; True when it exits with 0."""
    try:
        code = runner(command).returncode
    except OSError as error:
        print(f'FAIL {what}: {command[0]} could not start: {error.strerror}', file=sys.stderr)
        return False
    if code:
        print(f'FAIL {what}: exited with {code}', file=sys.stderr)
    return code == 0


def scaledobject_minimum(runner, args):
    command = [*kubectl(args.kubeconfig, args.kube_context, args.namespace), 'get', 'scaledobject.keda.sh',
               f'{args.release}-gateway', '-o', 'json']
    document = json_from(runner, command, 'get ScaledObject')
    try:
        return int(document['spec']['minReplicaCount'])
    except (KeyError, TypeError, ValueError):
        raise QueryError('the ScaledObject has no minReplicaCount') from None


def run_upgrade(args, runner, sleep, clock):
    if args.terraform_output:
        refresh = [sys.executable, str(HERE/'secrets.py'), '--kubeconfig', str(args.kubeconfig),
                   '--kube-context', args.kube_context, '--namespace', args.namespace,
                   '--terraform-output', str(args.terraform_output), '--refresh-master-only']
        if args.secrets_state:
            refresh += ['--state', str(args.secrets_state)]
        if not step(runner, refresh, 'secrets.py --refresh-master-only'):
            return 1
    if args.fixed is not None:
        sets = ['--set', 'autoscaling.enabled=false', '--set', f'services.gateway.replicas={args.fixed}']
    else:
        sets = ['--set', 'autoscaling.enabled=true', '--set-json', f'autoscaling.targetPerReplica={args.target}',
                '--set-json', f'autoscaling.requestDurationSeconds={args.duration}']
    helm = ['helm', '--kubeconfig', str(args.kubeconfig), '--kube-context', args.kube_context, 'upgrade',
            args.release, str(args.chart), '-n', args.namespace]
    for values in args.values or [ROOT/'deploy'/'helm'/'values-aws.yaml']:
        helm += ['-f', str(values)]
    helm += ['--reuse-values', *sets, '--wait', '--wait-for-jobs', '--timeout', args.helm_timeout]
    if not step(runner, helm, 'helm upgrade'):
        return 1
    if args.fixed is not None:
        rollout = [*kubectl(args.kubeconfig, args.kube_context, args.namespace), 'rollout', 'status',
                   f'deploy/{args.release}-gateway', f'--timeout={args.settle_timeout:g}s']
        return 0 if step(runner, rollout, 'rollout status') else 1
    try:
        minimum = scaledobject_minimum(runner, args)
    except QueryError as error:
        print(f'FAIL upgrade: query failed: {error}', file=sys.stderr)
        return 2
    return settle(args, runner, sleep, clock, minimum)


# --- run -----------------------------------------------------------------------------

def passes(row):
    return row['scheduled'] > 0 and row['completed'] * 1000 >= 995 * row['scheduled']


def verdicts(results):
    lines = [f"checks {results.get('checks')}"]
    for row in results.get('phases', []):
        if row.get('phase') == 'hold':
            lines.append(f" hold {row['level_rps']:g} {row['completed']}/{row['scheduled']} "
                         f"{'pass' if passes(row) else 'FAIL'} ttfb_p95 {(row.get('ttfb_ms') or {}).get('p95')} "
                         f"vus {row.get('max_active_vus')}")
    return lines


def run_load(args, runner):
    folder = args.runs/args.name
    log = args.log or ROOT/'.local'/'cloud-day'/f'{args.name}.log'
    log.parent.mkdir(parents=True, exist_ok=True)
    extra = args.kube_run_args[1:] if args.kube_run_args[:1] == ['--'] else args.kube_run_args
    command = [sys.executable, str(ROOT/'loadtest'/'kube_run.py'), '--kubeconfig', str(args.kubeconfig),
               '--kube-context', args.kube_context, '--namespace', args.namespace, '--release', args.release,
               '--name', args.name, '--out-dir', str(folder), '--env', f'LEVELS={args.levels}', *extra]
    try:
        with open(log, 'w', encoding='utf-8') as stream:
            code = runner(command, stdout=stream, stderr=subprocess.STDOUT).returncode
    except OSError as error:
        print(f'FAIL kube_run.py could not start: {error.strerror}', file=sys.stderr)
        return 1
    lines = log.read_text(encoding='utf-8', errors='replace').strip().splitlines()
    print(f"kube_run.py exit {code}: {lines[-1] if lines else 'no output'} (log {log})")
    record = folder/'run-record.json'
    if not record.exists():
        print(f'FAIL no {record}; nothing to export', file=sys.stderr)
        return code or 1
    export = [sys.executable, str(ROOT/'loadtest'/'prom_export.py'), '--kubeconfig', str(args.kubeconfig),
              '--kube-context', args.kube_context, '--record', str(record)]
    if args.mode == 'fixed':
        export += ['--skip', 'hpa_desired', '--skip', 'hpa_conditions']
    try:
        export_code = runner(export).returncode
    except OSError as error:
        print(f'FAIL prom_export.py could not start: {error.strerror}', file=sys.stderr)
        export_code = 1
    results = folder/'run.results.json'
    if results.exists():
        for line in verdicts(json.loads(results.read_text(encoding='utf-8'))):
            print(line)
    return code or export_code


# --- arguments -----------------------------------------------------------------------

def cluster_arguments(parser):
    parser.add_argument('--kubeconfig', type=Path, required=True)
    parser.add_argument('--kube-context', required=True)
    parser.add_argument('--namespace', default='gateflow')
    parser.add_argument('--release', default='gf')
    parser.add_argument('--interval', type=positive, default=10, help='seconds between polls (default: 10)')


def settle_arguments(parser, default=600):
    parser.add_argument('--settle-timeout', type=positive, default=default, metavar='SECONDS',
                        help=f'seconds to wait for the gateway (default: {default})')
    parser.add_argument('--events', type=int, default=15, help='latest events printed when not settled (default: 15)')


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    commands = parser.add_subparsers(dest='command', required=True)

    levels = commands.add_parser('levels', help='print the LEVELS of a ladder')
    modes = levels.add_subparsers(dest='mode', required=True)
    fixed = modes.add_parser('fixed')
    fixed.add_argument('--replicas', type=int, required=True, help='N, the fixed gateway replicas')
    fixed.add_argument('--level', type=positive, required=True, help='L, the calibrated single-Pod level (req/s)')
    fixed.add_argument('--factors', type=numbers, default=numbers(FACTORS), help=f'(default: {FACTORS})')
    auto = modes.add_parser('auto')
    auto.add_argument('--target', type=positive, required=True, help='T, active requests per replica')
    auto.add_argument('--duration', type=positive, required=True, help='D, mean request duration (s)')
    auto.add_argument('--steps', type=numbers, default=numbers(STEPS), help=f'replica steps k (default: {STEPS})')

    upgrade = commands.add_parser('upgrade', help='helm upgrade to a fixed or autoscaling gateway')
    cluster_arguments(upgrade)
    settle_arguments(upgrade)
    upgrade.add_argument('--chart', type=Path, default=ROOT/'deploy'/'helm'/'gateflow')
    upgrade.add_argument('--values', type=Path, action='append', metavar='FILE',
                         help='values file, in order (repeatable; default: deploy/helm/values-aws.yaml)')
    upgrade.add_argument('--helm-timeout', default='15m')
    upgrade.add_argument('--terraform-output', type=Path, metavar='JSON',
                         help='refresh the RDS master password with secrets.py first')
    upgrade.add_argument('--secrets-state', type=Path, help="secrets.py's --state file")
    mode = upgrade.add_mutually_exclusive_group(required=True)
    mode.add_argument('--fixed', type=int, metavar='N', help='autoscaling off, N gateway replicas')
    mode.add_argument('--auto', action='store_true', help='autoscaling on, with --target and --duration')
    upgrade.add_argument('--target', type=number_text, help='T: autoscaling.targetPerReplica')
    upgrade.add_argument('--duration', type=number_text, help='D: autoscaling.requestDurationSeconds')

    drain = commands.add_parser('drain', help='wait until no admitted request is active')
    cluster_arguments(drain)
    drain.add_argument('--timeout', type=positive, default=300, help='seconds to wait for 0 (default: 300)')
    drain.add_argument('--target-wait', type=positive, default=60, metavar='SECONDS',
                       help='seconds to wait for every gateway target to be measurable (default: 60)')
    drain.add_argument('--targets', type=int, metavar='N', help='require exactly N gateway targets')
    drain.add_argument('--job', default='gateflow-gateway', help='Prometheus job of the gateway (default: gateflow-gateway)')
    drain.add_argument('--max-sample-age', type=positive, default=60, metavar='SECONDS',
                       help='oldest accepted gateway scrape (default: 60)')
    drain.add_argument('--monitoring-namespace', default='monitoring')
    drain.add_argument('--prometheus', default='prometheus-server', help='Prometheus Deployment (default: prometheus-server)')
    drain.add_argument('--prometheus-container', default='prometheus-server')
    drain.add_argument('--prometheus-url', default='http://localhost:9090', help='Prometheus inside its Pod')

    settle_parser = commands.add_parser('settle', help='wait for exactly N ready gateway replicas and Pods')
    cluster_arguments(settle_parser)
    settle_arguments(settle_parser)
    settle_parser.add_argument('--replicas', type=int, required=True, metavar='N')

    run = commands.add_parser('run', help='kube_run.py, prom_export.py and the hold verdicts')
    run.add_argument('--kubeconfig', type=Path, required=True)
    run.add_argument('--kube-context', required=True)
    run.add_argument('--namespace', default='gateflow')
    run.add_argument('--release', default='gf')
    run.add_argument('--name', required=True, help='run ID, for example d3-rep1-r1')
    run.add_argument('--mode', choices=('fixed', 'auto'), required=True)
    run.add_argument('--levels', required=True, help='LEVELS, for example the output of `levels`')
    run.add_argument('--runs', type=Path, default=ROOT/'.local'/'loadtest', help='run folders (default: .local/loadtest)')
    run.add_argument('--log', type=Path, help="kube_run.py's output (default: .local/cloud-day/<name>.log)")
    run.add_argument('kube_run_args', nargs=argparse.REMAINDER, help='after --: more kube_run.py arguments')

    args = parser.parse_args(argv)
    if args.command == 'upgrade' and args.auto and (args.target is None or args.duration is None):
        parser.error('--auto needs --target and --duration')
    if args.command == 'upgrade' and args.fixed is not None and (args.target or args.duration):
        parser.error('--target and --duration belong to --auto')
    return args


def main(argv=None, runner=subprocess.run, sleep=time.sleep, clock=time.monotonic):
    args = parse_args(argv)
    try:
        if args.command == 'levels':
            return run_levels(args)
        if args.command == 'drain':
            return run_drain(args, runner, sleep, clock)
        if args.command == 'settle':
            return run_settle(args, runner, sleep, clock)
        if args.command == 'upgrade':
            return run_upgrade(args, runner, sleep, clock)
        return run_load(args, runner)
    except OperatorError as error:
        print(f'FAIL {error}', file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
