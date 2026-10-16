"""Move one FlowGate service of an existing Helm release to an image digest.

Operator-run. Reads the release's current values, pins every other service to the
image it already runs, upgrades with Helm, checks that only the chosen service's
image changed, runs the smoke suite and writes a JSON deployment record. Roll back
by deploying the previous digest again, for example with --rollback-from RECORD.

Requires helm and kubectl on PATH. Never uses the default kubeconfig.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
SERVICES = ('gateway', 'access', 'metering', 'mock-upstream')
DIGEST = re.compile(r'sha256:[0-9a-f]{64}')


class DeployError(Exception):
    pass


def digest(text):
    if not DIGEST.fullmatch(text):
        raise argparse.ArgumentTypeError('digest must be sha256: followed by 64 lowercase hex digits')
    return text


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--kubeconfig', type=Path, required=True)
    parser.add_argument('--kube-context', required=True)
    parser.add_argument('--namespace', default='gateflow')
    parser.add_argument('--release', default='gf')
    parser.add_argument('--chart', type=Path, default=HERE/'gateflow')
    parser.add_argument('--service', choices=SERVICES, help='service to move (taken from the record with --rollback-from)')
    target = parser.add_mutually_exclusive_group(required=True)
    target.add_argument('--digest', type=digest, help='new image digest, sha256:<64 hex>')
    target.add_argument('--rollback-from', type=Path, metavar='RECORD',
                        help='deployment record whose previous_digest is deployed again')
    parser.add_argument('-f', '--values', type=Path, action='append', default=[],
                        help='extra values file, repeatable (for example deploy/helm/values-aws.yaml)')
    parser.add_argument('--timeout', default='10m', help='helm upgrade timeout (default: 10m)')
    parser.add_argument('--record', type=Path, help='record path (default: .local/deployments/<UTC>-<service>.json)')
    smoke = parser.add_argument_group('smoke')
    smoke.add_argument('--smoke-host', default='127.0.0.1', help='ingress host for test/e2e/smoke.py')
    smoke.add_argument('--smoke-port', type=int, help='ingress port for test/e2e/smoke.py')
    smoke.add_argument('--smoke-postgres-pod', help='PostgreSQL Pod for the smoke ledger checks')
    smoke.add_argument('--skip-smoke', action='store_true',
                       help='do not run smoke; the record says skipped and is not success evidence')
    args = parser.parse_args(argv)
    if args.skip_smoke == (args.smoke_port is not None):
        parser.error('pass exactly one of --smoke-port and --skip-smoke')
    if args.digest and not args.service:
        parser.error('--digest requires --service')
    return args


def capture(runner, command):
    result = runner(command, capture_output=True, text=True, encoding='utf-8')
    if result.returncode:
        raise DeployError(f"{' '.join(command)} failed: {result.stderr.strip()}")
    return result.stdout


def head_commit(runner):
    try:
        result = runner(['git', 'rev-parse', 'HEAD'], capture_output=True, text=True, encoding='utf-8', cwd=ROOT)
    except OSError:
        return None
    if result.returncode:
        return None
    return result.stdout.strip() or None


def deployment_images(runner, kube, release):
    """Image of each service's Deployment, read from the cluster."""
    names = [f'{release}-{name}' for name in SERVICES]
    document = json.loads(capture(runner, [*kube, 'get', 'deployment', *names, '-o', 'json']))
    items = document.get('items', [document])
    images = {}
    for item in items:
        name = item['metadata']['name'][len(release)+1:]
        containers = item['spec']['template']['spec']['containers']
        images[name] = next((c for c in containers if c['name'] == name), containers[0])['image']
    if set(images) != set(SERVICES):
        raise DeployError(f'expected Deployments for {", ".join(SERVICES)}, found {", ".join(sorted(images))}')
    return images


def helm_revision(runner, helm, release, namespace):
    return json.loads(capture(runner, [*helm, 'status', release, '-n', namespace, '-o', 'json']))['version']


def current_digest(values, service):
    return values['services'][service].get('digest') or None


def image_pins(values, service, new_digest):
    """Keep imageTag and every other digest as deployed; change only the target digest."""
    services = {name: {'digest': current_digest(values, name) or ''} for name in SERVICES}
    services[service]['digest'] = new_digest or ''
    return {'imageTag': values['imageTag'], 'services': services}


def expected_image(values, service, new_digest):
    image = values['services'][service]['image']
    return f'{image}@{new_digest}' if new_digest else f"{image}:{values['imageTag']}"


def rollback_target(path, args):
    """Service and digest to restore from an earlier record."""
    record = json.loads(path.read_text(encoding='utf-8'))
    for key, expected in (('release', args.release), ('namespace', args.namespace), ('context', args.kube_context)):
        if record.get(key) != expected:
            raise DeployError(f'record {key} is {record.get(key)!r}, not {expected!r}')
    service = record.get('service')
    if service not in SERVICES or (args.service and args.service != service):
        raise DeployError(f'record service {service!r} does not match --service {args.service!r}')
    # Pins are written only after the deployed values were read, so previous_digest is real.
    if 'previous_digest' not in record or not record.get('image_pins'):
        raise DeployError('record never read the deployed values; it has no usable previous_digest')
    previous = record['previous_digest']
    if previous is not None and not DIGEST.fullmatch(previous):
        raise DeployError(f'record previous_digest {previous!r} is not a sha256 digest')
    # A null previous digest means the service ran imageTag; restoring it clears the digest.
    return service, previous, record.get('new_digest')


def default_record(service, stamp):
    return ROOT/'.local'/'deployments'/f"{stamp.strftime('%Y%m%dT%H%M%SZ')}-{service}.json"


def values_file(path):
    return {'path': str(path), 'sha256': hashlib.sha256(path.read_bytes()).hexdigest()}


def deploy(args, runner, stamp):
    """Runs the deployment and returns the record. Failures are recorded in 'error'."""
    helm = ['helm', '--kubeconfig', str(args.kubeconfig), '--kube-context', args.kube_context]
    kube = ['kubectl', '--kubeconfig', str(args.kubeconfig), '--context', args.kube_context, '-n', args.namespace]
    record = {'time': stamp.strftime('%Y-%m-%dT%H:%M:%SZ'),
              'operation': 'rollback' if args.rollback_from else 'deploy',
              'commit': head_commit(runner), 'release': args.release, 'namespace': args.namespace,
              'context': args.kube_context, 'service': args.service, 'previous_digest': None, 'new_digest': None,
              'rollback_from': str(args.rollback_from) if args.rollback_from else None,
              'chart': str(args.chart), 'values_files': [], 'image_pins': None,
              'images_before': None, 'images_after': None, 'changed_services': None,
              'helm_revision_before': None, 'helm_revision': None,
              'smoke': {'result': 'not-run', 'exit_code': None, 'host': args.smoke_host, 'port': args.smoke_port},
              'verified': False, 'error': None}
    try:
        record['values_files'] = [values_file(path) for path in args.values]
        service, new_digest = args.service, args.digest
        if args.rollback_from:
            service, new_digest, rolled_back = rollback_target(args.rollback_from, args)
            record['service'] = service
        values = json.loads(capture(runner, [*helm, 'get', 'values', args.release, '-n', args.namespace, '--all', '-o', 'json']))
        previous = current_digest(values, service)
        if args.rollback_from and previous != rolled_back:
            raise DeployError(f'{service} now runs digest {previous}, not the record new_digest {rolled_back}; '
                              'refusing to roll back over a later deployment')
        record['previous_digest'], record['new_digest'] = previous, new_digest
        record['image_pins'] = image_pins(values, service, new_digest)
        record['helm_revision_before'] = helm_revision(runner, helm, args.release, args.namespace)
        record['images_before'] = deployment_images(runner, kube, args.release)
        with tempfile.TemporaryDirectory() as directory:
            # The deployed values are the base, so values not re-supplied keep their deployed setting.
            base, pins = Path(directory)/'deployed-values.json', Path(directory)/'image-pins.json'
            base.write_text(json.dumps(values), encoding='utf-8')
            pins.write_text(json.dumps(record['image_pins']), encoding='utf-8')
            files = [option for path in [base, *args.values, pins] for option in ('-f', str(path))]
            upgrade = runner([*helm, 'upgrade', args.release, str(args.chart), '-n', args.namespace, *files,
                              '--wait', '--wait-for-jobs', '--timeout', args.timeout])
        try:
            record['helm_revision'] = helm_revision(runner, helm, args.release, args.namespace)
            after = record['images_after'] = deployment_images(runner, kube, args.release)
            record['changed_services'] = [name for name in SERVICES if record['images_before'][name] != after[name]]
        except DeployError:
            # Keep the upgrade's own failure as the recorded error.
            if not upgrade.returncode:
                raise
        if upgrade.returncode:
            raise DeployError(f'helm upgrade exited with {upgrade.returncode}')
        others = [name for name in record['changed_services'] if name != service]
        if others:
            raise DeployError(f'images of services other than {service} changed: {", ".join(others)}')
        want = expected_image(values, service, new_digest)
        if after[service] != want:
            raise DeployError(f'{service} runs {after[service]}, expected {want}')
        if args.skip_smoke:
            record['smoke']['result'] = 'skipped'
            return record
        command = [sys.executable, str(ROOT/'test/e2e/smoke.py'), '--host', args.smoke_host, '--port', str(args.smoke_port)]
        if args.smoke_postgres_pod:
            command += ['--kube-context', args.kube_context, '--namespace', args.namespace,
                        '--postgres-pod', args.smoke_postgres_pod]
        # The smoke suite's kubectl calls pass --context but read the kubeconfig from the environment.
        smoke = runner(command, env=os.environ | {'KUBECONFIG': str(args.kubeconfig)})
        record['smoke'].update(result='passed' if smoke.returncode == 0 else 'failed', exit_code=smoke.returncode)
        if smoke.returncode:
            raise DeployError(f'smoke exited with {smoke.returncode}')
        record['verified'] = True
    except (DeployError, OSError, ValueError, KeyError) as error:
        record['error'] = f'{type(error).__name__}: {error}' if not isinstance(error, DeployError) else str(error)
    return record


def main(argv=None, runner=subprocess.run, now=None):
    args = parse_args(argv)
    stamp = now or datetime.now(timezone.utc)
    record = deploy(args, runner, stamp)
    path = args.record or default_record(record['service'] or 'unknown', stamp)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(record, indent=2) + '\n', encoding='utf-8')
    print(f'Deployment record: {path}')
    if record['error']:
        print(f"FAIL {record['operation']} {record['service']}: {record['error']}", file=sys.stderr)
        return 1
    if record['smoke']['result'] == 'skipped':
        print('WARNING smoke skipped: this record is not evidence of a working deployment', file=sys.stderr)
    print(f"{record['operation']} {record['service']}: {record['previous_digest']} -> {record['new_digest']}, "
          f"helm revision {record['helm_revision']}, smoke {record['smoke']['result']}")
    return 0


if __name__ == '__main__': sys.exit(main())
