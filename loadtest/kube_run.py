"""Run loadtest/messages.js inside the cluster and build its results table locally.

Operator-run. Puts messages.js into a ConfigMap and starts k6 in a Pod on the
load-generator node, aimed at the gateway Service, so the measurement leaves out the
ALB, the NAT and the operator's own link. When k6 has finished, its JSON output is
copied back, the Pod and the ConfigMap are deleted, loadtest/summarize.py builds the
results table and a JSON run record is written next to it.

Requires kubectl on PATH. Never uses the default kubeconfig.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import math
from pathlib import Path
import re
import subprocess
import sys
import time

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent
SCRIPT = HERE/'messages.js'
K6_IMAGE = 'grafana/k6:2.3.0@sha256:9c2dee7f8ed74d317e4027c06a10f169b625638189de8d4555d0b3486a5aeb34'
K6_USER = 12345  # the k6 user of the image; runAsNonRoot needs it numeric
SECRET = 'gateflow-runtime'
SEED_KEY = 'ACCESS_SEED_SECRET'
# The messages.js settings --env may set. The target URLs follow from --release, and
# the seed secret comes only from the Secret, never from the command line.
SETTINGS = ('PAIRED', 'MODEL', 'MAX_TOKENS', 'INPUT_BYTES', 'TENANTS', 'LEVELS', 'WARMUP_S', 'RAMP_S',
            'HOLD_S', 'TTFB_DEADLINE_MS', 'TOTAL_DEADLINE_S', 'MOCK_FIRST_EVENT_DELAY_MS', 'MAX_VUS', 'STREAM_S')
FROM_RELEASE = ('GATEWAY_URL', 'MOCK_URL')
WORKLOAD = {'gateflow.io/workload': 'load-generator'}
PREFIX = 'gateflow-k6-'
NAME = re.compile(r'[a-z0-9]([-a-z0-9]*[a-z0-9])?')
CPU = re.compile(r'\d+(\.\d+)?m?')
MEMORY = re.compile(r'\d+(Ki|Mi|Gi)?')
GRACE_S = 900  # the container stays this long after k6 so the output can be copied
MARGIN_S = 900  # default timeout on top of the plan: scheduling, image pull, setup
POLL_S = 10
# Waiting reasons that never resolve by themselves.
FATAL = {'CreateContainerConfigError', 'CreateContainerError', 'InvalidImageName', 'ErrImageNeverPull'}
OUTPUTS = ('exit', 'run.json', 'run.results.json', 'summary.md', 'k6.log', 'run-record.json')
COMMAND = ('k6 run --no-usage-report --quiet --out json=/out/run.json /scripts/messages.js; '
           'echo $? > /out/exit; sleep {grace}')
INTERRUPTED = 'interrupted'
SUMMARIZE_INTERRUPTED = 'interrupted while summarize.py ran'


def load_deploy():
    """deploy/helm/deploy.py, for its handling of local paths and the repository HEAD."""
    spec = importlib.util.spec_from_file_location('gateflow_helm_deploy', ROOT/'deploy'/'helm'/'deploy.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


deploy = load_deploy()


class KubeRunError(Exception):
    pass


def setting(text):
    key, sep, value = text.partition('=')
    if not sep:
        raise argparse.ArgumentTypeError(f'expected KEY=VALUE, got {key!r}')
    # Only the key is ever echoed, so a rejected secret value is not printed.
    if key == SEED_KEY:
        raise argparse.ArgumentTypeError(f'{SEED_KEY} is read from the {SECRET} Secret, not from --env')
    if key in FROM_RELEASE:
        raise argparse.ArgumentTypeError(f'{key} is the in-cluster Service of --release, not set with --env')
    if key not in SETTINGS:
        raise argparse.ArgumentTypeError(f'{key} is not a setting of messages.js; use one of {", ".join(SETTINGS)}')
    return key, value


def plan_seconds(env):
    """Seconds the plan of env lasts with messages.js defaults, graceful stop included."""
    def value(name, default):
        number = float(env.get(name) or default)  # messages.js treats an empty value as unset
        if not math.isfinite(number) or number < 0:
            raise ValueError(f'{name} must be a non-negative number')
        return number
    levels = len((env.get('LEVELS') or '5,25,50').split(','))
    return (value('WARMUP_S', 60) + (levels-1)*value('RAMP_S', 60) + levels*value('HOLD_S', 180)
            + value('TOTAL_DEADLINE_S', 60) + 5)


def parse_args(argv, now):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--kubeconfig', type=Path, required=True)
    parser.add_argument('--kube-context', required=True)
    parser.add_argument('--namespace', default='gateflow')
    parser.add_argument('--release', default='gf')
    parser.add_argument('--name', help='run name, lowercase letters, digits and dashes (default: UTC time)')
    parser.add_argument('--out-dir', type=Path, help='output folder (default: .local/loadtest/<name>/)')
    parser.add_argument('--env', type=setting, action='append', default=[], metavar='KEY=VALUE',
                        help=f'messages.js setting, repeatable: {", ".join(SETTINGS)}')
    parser.add_argument('--timeout', type=int, help=f'seconds to wait for k6 (default: the plan plus {MARGIN_S})')
    parser.add_argument('--k6-cpu', default='2', help='CPU request and limit of k6 (default: 2)')
    parser.add_argument('--k6-memory', default='4Gi', help='memory request and limit of k6 (default: 4Gi)')
    args = parser.parse_args(argv)
    keys = [key for key, _ in args.env]
    repeated = sorted({key for key in keys if keys.count(key) > 1})
    if repeated:
        parser.error(f'--env sets {", ".join(repeated)} more than once')
    args.env = dict(args.env)
    args.name = args.name or stamp_name(now)
    if not NAME.fullmatch(args.name) or len(PREFIX + args.name) > 63:
        parser.error(f'--name must be lowercase letters, digits and dashes, at most {63-len(PREFIX)} characters')
    if not CPU.fullmatch(args.k6_cpu) or not MEMORY.fullmatch(args.k6_memory):
        parser.error('--k6-cpu is like 2 or 500m, --k6-memory like 4Gi or 512Mi')
    if args.timeout is None:
        try:
            args.timeout = math.ceil(plan_seconds(args.env)) + MARGIN_S
        except ValueError as error:
            parser.error(f'cannot work out the plan length: {error}')
    elif args.timeout <= 0:
        parser.error('--timeout must be positive')
    args.out_dir = args.out_dir or ROOT/'.local'/'loadtest'/args.name
    return args


def stamp_name(now):
    return now.strftime('%Y%m%d-%H%M%S')


def labels(args):
    return {'app.kubernetes.io/name': 'k6', 'app.kubernetes.io/component': 'load-generator',
            'gateflow.io/k6-run': args.name}


def target_env(args):
    """Every environment variable of k6 except the seed secret."""
    return {'GATEWAY_URL': f'http://{args.release}-gateway:8080',
            'MOCK_URL': f'http://{args.release}-mock-upstream:8090', **dict(sorted(args.env.items()))}


def config_map(args, script):
    return {'apiVersion': 'v1', 'kind': 'ConfigMap',
            'metadata': {'name': PREFIX + args.name, 'labels': labels(args)},
            'data': {'messages.js': script}}


def pod(args):
    name = PREFIX + args.name
    env = [{'name': key, 'value': value} for key, value in target_env(args).items()]
    env.append({'name': SEED_KEY, 'valueFrom': {'secretKeyRef': {'name': SECRET, 'key': SEED_KEY}}})
    size = {'cpu': args.k6_cpu, 'memory': args.k6_memory}
    container = {'name': 'k6', 'image': K6_IMAGE, 'command': ['sh', '-c', COMMAND.format(grace=GRACE_S)],
                 'env': env,
                 # Requests equal to limits: k6 is never throttled below them, and cannot
                 # take the CPU of the mock upstream that shares its node.
                 'resources': {'requests': size, 'limits': size},
                 'securityContext': {'allowPrivilegeEscalation': False, 'capabilities': {'drop': ['ALL']}},
                 'volumeMounts': [{'name': 'script', 'mountPath': '/scripts', 'readOnly': True},
                                  {'name': 'out', 'mountPath': '/out'}]}
    spec = {'restartPolicy': 'Never', 'automountServiceAccountToken': False,
            # A last stop if this script dies without deleting the Pod.
            'activeDeadlineSeconds': args.timeout + GRACE_S,
            'terminationGracePeriodSeconds': 5,
            'securityContext': {'runAsNonRoot': True, 'runAsUser': K6_USER, 'runAsGroup': K6_USER,
                                'seccompProfile': {'type': 'RuntimeDefault'}},
            'nodeSelector': dict(WORKLOAD),
            'tolerations': [{'key': key, 'operator': 'Equal', 'value': value, 'effect': 'NoSchedule'}
                            for key, value in WORKLOAD.items()],
            'containers': [container],
            'volumes': [{'name': 'script', 'configMap': {'name': name}}, {'name': 'out', 'emptyDir': {}}]}
    return {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': name, 'labels': labels(args)}, 'spec': spec}


def capture(runner, command, **kwargs):
    result = runner(command, capture_output=True, text=True, encoding='utf-8', **kwargs)
    if result.returncode:
        kubeconfig = command[command.index('--kubeconfig')+1] if '--kubeconfig' in command else None
        stderr = deploy.scrub('\n'.join((result.stderr or '').strip().splitlines()[-5:]),
                              deploy.private_paths(kubeconfig))
        raise KubeRunError(f'{deploy.command_name(command)} exited with {result.returncode}: {stderr}')
    return result.stdout


def create(runner, kube, document, created):
    """Creates document, never replacing an object of the same name, and notes it for deletion."""
    ref = f"{document['kind'].lower()}/{document['metadata']['name']}"
    # Noted before the call, so an interrupted create is still deleted.
    created.append(ref)
    try:
        capture(runner, [*kube, 'create', '-f', '-'], input=json.dumps(document))
    except KubeRunError:
        # Nothing was created; an object already of that name belongs to another run.
        created.remove(ref)
        raise


def leftovers(created):
    """How to delete what this run created by hand; it names only the objects, never the kubeconfig."""
    return f"delete them with kubectl -n <namespace> delete {' '.join(reversed(created))}"


def delete(runner, kube, created):
    """Deletes what this run created; returns a problem or None."""
    if not created:
        return None
    command = [*kube, 'delete', *reversed(created), '--ignore-not-found', '--wait=false']
    try:
        capture(runner, command)
    except (KubeRunError, OSError) as error:
        return f'{error}; {leftovers(created)}'
    return None


def gateway_state(runner, kube, release):
    """Replicas and image of the gateway Deployment."""
    item = json.loads(capture(runner, [*kube, 'get', 'deployment', f'{release}-gateway', '-o', 'json']))
    containers = item['spec']['template']['spec']['containers']
    container = next((c for c in containers if c['name'] == 'gateway'), containers[0])
    return {'replicas': item['spec'].get('replicas'),
            'ready_replicas': item.get('status', {}).get('readyReplicas', 0),
            'image': container['image']}


def pod_problem(status):
    """Why the Pod is not running, from its container or scheduling status, or ''."""
    for container in status.get('containerStatuses') or []:
        for state in ('waiting', 'terminated'):
            detail = (container.get('state') or {}).get(state)
            if detail:
                return f"{detail.get('reason', state)}: {detail.get('message', '')}".rstrip(': ')
    for condition in status.get('conditions') or []:
        if condition.get('type') == 'PodScheduled' and condition.get('status') == 'False':
            return f"{condition.get('reason', 'Unscheduled')}: {condition.get('message', '')}".rstrip(': ')
    return ''


def wait_for_exit(runner, kube, name, timeout, clock, sleep):
    """Polls /out/exit in the k6 container and returns k6's exit code."""
    deadline = clock() + timeout
    while True:
        result = runner([*kube, 'get', 'pod', name, '-o', 'json'], capture_output=True, text=True, encoding='utf-8')
        # A failed status read is retried; the API server may be briefly unavailable.
        status = json.loads(result.stdout).get('status', {}) if not result.returncode else {}
        phase = status.get('phase')
        if phase == 'Running':
            done = runner([*kube, 'exec', name, '-c', 'k6', '--', 'cat', '/out/exit'],
                          capture_output=True, text=True, encoding='utf-8')
            text = done.stdout.strip() if not done.returncode else ''
            if text.isdigit():  # empty while the shell is still writing it
                return int(text)
        elif phase in ('Succeeded', 'Failed'):
            raise KubeRunError(f'pod {name} {phase.lower()} before its output was copied: {pod_problem(status)}')
        problem = pod_problem(status)
        if problem.split(':')[0] in FATAL:
            raise KubeRunError(f'pod {name} cannot start: {problem}')
        if clock() >= deadline:
            raise KubeRunError(f'k6 did not finish within {timeout} seconds; pod {phase}: {problem}'.rstrip(': '))
        sleep(POLL_S)


def fetch(runner, kube, name, out):
    """Copies exit and run.json into out and checks the copy is complete."""
    for file in ('exit', 'run.json'):
        # A relative target, because kubectl cp reads a Windows drive letter as a Pod name.
        capture(runner, [*kube, 'cp', '-c', 'k6', f'{name}:/out/{file}', file], cwd=out)
    size = int(capture(runner, [*kube, 'exec', name, '-c', 'k6', '--', 'wc', '-c', '/out/run.json']).split()[0])
    copied = (out/'run.json').stat().st_size
    if copied != size:
        raise KubeRunError(f'copied {copied} of the {size} bytes of run.json')


def save_logs(runner, kube, name, out):
    """k6's own output, saved for successful and failed runs alike; best effort."""
    try:
        result = runner([*kube, 'logs', name, '-c', 'k6'], capture_output=True, text=True, encoding='utf-8')
    except OSError:
        return
    if not result.returncode:
        (out/'k6.log').write_text(result.stdout, encoding='utf-8', newline='\n')


def error_text(error):
    if isinstance(error, KubeRunError):
        return str(error)
    return deploy.error_text(error)


def kube_run(args, runner, stamp, clock=time.monotonic, sleep=time.sleep):
    """Runs k6 in the cluster and returns the record. Failures are recorded in 'error'."""
    # Resolved, because kubectl cp runs in the output folder, where a relative path would not lead to it.
    kubeconfig = str(args.kubeconfig.resolve())
    kube = ['kubectl', '--kubeconfig', kubeconfig, '--context', args.kube_context, '-n', args.namespace]
    name, out = PREFIX + args.name, args.out_dir
    script = SCRIPT.read_text(encoding='utf-8')
    record = {'time': stamp.strftime('%Y-%m-%dT%H:%M:%SZ'), 'name': args.name, 'context': args.kube_context,
              'namespace': args.namespace, 'release': args.release, 'k6_image': K6_IMAGE,
              'script': {'path': deploy.repo_path(SCRIPT)[0],
                         'sha256': hashlib.sha256(script.encode('utf-8')).hexdigest()},
              'pod': name, 'node_selector': dict(WORKLOAD),
              'resources': {'cpu': args.k6_cpu, 'memory': args.k6_memory}, 'timeout_s': args.timeout,
              # The seed secret's value never leaves the cluster; only its source is recorded.
              'env': target_env(args) | {SEED_KEY: f'from Secret {SECRET}, key {SEED_KEY}'},
              'commit': deploy.head_commit(runner), 'gateway': None, 'gateway_after': None,
              'k6_exit_code': None, 'summarize_exit_code': None, 'outputs': [], 'cleanup': None, 'error': None}
    created, interrupted = [], False
    try:
        try:
            present = [file for file in OUTPUTS if (out/file).exists()]
            if present:
                raise KubeRunError(f'the output folder already has {", ".join(present)}; use another --name or --out-dir')
            out.mkdir(parents=True, exist_ok=True)
            record['gateway'] = gateway_state(runner, kube, args.release)
            create(runner, kube, config_map(args, script), created)
            create(runner, kube, pod(args), created)
            record['k6_exit_code'] = wait_for_exit(runner, kube, name, args.timeout, clock, sleep)
            fetch(runner, kube, name, out)
            record['outputs'] += ['exit', 'run.json']
            record['gateway_after'] = gateway_state(runner, kube, args.release)
        except (KubeRunError, OSError, ValueError, KeyError) as error:
            record['error'] = error_text(error)
        except KeyboardInterrupt:
            record['error'], interrupted = INTERRUPTED, True
        finally:
            try:
                if f'pod/{name}' in created and not interrupted:
                    save_logs(runner, kube, name, out)
                    if (out/'k6.log').exists():
                        record['outputs'].append('k6.log')
            finally:
                try:
                    record['cleanup'] = delete(runner, kube, created) or ('deleted' if created else 'nothing created')
                except KeyboardInterrupt:
                    # A second Ctrl-C: the objects may remain, so the record says how to delete them.
                    record['cleanup'] = f'interrupted; {leftovers(created)}'
                    raise
    except KeyboardInterrupt:
        record['error'], interrupted = INTERRUPTED, True
    if 'run.json' in record['outputs'] and not interrupted:
        try:
            result = runner([sys.executable, str(HERE/'summarize.py'), 'run.json'],
                            capture_output=True, text=True, encoding='utf-8', cwd=out)
            record['summarize_exit_code'] = result.returncode
            (out/'summary.md').write_text(result.stdout, encoding='utf-8', newline='\n')
            record['outputs'] += [file for file in ('run.results.json', 'summary.md') if (out/file).exists()]
            print(result.stdout, end='')
            if result.stderr:
                print(result.stderr, end='', file=sys.stderr)
        except KeyboardInterrupt:
            # Cleanup has already run and run.json is kept; summarize.py can be run again by hand.
            record['error'] = SUMMARIZE_INTERRUPTED
    return record


def main(argv=None, runner=subprocess.run, now=None, clock=time.monotonic, sleep=time.sleep):
    stamp = now or datetime.now(timezone.utc)
    args = parse_args(argv, stamp)
    record = kube_run(args, runner, stamp, clock, sleep)
    # Last guard before writing: replace any kubeconfig, home or repository path still in the record.
    paths = deploy.private_paths(args.kubeconfig)
    record['outputs'].append('run-record.json')
    path = args.out_dir/'run-record.json'
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(deploy.scrub(json.dumps(deploy.scrub_record(record, paths), indent=2), paths) + '\n',
                    encoding='utf-8')
    print(f'\nRun record: {path}')
    print(f"k6 exit code {record['k6_exit_code']}, summarize exit code {record['summarize_exit_code']}, "
          f"cleanup {record['cleanup']}")
    if record['error']:
        print(f"FAIL k6 run {record['name']}: {record['error']}", file=sys.stderr)
        return 130 if record['error'] in (INTERRUPTED, SUMMARIZE_INTERRUPTED) else 1
    if record['k6_exit_code']:
        return 1
    return record['summarize_exit_code']


if __name__ == '__main__': sys.exit(main())
