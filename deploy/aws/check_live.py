"""Save and check the live GateFlow configuration before and after a load-test run.

Operator-run (bibi45c only), read-only: three `kubectl get ... -o json` calls in the
release's namespace (Deployments, Pods, KEDA ScaledObjects). Every query must
succeed: a failed or unparsable call is an error, never "no objects".

Checks against the --expect file (one per configuration, see expect/):

- every service of the expect file has its Deployment <release>-<service>, with the
  file's replicas, requests and limits; the gateway's replicas come from --replicas,
  and KEDA owns them with --autoscale;
- every container image carries --image-tag, and every Deployment is rolled out
  (replicas, updated, ready and available agree);
- a fixed run (--replicas N) has no ScaledObject and exactly N running gateway Pods;
- an auto run (--autoscale MIN MAX --target T --duration D) has exactly one
  ScaledObject, on the gateway, with MIN and MAX, the expect file's scale-down window,
  and the chart's two AverageValue Prometheus triggers (threshold T, the expect file's
  server address, ignoreNullValues false, the demand query multiplied by D), and has
  settled at MIN ready and running gateway Pods;
- with --baseline (the --out file of the check before the run), no restart count has
  grown, and in a fixed run no Pod has been replaced.

Writes the observed state and the problems to --out. Exit codes: 0 as expected, 1 a
problem was found, 2 a query failed (then nothing is checked) or an input file is
unreadable.
"""
import argparse
import json
from pathlib import Path
import subprocess
import sys

from common import OperatorError, kubectl, read_json_file

SCALEDOBJECTS = 'scaledobjects.keda.sh'
TRIGGERS = ['active-admitted', 'eligible-demand']


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--kubeconfig', type=Path, required=True)
    parser.add_argument('--kube-context', required=True)
    parser.add_argument('--namespace', default='gateflow')
    parser.add_argument('--release', default='gf')
    parser.add_argument('--out', type=Path, required=True, help='JSON file for the observed state and the problems')
    parser.add_argument('--expect', type=Path, required=True,
                        help='expected configuration, for example deploy/aws/expect/gateway-250m.json')
    parser.add_argument('--image-tag', required=True, help='tag every container image must carry: the deployed commit')
    parser.add_argument('--baseline', type=Path, help='the --out file of the check before the run')
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument('--replicas', type=int, metavar='N', help='fixed run with N gateway replicas')
    mode.add_argument('--autoscale', type=int, nargs=2, metavar=('MIN', 'MAX'), help='KEDA run between MIN and MAX')
    parser.add_argument('--target', help='T, the threshold of both triggers (with --autoscale)')
    parser.add_argument('--duration', help='D in seconds, the factor in the demand query (with --autoscale)')
    args = parser.parse_args(argv)
    if args.autoscale and (args.target is None or args.duration is None):
        parser.error('--autoscale needs --target and --duration')
    return args


def read_input(path, what):
    try:
        return read_json_file(path)
    except (OSError, ValueError) as error:
        raise OperatorError(f'cannot read the {what} file {path}: {type(error).__name__}') from error


def get(runner, command, kind, errors):
    """The items of `kubectl get <kind> -o json`, or None with the reason added to errors."""
    try:
        result = runner([*command, 'get', kind, '-o', 'json'], capture_output=True, text=True,
                        encoding='utf-8', errors='replace')
    except OSError as error:
        errors.append(f'kubectl get {kind} could not start: {error.strerror}')
        return None
    if result.returncode:
        errors.append(f'kubectl get {kind} exited {result.returncode}: {(result.stderr or "").strip()[:200]}')
        return None
    try:
        items = json.loads(result.stdout).get('items')
    except (ValueError, AttributeError) as error:
        errors.append(f'kubectl get {kind}: unparsable output ({type(error).__name__})')
        return None
    if not isinstance(items, list):
        errors.append(f'kubectl get {kind}: unparsable output (no items list)')
        return None
    return items


def observe(deployments, pods, scaled, release):
    def summary(item):
        spec, status = item['spec'], item.get('status') or {}
        containers = spec['template']['spec']['containers']
        return {'replicas': spec.get('replicas'), 'ready': status.get('readyReplicas', 0),
                'updated': status.get('updatedReplicas', 0), 'available': status.get('availableReplicas', 0),
                'images': [container['image'] for container in containers],
                'resources': containers[0].get('resources', {})}
    gateway = f'{release}-gateway-'
    return {
        'deployments': {item['metadata']['name']: summary(item) for item in deployments},
        'restarts': {pod['metadata']['name']: sum(c.get('restartCount', 0)
                                                  for c in pod.get('status', {}).get('containerStatuses', []))
                     for pod in pods if pod['metadata']['name'].startswith(f'{release}-')},
        'gateway_pods_running': sum(1 for pod in pods if pod['metadata']['name'].startswith(gateway)
                                    and pod.get('status', {}).get('phase') == 'Running'
                                    and not pod['metadata'].get('deletionTimestamp')),
        'scaledobjects': [item['spec'] | {'name': item['metadata']['name']} for item in scaled],
    }


def check_deployments(state, expect, args):
    problems = []
    for service, want in expect['deployments'].items():
        name = f'{args.release}-{service}'
        have = state['deployments'].get(name)
        if have is None:
            problems.append(f'{name}: missing')
            continue
        # None in an auto run: KEDA's HPA owns the gateway's spec.replicas.
        replicas = args.replicas if service == 'gateway' else want.get('replicas')
        if replicas is not None and have['replicas'] != replicas:
            problems.append(f"{name}: spec.replicas {have['replicas']} != {replicas}")
        for kind in ('requests', 'limits'):
            if kind in want and have['resources'].get(kind) != want[kind]:
                problems.append(f"{name}: {kind} {have['resources'].get(kind)} != {want[kind]}")
        if not have['ready'] == have['updated'] == have['available'] == have['replicas']:
            problems.append(f"{name}: not rolled out (replicas {have['replicas']}, updated {have['updated']}, "
                            f"ready {have['ready']}, available {have['available']})")
        for image in have['images']:
            if not image.endswith(f':{args.image_tag}'):
                problems.append(f'{name}: image {image} is not tag {args.image_tag}')
    return problems


def check_scaledobject(spec, expect, args):
    auto = expect['autoscaling']
    triggers = {trigger.get('name'): trigger for trigger in spec.get('triggers', [])}
    window = (spec.get('advanced', {}).get('horizontalPodAutoscalerConfig', {}).get('behavior', {})
              .get('scaleDown', {}).get('stabilizationWindowSeconds'))
    checks = [
        (spec.get('scaleTargetRef', {}).get('name') == f'{args.release}-gateway', f"scaleTargetRef {spec.get('scaleTargetRef')}"),
        ([spec.get('minReplicaCount'), spec.get('maxReplicaCount')] == args.autoscale,
         f"min/max {spec.get('minReplicaCount')}/{spec.get('maxReplicaCount')}"),
        (window == auto['scaleDownStabilizationSeconds'], f'scale-down stabilization window {window}'),
        (sorted(triggers) == TRIGGERS, f'trigger names {sorted(triggers)}'),
    ]
    for name, trigger in triggers.items():
        metadata = trigger.get('metadata', {})
        checks += [
            (trigger.get('type') == 'prometheus' and trigger.get('metricType') == 'AverageValue', f'{name}: type/metricType'),
            (metadata.get('threshold') == str(args.target), f"{name}: threshold {metadata.get('threshold')} != {args.target}"),
            (metadata.get('serverAddress') == auto['serverAddress'], f"{name}: serverAddress {metadata.get('serverAddress')}"),
            (metadata.get('ignoreNullValues') == 'false', f'{name}: ignoreNullValues'),
        ]
    demand = triggers.get('eligible-demand', {}).get('metadata', {}).get('query', '')
    checks.append((f') * {args.duration} ' in demand, f'eligible-demand query does not multiply by D={args.duration}'))
    return [message for ok, message in checks if not ok]


def check(state, expect, args, baseline):
    problems = check_deployments(state, expect, args)
    scaled = state['scaledobjects']
    if args.replicas is not None:
        if scaled:
            problems.append(f"fixed run but ScaledObject present: {[item['name'] for item in scaled]}")
        if state['gateway_pods_running'] != args.replicas:
            problems.append(f"gateway Pods running {state['gateway_pods_running']} != {args.replicas}")
    else:
        if len(scaled) != 1:
            problems.append(f'auto run needs exactly one ScaledObject, found {len(scaled)}')
        else:
            problems += check_scaledobject(scaled[0], expect, args)
        gateway = state['deployments'].get(f'{args.release}-gateway', {})
        minimum = args.autoscale[0]
        if gateway.get('ready') != minimum or state['gateway_pods_running'] != minimum:
            problems.append(f"auto run not settled at min {minimum}: ready {gateway.get('ready')}, "
                            f"running {state['gateway_pods_running']}")
    if baseline is not None:
        before = baseline.get('restarts', {})
        for pod, count in state['restarts'].items():
            if count > before.get(pod, 0):
                problems.append(f'{pod}: restarts {before.get(pod, 0)} -> {count}')
        gone = sorted(set(before) - set(state['restarts']))
        if gone and args.replicas is not None:
            problems.append(f'Pods replaced since baseline: {gone}')
    return problems


def write(path, state):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(state, indent=2) + '\n', encoding='utf-8')


def main(argv=None, runner=subprocess.run):
    args = parse_args(argv)
    try:
        expect = read_input(args.expect, 'expect')
        baseline = read_input(args.baseline, 'baseline') if args.baseline else None
        if not isinstance(expect.get('deployments'), dict) or 'autoscaling' not in expect:
            raise OperatorError(f'{args.expect} needs "deployments" and "autoscaling"')
    except OperatorError as error:
        print(f'FAIL {error}', file=sys.stderr)
        return 2
    command = kubectl(args.kubeconfig, args.kube_context, args.namespace)
    errors = []
    deployments, pods, scaled = (get(runner, command, kind, errors) for kind in ('deployments', 'pods', SCALEDOBJECTS))
    state = {'expect': args.expect.name, 'config': expect.get('config'), 'namespace': args.namespace,
             'release': args.release, 'image_tag': args.image_tag, 'errors': errors}
    if errors:
        state['problems'] = ['query failed; nothing was checked']
        write(args.out, state)
        print('\n'.join(errors), '->', args.out)
        return 2
    state |= observe(deployments, pods, scaled, args.release)
    state['problems'] = check(state, expect, args, baseline)
    write(args.out, state)
    print('\n'.join(state['problems']) or 'live configuration as expected', '->', args.out)
    return 1 if state['problems'] else 0


if __name__ == '__main__':
    sys.exit(main())
