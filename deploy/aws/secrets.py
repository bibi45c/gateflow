"""Create or update the gateflow-bootstrap and gateflow-runtime Secrets on EKS.

Operator-run (bibi45c only). Reads the RDS endpoint from a file holding the output of
`terraform output -json`, the RDS master user and password from the Secrets Manager
secret that RDS manages (master_secret_arn), and the service passwords, internal
tokens, key pepper and seed secret from a local state file, so they stay the same
across days. Applies both Secrets with `kubectl apply -f -`: values travel only on
stdin, never in arguments, and nothing printed carries a value.

First run: when neither Secret exists in the namespace, missing state values are
generated and written to the state file first. When either Secret already exists, the
state file must exist, hold every value and match the cluster; otherwise the script
stops and asks for the original state file instead of generating new values, which
would no longer match the database roles, the seeded keys or the key pepper.

--refresh-master-only reads the existing gateflow-bootstrap, replaces only PGPASSWORD
with the current master password (RDS rotates it) and writes every other key back
unchanged; gateflow-runtime and the state file are not touched. Run it before every
`helm upgrade`, whose bootstrap hook logs in as the master user.

Requires aws (signed in) and kubectl on PATH. Never uses the default kubeconfig.
"""
import argparse
import base64
import json
from pathlib import Path
import subprocess
import sys
import urllib.parse

from common import (OperatorError, REGION, ROOT, Redactor, capture, kubectl, random_token,
                    require_ignored, terraform_outputs, write_private)

BOOTSTRAP = 'gateflow-bootstrap'
RUNTIME = 'gateflow-runtime'
# Values kept in the state file, the same on every day of the experiment.
STATE_KEYS = ('ACCESS_PASSWORD', 'METERING_PASSWORD', 'ACCESS_INTERNAL_TOKEN', 'METERING_INTERNAL_TOKEN',
              'ACCESS_KEY_PEPPER', 'ACCESS_SEED_SECRET')
BOOTSTRAP_KEYS = ('PGHOST', 'PGPORT', 'PGDATABASE', 'PGUSER', 'PGPASSWORD', 'PGSSLMODE',
                  'ACCESS_PASSWORD', 'METERING_PASSWORD')
RUNTIME_KEYS = ('ACCESS_DATABASE_URL', 'METERING_DATABASE_URL', 'ACCESS_INTERNAL_TOKEN',
                'METERING_INTERNAL_TOKEN', 'ACCESS_KEY_PEPPER', 'ACCESS_SEED_SECRET')
TERRAFORM_KEYS = ('database_address', 'database_port', 'database_name', 'master_secret_arn')
DEFAULT_STATE = ROOT/'.local'/'cloud-day'/'secrets-state.json'


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--kubeconfig', type=Path, required=True)
    parser.add_argument('--kube-context', required=True)
    parser.add_argument('--namespace', default='gateflow')
    parser.add_argument('--terraform-output', type=Path, required=True, metavar='JSON',
                        help='file holding the output of `terraform output -json`')
    parser.add_argument('--state', type=Path, default=DEFAULT_STATE,
                        help='local state file of the values kept across days; must be ignored by git '
                             '(default: .local/cloud-day/secrets-state.json)')
    parser.add_argument('--region', default=REGION)
    parser.add_argument('--refresh-master-only', action='store_true',
                        help='replace only PGPASSWORD in the existing gateflow-bootstrap')
    return parser.parse_args(argv)


def encode(values):
    return {key: base64.b64encode(value.encode()).decode() for key, value in values.items()}


def decode(data):
    return {key: base64.b64decode(value).decode() for key, value in (data or {}).items()}


def manifest(name, namespace, data, labels=None):
    metadata = {'name': name, 'namespace': namespace}
    if labels:
        metadata['labels'] = labels
    return {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': metadata, 'type': 'Opaque', 'data': data}


def database_url(role, password, host, port, database):
    return f'postgres://{role}:{password}@{host}:{port}/{database}?sslmode=require'


def master_credentials(runner, arn, region, redact):
    """Username and password of the RDS-managed master secret; neither is ever printed."""
    output = capture(runner, ['aws', 'secretsmanager', 'get-secret-value', '--secret-id', arn,
                              '--region', region, '--output', 'json'], redact)
    try:
        secret = json.loads(json.loads(output)['SecretString'])
        username, password = secret['username'], secret['password']
    except (ValueError, KeyError, TypeError) as error:
        raise OperatorError('the master secret has no JSON SecretString with username and password') from error
    redact.add(password)
    return username, password


def cluster_secret(runner, kube, name, redact):
    """The decoded data of an existing Secret, or None when it does not exist."""
    output = capture(runner, [*kube, 'get', 'secret', name, '--ignore-not-found', '-o', 'json'], redact)
    if not output.strip():
        return None
    document = json.loads(output)
    values = decode(document.get('data'))
    redact.add(*values.values())
    return {'values': values, 'labels': document.get('metadata', {}).get('labels')}


def read_state(path):
    if not path.exists():
        return None
    try:
        state = json.loads(path.read_text(encoding='utf-8'))
    except ValueError as error:
        raise OperatorError(f'state file {path} is not valid JSON') from error
    if not isinstance(state, dict) or not all(isinstance(value, str) for value in state.values()):
        raise OperatorError(f'state file {path} must be a JSON object of strings')
    return state


def cluster_values(existing):
    """The state values the existing Secrets carry, the role passwords included in the URLs."""
    found = {}
    bootstrap = (existing.get(BOOTSTRAP) or {}).get('values', {})
    runtime = (existing.get(RUNTIME) or {}).get('values', {})
    for key in STATE_KEYS:
        for values in (bootstrap, runtime):
            if key in values:
                found.setdefault(key, set()).add(values[key])
    for role, key in (('access', 'ACCESS_PASSWORD'), ('metering', 'METERING_PASSWORD')):
        url = runtime.get(f'{role.upper()}_DATABASE_URL')
        if url:
            password = urllib.parse.urlsplit(url).password
            found.setdefault(key, set()).add(urllib.parse.unquote(password or ''))
    return found


def settle_state(state, existing, path):
    """The state values to apply and the names of those generated now. Generates values only
    when no Secret exists yet; otherwise the state must be complete and match the cluster."""
    present = [name for name, secret in existing.items() if secret is not None]
    if present:
        names = ', '.join(present)
        if state is None:
            raise OperatorError(f'{names} already exist but the state file {path} is missing; restore the state '
                                'file that created them (no new values were generated)')
        missing = [key for key in STATE_KEYS if key not in state]
        if missing:
            raise OperatorError(f'{names} already exist but the state file lacks {", ".join(missing)}; restore the '
                                'state file that created them (no new values were generated)')
        conflicts = [key for key, values in cluster_values(existing).items() if values != {state[key]}]
        if conflicts:
            raise OperatorError(f'the state file does not match {names} for {", ".join(sorted(conflicts))}; restore '
                                'the state file that created them (nothing was changed)')
        return dict(state), []
    state = dict(state or {})
    generated = [key for key in STATE_KEYS if key not in state]
    for key in generated:
        state[key] = random_token()
    return state, generated


def bootstrap_values(outputs, username, password, state):
    return {'PGHOST': outputs['database_address'], 'PGPORT': str(outputs['database_port']),
            'PGDATABASE': outputs['database_name'], 'PGUSER': username, 'PGPASSWORD': password,
            'PGSSLMODE': 'require', 'ACCESS_PASSWORD': state['ACCESS_PASSWORD'],
            'METERING_PASSWORD': state['METERING_PASSWORD']}


def runtime_values(outputs, state):
    where = (outputs['database_address'], outputs['database_port'], outputs['database_name'])
    return {'ACCESS_DATABASE_URL': database_url('access', state['ACCESS_PASSWORD'], *where),
            'METERING_DATABASE_URL': database_url('metering', state['METERING_PASSWORD'], *where),
            **{key: state[key] for key in RUNTIME_KEYS[2:]}}


def apply(runner, kube, document, redact):
    """kubectl apply with the manifest on stdin, so no value reaches the command line."""
    capture(runner, [*kube, 'apply', '-f', '-'], redact, input=json.dumps(document))


def refresh_master(args, runner, kube, outputs, redact):
    existing = cluster_secret(runner, kube, BOOTSTRAP, redact)
    if existing is None:
        raise OperatorError(f'{BOOTSTRAP} does not exist in {args.namespace}; run without --refresh-master-only first')
    username, password = master_credentials(runner, outputs['master_secret_arn'], args.region, redact)
    if existing['values'].get('PGUSER') != username:
        raise OperatorError(f'{BOOTSTRAP} PGUSER differs from the master secret username; refusing to refresh')
    values = dict(existing['values'], PGPASSWORD=password)
    # Every existing key is written back, so a client-side apply removes none of them.
    apply(runner, kube, manifest(BOOTSTRAP, args.namespace, encode(values), existing['labels']), redact)
    kept = sorted(key for key in values if key != 'PGPASSWORD')
    print(f'{BOOTSTRAP}: PGPASSWORD refreshed from Secrets Manager; kept {", ".join(kept)}')
    print(f'{RUNTIME}: unchanged')


def create_or_update(args, runner, kube, outputs, redact):
    require_ignored(args.state, runner, 'state file')
    existing = {name: cluster_secret(runner, kube, name, redact) for name in (BOOTSTRAP, RUNTIME)}
    state, generated = settle_state(read_state(args.state), existing, args.state)
    redact.add(*state.values())
    username, password = master_credentials(runner, outputs['master_secret_arn'], args.region, redact)
    if generated:
        # Saved before anything is applied, so values in the cluster always exist locally too.
        write_private(args.state, json.dumps(state, indent=2, sort_keys=True) + '\n')
    for name, values in ((BOOTSTRAP, bootstrap_values(outputs, username, password, state)),
                         (RUNTIME, runtime_values(outputs, state))):
        redact.add(*values.values())
        labels = (existing[name] or {}).get('labels')
        apply(runner, kube, manifest(name, args.namespace, encode(values), labels), redact)
        print(f'{name}: applied {", ".join(values)}')
    if generated:
        print(f'state: generated {", ".join(generated)} in {args.state}')
    else:
        print(f'state: reused every value from {args.state}')
    print('master password refreshed from Secrets Manager')


def main(argv=None, runner=subprocess.run):
    args = parse_args(argv)
    redact = Redactor()
    kube = kubectl(args.kubeconfig, args.kube_context, args.namespace)
    try:
        outputs = terraform_outputs(args.terraform_output, TERRAFORM_KEYS)
        if args.refresh_master_only:
            refresh_master(args, runner, kube, outputs, redact)
        else:
            create_or_update(args, runner, kube, outputs, redact)
    except (OperatorError, OSError, ValueError, KeyError) as error:
        text = str(error) if isinstance(error, OperatorError) else f'{type(error).__name__}: {error}'
        print(f'FAIL {redact(text)}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
