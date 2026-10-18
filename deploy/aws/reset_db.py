"""Reset the experiment's accounting data on EKS and seed large wallets again.

Operator-run (bibi45c only), between load-test conditions and once after the first
Helm install (the chart's own metering seed creates small wallets, and seeding never
changes an existing balance; see loadtest/README.md). Stop every load generator
first. Without --yes it only prints the plan. With --yes it:

 1. records the gateway, access and metering replica counts;
 2. stops traffic: scales the gateway to 0, or, when a KEDA ScaledObject owns the
    gateway, annotates it with autoscaling.keda.sh/paused-replicas: "0"; then waits
    until no gateway Pod is left;
 3. starts a short-lived query Pod (postgres image, non-root, all capabilities
    dropped, no service account token, on a services node) whose DATABASE_URL comes
    from the metering URL in gateflow-runtime through secretKeyRef, so the URL never
    reaches this process;
 4. exports the row counts and a CSV of accounting.wallets, accounting.reservations
    and accounting.ledger_entries to the evidence directory, and checks the CSV row
    counts;
 5. truncates the three tables in one statement and transaction;
 6. runs `metering seed --balance N --tenants N` in a one-off Pod with the image,
    environment and security settings of the running metering Deployment;
 7. verifies tenants+1 wallets, tenant-00 at 0, every other wallet at N, nothing
    held, no reservations and no ledger entries;
 8. restores the gateway replica count or the previous KEDA annotation. When the
    ScaledObject had no paused-replicas annotation before, it first sets the
    annotation to the recorded gateway replica count, waits until that many gateway
    Pods are Ready, then removes the annotation and waits until the ScaledObject no
    longer reports Paused. Removing the annotation alone would leave the gateway at
    0: with no gateway Pod there is no gateway metric, the chart's triggers return
    an empty result by design, and the HPA never scales back up to minReplicas.

Whatever fails, the temporary Pods are deleted and the gateway is restored, and the
output says what state the database was left in. Nothing printed carries a secret.
The access schema is never touched. Requires kubectl on PATH; never uses the default
kubeconfig.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import time

from common import ROOT, OperatorError, capture, kubectl, random_token, redact_urls, require_ignored

SERVICES = ('gateway', 'access', 'metering')
TABLES = ('wallets', 'reservations', 'ledger_entries')
EXPORT_ORDER = {'wallets': 'tenant_id', 'reservations': 'created_at, reservation_id',
                'ledger_entries': 'created_at, ledger_entry_id'}
KEDA_PAUSE = 'autoscaling.keda.sh/paused-replicas'
DEFAULT_BALANCE = 1_000_000_000_000
QUERY_POD_LIFETIME = 1800  # seconds the query Pod sleeps before it ends by itself
POLL_SECONDS = 2
COUNTS_SQL = ("SELECT (SELECT count(*) FROM accounting.wallets), (SELECT count(*) FROM accounting.reservations), "
              "(SELECT count(*) FROM accounting.reservations WHERE status = 'reserved'), "
              "(SELECT coalesce(sum(held), 0) FROM accounting.wallets), "
              "(SELECT count(*) FROM accounting.ledger_entries)")
COUNT_NAMES = ('wallets', 'reservations', 'reservations_reserved', 'wallets_held', 'ledger_entries')
# All three tables in one TRUNCATE: the foreign keys among them are satisfied without
# CASCADE, so a table added later that references them makes the statement fail instead
# of being emptied silently. TRUNCATE does not fire ledger_entries' row-level
# append-only trigger, which guards UPDATE and DELETE only.
TRUNCATE_SQL = ("SET LOCAL lock_timeout = '30s'; "
                "TRUNCATE TABLE accounting.ledger_entries, accounting.reservations, accounting.wallets")


def verify_sql(balance):
    return ("SELECT count(*), count(*) FILTER (WHERE tenant_id = 'tenant-00' AND balance = 0), "
            f"count(*) FILTER (WHERE tenant_id <> 'tenant-00' AND balance = {int(balance)}), "
            "coalesce(sum(held), 0), coalesce(max(balance) FILTER (WHERE tenant_id = 'tenant-01'), -1), "
            "(SELECT count(*) FROM accounting.reservations WHERE status = 'reserved'), "
            "(SELECT count(*) FROM accounting.reservations), (SELECT count(*) FROM accounting.ledger_entries) "
            "FROM accounting.wallets")


def tenants(text):
    value = int(text)
    if not 0 <= value <= 99:
        raise argparse.ArgumentTypeError('tenants must be 0 to 99, as metering seed accepts')
    return value


def balance(text):
    value = int(text)
    if value < 0:
        raise argparse.ArgumentTypeError('balance must not be negative')
    return value


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--kubeconfig', type=Path, required=True)
    parser.add_argument('--kube-context', required=True)
    parser.add_argument('--namespace', default='gateflow')
    parser.add_argument('--release', default='gf')
    parser.add_argument('--balance', type=balance, default=DEFAULT_BALANCE,
                        help=f'balance of every seeded wallet but tenant-00, in micro-units (default: {DEFAULT_BALANCE})')
    parser.add_argument('--tenants', type=tenants, default=10, help='seed tenant-00 to tenant-NN (default: 10)')
    parser.add_argument('--evidence-dir', type=Path, default=ROOT/'.local'/'cloud-day'/'db-reset',
                        help='directory for the pre-reset export, one subdirectory per run; must be ignored by git '
                             '(default: .local/cloud-day/db-reset)')
    parser.add_argument('--db-secret', default='gateflow-runtime', help='Secret with the metering database URL')
    parser.add_argument('--db-secret-key', default='METERING_DATABASE_URL')
    parser.add_argument('--query-image', default='postgres:17')
    parser.add_argument('--timeout', type=int, default=300, help='seconds to wait for each Pod step (default: 300)')
    parser.add_argument('--yes', action='store_true', help='execute; without it only the plan is printed')
    return parser.parse_args(argv)


def plan(args):
    gateway = f'{args.release}-gateway'
    return [
        f'record the replicas of {", ".join(f"{args.release}-{name}" for name in SERVICES)} in {args.namespace}',
        f'stop traffic: scale {gateway} to 0, or annotate ScaledObject {gateway} with {KEDA_PAUSE}="0" if KEDA owns it;'
        ' wait until no gateway Pod is left',
        f'start a query Pod ({args.query_image}) reading {args.db_secret}/{args.db_secret_key} through secretKeyRef',
        f'export counts and CSV of accounting.{", accounting.".join(TABLES)} to {args.evidence_dir/"<UTC time>"}',
        'TRUNCATE accounting.ledger_entries, accounting.reservations, accounting.wallets in one transaction',
        f'run metering seed --balance {args.balance} --tenants {args.tenants} in a one-off Pod copied from '
        f'{args.release}-metering',
        f'verify {args.tenants + 1} wallets, tenant-00 at 0, the others at {args.balance}, nothing held, '
        'no reservations or ledger entries',
        f'delete the temporary Pods and restore {gateway}: its replicas, or, under KEDA, {KEDA_PAUSE} set to the'
        ' recorded replicas until that many gateway Pods are Ready, then the previous annotation',
    ]


def query_pod_spec(name, secret, key, image):
    container = {
        'name': 'psql', 'image': image, 'command': ['sleep', str(QUERY_POD_LIFETIME)],
        'env': [{'name': 'DATABASE_URL', 'valueFrom': {'secretKeyRef': {'name': secret, 'key': key}}},
                {'name': 'HOME', 'value': '/tmp'}],
        'securityContext': {'allowPrivilegeEscalation': False, 'capabilities': {'drop': ['ALL']}},
        'resources': {'requests': {'cpu': '10m', 'memory': '32Mi'}, 'limits': {'memory': '256Mi'}},
    }
    return {'apiVersion': 'v1', 'kind': 'Pod',
            'metadata': {'name': name, 'labels': {'app.kubernetes.io/name': 'gateflow-reset',
                                                  'app.kubernetes.io/component': 'db-query'}},
            'spec': {'containers': [container], 'restartPolicy': 'Never', 'automountServiceAccountToken': False,
                     'terminationGracePeriodSeconds': 1, 'nodeSelector': {'gateflow.io/workload': 'services'},
                     'securityContext': {'runAsNonRoot': True, 'runAsUser': 65532, 'runAsGroup': 65532,
                                         'seccompProfile': {'type': 'RuntimeDefault'}}}}


def seed_pod_spec(name, deployment, balance, tenants):
    """A one-off Pod running metering seed with the image, environment (Secret references
    only) and security settings of the metering Deployment."""
    pod = deployment['spec']['template']['spec']
    source = next((c for c in pod['containers'] if c.get('name') == 'metering'), pod['containers'][0])
    container = {'name': 'seed', 'image': source['image'],
                 'args': ['seed', '--balance', str(balance), '--tenants', str(tenants)]}
    for key in ('command', 'env', 'envFrom', 'securityContext', 'resources', 'imagePullPolicy'):
        if key in source:
            container[key] = source[key]
    spec = {'containers': [container], 'restartPolicy': 'Never', 'automountServiceAccountToken': False,
            'nodeSelector': pod.get('nodeSelector') or {'gateflow.io/workload': 'services'}}
    for key in ('securityContext', 'tolerations', 'imagePullSecrets'):
        if key in pod:
            spec[key] = pod[key]
    spec.setdefault('securityContext', {'runAsNonRoot': True})
    return {'apiVersion': 'v1', 'kind': 'Pod',
            'metadata': {'name': name, 'labels': {'app.kubernetes.io/name': 'gateflow-reset',
                                                  'app.kubernetes.io/component': 'seed'}},
            'spec': spec}


class Reset:
    """One reset run. Holds what the finally block must undo."""

    def __init__(self, args, runner, sleep, clock, now):
        self.args, self.runner, self.sleep, self.clock, self.now = args, runner, sleep, clock, now
        self.kube = kubectl(args.kubeconfig, args.kube_context, args.namespace)
        self.gateway = f'{args.release}-gateway'
        self.replicas, self.deployments = {}, {}
        self.scaled_object, self.previous_pause = None, None
        self.stopped = False
        self.query_pod = self.seed_pod = None
        self.phase = 'start'
        self.evidence = None

    def kubectl(self, *args, input=None):
        return capture(self.runner, [*self.kube, *args], input=input)

    def wait_until(self, what, done):
        deadline = self.clock() + self.args.timeout
        while not done():
            if self.clock() >= deadline:
                raise OperatorError(f'timed out after {self.args.timeout}s waiting for {what}')
            self.sleep(POLL_SECONDS)

    def record_replicas(self):
        names = [f'{self.args.release}-{name}' for name in SERVICES]
        document = json.loads(self.kubectl('get', 'deployment', *names, '-o', 'json'))
        for item in document.get('items', [document]):
            name = item['metadata']['name'][len(self.args.release) + 1:]
            self.deployments[name] = item
            self.replicas[name] = item['spec'].get('replicas', 1)
        missing = [name for name in SERVICES if name not in self.deployments]
        if missing:
            raise OperatorError(f'Deployments not found: {", ".join(missing)}')
        print('replicas: ' + ', '.join(f'{name} {self.replicas[name]}' for name in SERVICES))

    def find_scaled_object(self):
        kube = kubectl(self.args.kubeconfig, self.args.kube_context)
        resources = capture(self.runner, [*kube, 'api-resources', '--api-group=keda.sh', '-o', 'name'])
        if 'scaledobjects.keda.sh' not in resources.split():
            return
        output = self.kubectl('get', 'scaledobject.keda.sh', self.gateway, '--ignore-not-found', '-o', 'json')
        if output.strip():
            self.scaled_object = self.gateway
            self.previous_pause = json.loads(output)['metadata'].get('annotations', {}).get(KEDA_PAUSE)

    def gateway_pods(self):
        selector = f'app.kubernetes.io/instance={self.args.release},app.kubernetes.io/component=gateway'
        return json.loads(self.kubectl('get', 'pods', '-l', selector, '-o', 'json')).get('items', [])

    def stop_gateway(self):
        self.phase = 'stopping'
        self.stopped = True
        if self.scaled_object:
            self.kubectl('annotate', 'scaledobject.keda.sh', self.scaled_object, f'{KEDA_PAUSE}=0', '--overwrite')
            print(f'gateway: KEDA ScaledObject {self.scaled_object} paused with {KEDA_PAUSE}=0')
        else:
            self.kubectl('scale', 'deployment', self.gateway, '--replicas=0')
            print(f'gateway: scaled {self.gateway} from {self.replicas["gateway"]} to 0')
        self.wait_until('the gateway Pods to end', lambda: not self.gateway_pods())

    def start_query_pod(self):
        self.query_pod = f'gateflow-reset-db-{random_token(4)}'
        spec = query_pod_spec(self.query_pod, self.args.db_secret, self.args.db_secret_key, self.args.query_image)
        self.kubectl('create', '-f', '-', input=json.dumps(spec))
        self.kubectl('wait', '--for=condition=Ready', f'pod/{self.query_pod}', f'--timeout={self.args.timeout}s')

    def psql(self, *options, stdout=None):
        """psql in the query Pod; the URL stays inside the Pod, the SQL is an argument."""
        command = [*self.kube, 'exec', self.query_pod, '--', 'sh', '-c',
                   'exec psql -X -v ON_ERROR_STOP=1 "$@" "$DATABASE_URL"', 'reset', *options]
        if stdout is None:
            return capture(self.runner, command)
        try:
            result = self.runner(command, stdout=stdout, stderr=subprocess.PIPE)
        except OSError as error:
            raise OperatorError(f'kubectl exec could not start: {error.strerror}') from error
        if result.returncode:
            stderr = (result.stderr or b'').decode('utf-8', 'replace').strip().splitlines()[-3:]
            raise OperatorError(redact_urls(f'psql exited with {result.returncode}: {" ".join(stderr)}'))
        return None

    def counts(self, sql, names):
        values = self.psql('-tA', '-c', sql).strip().split('|')
        if len(values) != len(names):
            raise OperatorError(f'unexpected answer to the count query: {len(values)} columns')
        return dict(zip(names, (int(value) for value in values)))

    def export(self):
        self.phase = 'exporting'
        directory = self.args.evidence_dir/self.now.strftime('%Y%m%dT%H%M%SZ')
        directory.mkdir(parents=True, exist_ok=False)
        self.evidence = directory
        before = self.counts(COUNTS_SQL, COUNT_NAMES)
        files = {}
        for table in TABLES:
            path = directory/f'{table}.csv'
            sql = f'COPY (SELECT * FROM accounting.{table} ORDER BY {EXPORT_ORDER[table]}) TO STDOUT WITH (FORMAT csv, HEADER)'
            with path.open('wb') as stream:
                self.psql('-c', sql, stdout=stream)
            data = path.read_bytes()
            rows = max(len(data.splitlines()) - 1, 0)
            if rows != before[table]:
                raise OperatorError(f'{path.name} has {rows} rows, but the table had {before[table]}; not resetting')
            files[path.name] = {'rows': rows, 'sha256': hashlib.sha256(data).hexdigest()}
        record = {'time': self.now.strftime('%Y-%m-%dT%H:%M:%SZ'), 'context': self.args.kube_context,
                  'namespace': self.args.namespace, 'release': self.args.release, 'counts_before': before,
                  'files': files, 'seed': {'balance': self.args.balance, 'tenants': self.args.tenants},
                  'replicas_before': self.replicas, 'keda_paused': bool(self.scaled_object)}
        (directory/'counts.json').write_text(json.dumps(record, indent=2) + '\n', encoding='utf-8')
        print(f'evidence: {directory} (' + ', '.join(f'{key} {value}' for key, value in before.items()) + ')')

    def truncate(self):
        self.phase = 'truncating'
        self.psql('-1', '-c', TRUNCATE_SQL)
        self.phase = 'truncated'
        print('truncated accounting.ledger_entries, accounting.reservations, accounting.wallets')

    def seed(self):
        self.seed_pod = f'gateflow-reset-seed-{random_token(4)}'
        spec = seed_pod_spec(self.seed_pod, self.deployments['metering'], self.args.balance, self.args.tenants)
        self.kubectl('create', '-f', '-', input=json.dumps(spec))
        phase = {}

        def finished():
            phase['value'] = json.loads(self.kubectl('get', 'pod', self.seed_pod, '-o', 'json')).get(
                'status', {}).get('phase')
            return phase['value'] in ('Succeeded', 'Failed')
        self.wait_until('the seed Pod to finish', finished)
        logs = redact_urls(self.kubectl('logs', self.seed_pod).strip())
        if self.delete_pod(self.seed_pod):
            self.seed_pod = None
        if phase['value'] != 'Succeeded':
            raise OperatorError(f'metering seed failed: {" ".join(logs.splitlines()[-3:])}')
        self.phase = 'seeded'
        print(f'seed: {logs.splitlines()[-1] if logs else "no output"}')

    def verify(self):
        names = ('wallets', 'tenant_00', 'funded', 'held', 'tenant_01', 'reserved', 'reservations', 'ledger_entries')
        got = self.counts(verify_sql(self.args.balance), names)
        tenants, amount = self.args.tenants, self.args.balance
        want = {'wallets': tenants + 1, 'tenant_00': 1, 'funded': tenants, 'held': 0,
                'tenant_01': amount if tenants else -1, 'reserved': 0, 'reservations': 0, 'ledger_entries': 0}
        wrong = [f'{key} {got[key]} (want {want[key]})' for key in names if got[key] != want[key]]
        if wrong:
            raise OperatorError('verification failed: ' + ', '.join(wrong))
        self.phase = 'verified'
        print(f'verified: {got["wallets"]} wallets, tenant-01 balance {got["tenant_01"]}, nothing held, '
              'no reservations, no ledger entries')

    def execute(self):
        require_ignored(self.args.evidence_dir, self.runner, 'evidence')
        self.record_replicas()
        self.find_scaled_object()
        self.stop_gateway()
        self.start_query_pod()
        self.export()
        self.truncate()
        self.seed()
        self.verify()

    def delete_pod(self, name):
        try:
            self.kubectl('delete', 'pod', name, '--wait=false', '--ignore-not-found')
        except OperatorError as error:
            command = ' '.join(['kubectl', '--context', self.args.kube_context, '-n', self.args.namespace,
                                'delete', 'pod', name])
            print(f'WARNING could not delete Pod {name}: {error}; delete it with: {command}', file=sys.stderr)
            return False
        return True

    def ready_gateways(self):
        item = json.loads(self.kubectl('get', 'deployment', self.gateway, '-o', 'json'))
        return item.get('status', {}).get('readyReplicas') or 0

    def keda_paused(self):
        item = json.loads(self.kubectl('get', 'scaledobject.keda.sh', self.scaled_object, '-o', 'json'))
        if KEDA_PAUSE in item.get('metadata', {}).get('annotations', {}):
            return True
        return any(condition.get('type') == 'Paused' and condition.get('status') == 'True'
                   for condition in item.get('status', {}).get('conditions', []))

    def resume_keda(self):
        """KEDA cannot bring the gateway back from 0 itself: with no gateway Pod the triggers
        return an empty result, so the HPA keeps 0. Let KEDA hold the recorded count until
        those Pods are Ready, so metrics flow again before autoscaling takes over."""
        target = self.replicas['gateway']
        if target:
            self.kubectl('annotate', 'scaledobject.keda.sh', self.scaled_object, f'{KEDA_PAUSE}={target}',
                         '--overwrite')
            print(f'gateway: KEDA holding {self.gateway} at {target} ({KEDA_PAUSE}={target})')
            self.wait_until(f'{target} ready gateway Pods while {KEDA_PAUSE}={target}',
                            lambda: self.ready_gateways() == target)
        self.kubectl('annotate', 'scaledobject.keda.sh', self.scaled_object, f'{KEDA_PAUSE}-')
        self.wait_until(f'ScaledObject {self.scaled_object} to leave the paused state',
                        lambda: not self.keda_paused())
        print(f'gateway: KEDA resumed ({KEDA_PAUSE} removed) with {target} ready gateway Pods')

    def restore_gateway(self):
        if self.scaled_object and self.previous_pause is None:
            self.resume_keda()
        elif self.scaled_object:
            self.kubectl('annotate', 'scaledobject.keda.sh', self.scaled_object,
                         f'{KEDA_PAUSE}={self.previous_pause}', '--overwrite')
            print(f'gateway: KEDA annotation {KEDA_PAUSE} restored to its previous value')
        else:
            target = self.replicas['gateway']
            self.kubectl('scale', 'deployment', self.gateway, f'--replicas={target}')
            print(f'gateway: scaled {self.gateway} back to {target}')
            if target:
                self.wait_until(f'{target} ready gateway Pods', lambda: self.ready_gateways() >= target)

    def advice(self):
        where = f' The pre-reset export is in {self.evidence}.' if self.evidence else ''
        if self.phase in ('start', 'stopping', 'exporting'):
            return 'The database was not changed.' + where
        if self.phase == 'truncating':
            return ('The truncate failed; its transaction was rolled back unless psql lost the connection. '
                    'Check accounting.wallets before any load run, then run reset_db.py --yes again.' + where)
        if self.phase == 'truncated':
            return ('The accounting tables are empty and no wallet exists. Run reset_db.py --yes again '
                    'before any load run.' + where)
        return ('Wallets were seeded but did not verify. Do not start a load run; inspect accounting.wallets '
                'and run reset_db.py --yes again.' + where)

    def restore_command(self):
        base = f'kubectl --context {self.args.kube_context} -n {self.args.namespace}'
        if self.scaled_object:
            annotate = f'{base} annotate scaledobject.keda.sh {self.scaled_object}'
            target = self.replicas['gateway']
            if self.previous_pause is not None:
                return f'{annotate} {KEDA_PAUSE}={self.previous_pause} --overwrite'
            if not target:
                return f'{annotate} {KEDA_PAUSE}-'
            return (f'{annotate} {KEDA_PAUSE}={target} --overwrite; wait until {target} gateway Pods are Ready; '
                    f'then {annotate} {KEDA_PAUSE}-')
        return f'{base} scale deployment {self.gateway} --replicas={self.replicas["gateway"]}'

    def cleanup(self):
        ok = True
        for name in (self.seed_pod, self.query_pod):
            if name:
                ok = self.delete_pod(name) and ok
        if self.stopped:
            try:
                self.restore_gateway()
            except OperatorError as error:
                print(f'FAIL restoring the gateway: {error}; restore it with: {self.restore_command()}',
                      file=sys.stderr)
                ok = False
        return ok


def main(argv=None, runner=subprocess.run, sleep=time.sleep, clock=time.monotonic, now=None):
    args = parse_args(argv)
    if not args.yes:
        print('Plan (nothing was changed; pass --yes to execute):')
        for number, step in enumerate(plan(args), 1):
            print(f' {number}. {step}')
        return 0
    reset = Reset(args, runner, sleep, clock, now or datetime.now(timezone.utc))
    code = 0
    try:
        reset.execute()
    except (OperatorError, OSError, ValueError, KeyError) as error:
        text = str(error) if isinstance(error, OperatorError) else f'{type(error).__name__}: {error}'
        print(f'FAIL {redact_urls(text)}', file=sys.stderr)
        print(f'Recovery: {reset.advice()}', file=sys.stderr)
        code = 1
    finally:
        if not reset.cleanup():
            code = 1
    if code == 0:
        print('reset complete: ' + ', '.join(f'{name} {reset.replicas[name]}' for name in SERVICES) + ' replicas before')
    return code


if __name__ == '__main__':
    sys.exit(main())
