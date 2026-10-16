"""Install the pinned Grafana dashboard on an explicitly selected FlowGate cluster.

The application release must already have dashboard.enabled=true, and the
Prometheus release from monitoring/prometheus-values.yaml (with its
kube-state-metrics) must be installed. Prometheus is checked, never changed.
Credentials never enter tracked files or stdout.
"""
import argparse
import base64
import json
from pathlib import Path
import secrets
import subprocess

HERE = Path(__file__).resolve().parent
MONITORING = HERE/'monitoring'
ADMIN_SECRET = 'gateflow-grafana-admin'
ADMIN_KEYS = ('admin-user', 'admin-password')


def run(command, **kwargs):
    return subprocess.run(command, check=True, text=True, encoding='utf-8', **kwargs)


def ensure_secret(kube, namespace):
    existing = run([*kube, '-n', namespace, 'get', 'secret', ADMIN_SECRET,
                    '--ignore-not-found', '-o', 'json'], capture_output=True).stdout.strip()
    if existing:
        missing = [k for k in ADMIN_KEYS if not (json.loads(existing).get('data') or {}).get(k)]
        if missing:
            raise ValueError('Secret '+ADMIN_SECRET+' lacks '+', '.join(missing)+'; add the keys or delete '
                             'the Secret so a new random one is created, then rerun')
        return
    values = {'admin-user': 'admin', 'admin-password': secrets.token_urlsafe(32)}
    document = {'apiVersion': 'v1', 'kind': 'Secret',
                'metadata': {'name': ADMIN_SECRET, 'namespace': namespace},
                'type': 'Opaque', 'data': {k: base64.b64encode(v.encode()).decode() for k, v in values.items()}}
    run([*kube, 'create', '-f', '-'], input=json.dumps(document), stdout=subprocess.DEVNULL)


def check_prometheus(helm, kube):
    """Requires the deployed Prometheus release with an available server and kube-state-metrics."""
    prometheus = json.loads((MONITORING/'versions.json').read_text(encoding='utf-8'))['prometheus']
    if prometheus['release'] != 'prometheus' or prometheus['namespace'] != 'monitoring':
        raise ValueError('Review the Grafana datasource URL before changing monitoring release names')
    status = json.loads(run([*helm, 'status', prometheus['release'], '-n', prometheus['namespace'], '-o', 'json'],
                            capture_output=True).stdout)
    if status['info']['status'] != 'deployed':
        raise ValueError('Prometheus release is '+status['info']['status']+'; install it from '
                         'monitoring/prometheus-values.yaml first')
    for name in ('-server', '-kube-state-metrics'):
        deployment = json.loads(run([*kube, '-n', prometheus['namespace'], 'get', 'deployment',
                                     prometheus['release']+name, '-o', 'json'], capture_output=True).stdout)
        if not deployment.get('status', {}).get('availableReplicas'):
            raise ValueError('Deployment '+prometheus['release']+name+' has no available replica')


def install(helm, kube, namespace, release):
    configmap = run([*kube, '-n', namespace, 'get', 'configmap', release+'-dashboard',
                     '--ignore-not-found', '-o', 'json'], capture_output=True).stdout.strip()
    if not configmap:
        raise ValueError('ConfigMap '+release+'-dashboard is missing; upgrade the application chart '
                         'with dashboard.enabled=true first')
    actual = json.loads(configmap)['data']['gateflow.json']
    expected = (HERE/'gateflow/files/gateflow-dashboard.json').read_text(encoding='utf-8')
    if json.loads(actual) != json.loads(expected):
        raise ValueError('Installed dashboard differs from this checkout; upgrade the application chart first')
    check_prometheus(helm, kube)
    ensure_secret(kube, namespace)
    grafana = json.loads((MONITORING/'grafana-version.json').read_text(encoding='utf-8'))
    run([*helm, 'upgrade', '--install', 'grafana', grafana['chart'], '--repo', grafana['repository'],
         '--version', grafana['version'], '-n', namespace,
         '-f', str(MONITORING/'grafana-values.yaml'), '--set-string',
         'dashboardsConfigMaps.gateflow='+release+'-dashboard', '--wait', '--timeout', '5m'])
    print('Grafana installed. Use a localhost port-forward to service/grafana port 80.')
    print('Read admin-user/admin-password from the '+ADMIN_SECRET+' Secret locally; never post them.')


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--kubeconfig', type=Path, required=True)
    parser.add_argument('--kube-context', required=True)
    parser.add_argument('--namespace', default='gateflow')
    parser.add_argument('--release', default='gf')
    args = parser.parse_args(argv)
    kube = ['kubectl', '--kubeconfig', str(args.kubeconfig), '--context', args.kube_context]
    helm = ['helm', '--kubeconfig', str(args.kubeconfig), '--kube-context', args.kube_context]
    install(helm, kube, args.namespace, args.release)


if __name__ == '__main__':
    main()
