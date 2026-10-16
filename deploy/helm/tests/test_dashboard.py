"""Check dashboard provisioning and the semantics that could misreport experiments."""
import importlib.util
import json
from pathlib import Path
import re
import subprocess
from types import SimpleNamespace
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parents[1]
MONITORING = HERE/'monitoring'
OBSERVABILITY = HERE.parents[1]/'pkg/observability/observability.go'
spec = importlib.util.spec_from_file_location('dashboard', HERE/'dashboard.py')
dashboard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(dashboard)

KUBE = ['kubectl', '--context', 'kind-gateflow74']
HELM = ['helm', '--kube-context', 'kind-gateflow74']
SECRET = json.dumps({'data': {'admin-user': 'YWRtaW4=', 'admin-password': 'c2VjcmV0'}})


def uncommented(path):
    return [l for l in path.read_text(encoding='utf-8').splitlines() if l.strip() and not l.lstrip().startswith('#')]


class FakeCluster:
    """Answers dashboard.run like a cluster with Prometheus and the dashboard ConfigMap installed."""
    def __init__(self, prometheus='deployed', available=1, secret=SECRET):
        self.calls = []
        self.answers = {'configmap': json.dumps({'data': {'gateflow.json': (
                            HERE/'gateflow/files/gateflow-dashboard.json').read_text(encoding='utf-8')}}),
                        'status': json.dumps({'info': {'status': prometheus}}),
                        'deployment': json.dumps({'status': {'availableReplicas': available}}),
                        'secret': secret}

    def __call__(self, command, **kwargs):
        self.calls.append(command)
        word = next((w for w in ('configmap', 'status', 'deployment', 'secret') if w in command), None)
        return SimpleNamespace(stdout=self.answers.get(word, ''))

    def helm_upgrades(self):
        return [c for c in self.calls if c[0] == 'helm' and 'upgrade' in c]


class DashboardTests(unittest.TestCase):
    def setUp(self):
        self.document = json.loads((HERE/'gateflow/files/gateflow-dashboard.json').read_text(encoding='utf-8'))
        self.expressions = [t['expr'] for p in self.document['panels'] for t in p.get('targets', [])]
        self.panels = {p['id']: p for p in self.document['panels']}

    def test_render_is_opt_in_and_preserves_dashboard_variables(self):
        def render(*args):
            output = subprocess.run(['helm', 'template', 'gf', str(HERE/'gateflow'), '--kube-version', '1.35.0', *args],
                                    check=True, capture_output=True, text=True, encoding='utf-8').stdout
            return [json.loads('\n'.join(l for l in d.splitlines() if not l.startswith('#')))
                    for d in output.split('---') if d.strip()]
        self.assertFalse(any(d['metadata']['name'] == 'gf-dashboard' for d in render()))
        documents = render('--set', 'dashboard.enabled=true')
        configmap = next(d for d in documents if d['metadata']['name'] == 'gf-dashboard')
        self.assertEqual(json.loads(configmap['data']['gateflow.json']), self.document)
        self.assertIn('$__rate_interval', configmap['data']['gateflow.json'])
        self.assertIn('${run}', configmap['data']['gateflow.json'])
        self.assertFalse(any(d['kind'] == 'Ingress' and 'grafana' in json.dumps(d) for d in documents))
        with self.assertRaises(subprocess.CalledProcessError):
            render('--set', 'dashboard.enabled=true', '--set', 'dashboard.enable=true')

    def test_missing_data_never_becomes_idle_success(self):
        for expr in self.expressions:
            self.assertNotIn('or vector(0)', expr)
            self.assertNotIn('clamp_min', expr)
            self.assertNotRegex(expr, r'count\(up\{[^}]*\} == 1\)')
        percentile = [e for e in self.expressions if e.startswith('histogram_quantile')]
        self.assertEqual(len(percentile), 4)
        self.assertTrue(all('sum by (le) (rate(' in e for e in percentile))
        self.assertTrue(any('kube_deployment_spec_replicas' in e for e in self.expressions))
        self.assertTrue(any('kube_deployment_status_replicas_available' in e for e in self.expressions))
        self.assertTrue(any(e.startswith('sum(up{job="$job"') for e in self.expressions))

    def test_replica_queries_match_the_prometheus_kube_state_metrics_job(self):
        replicas = [e for e in self.expressions if e.startswith('kube_deployment_')]
        self.assertEqual(len(replicas), 2)
        for expr in replicas:
            self.assertIn('namespace="$namespace"', expr)
            self.assertIn('deployment="$release-gateway"', expr)
            job = re.search(r'job="([^"]*)"', expr)
            self.assertTrue(job is None or job.group(1) == 'kube-state-metrics')
        values = (MONITORING/'prometheus-values.yaml').read_text(encoding='utf-8')
        self.assertRegex(values, r'- job_name: kube-state-metrics\n\s+honor_labels: true')

    def test_first_output_ceiling_is_explicit(self):
        buckets = re.search(r'latencyBuckets\s*=\s*\[\]float64\{([^}]*)\}', OBSERVABILITY.read_text(encoding='utf-8'))
        self.assertEqual(float(buckets.group(1).split(',')[-1]), 10)
        first, duration = self.panels[6], self.panels[7]
        self.assertIn('values at 10 s mean ≥ 10 s (bucket ceiling)', first['description'])
        quantile = first['targets'][1]['expr']
        selector = re.search(r'_bucket\{([^}]*)\}', quantile).group(1)
        above = next(t for t in first['targets'] if '_count' in t['expr'])
        self.assertEqual(above['expr'],
                         '1 - sum(rate(gateflow_gateway_first_output_seconds_bucket{'+selector+',le="10"}'
                         '[$__rate_interval])) / sum(rate(gateflow_gateway_first_output_seconds_count{'+selector+'}'
                         '[$__rate_interval]))')
        override = next(o for o in first['fieldConfig']['overrides'] if o['matcher']['options'] == above['refId'])
        self.assertIn({'id': 'unit', 'value': 'percentunit'}, override['properties'])
        self.assertIn('Above 10 s the buckets are coarse (10, 30, 60, 90, 120 s)', duration['description'])
        self.assertRegex(OBSERVABILITY.read_text(encoding='utf-8'), r'latencyBuckets\.\.\.\), 30, 60, 90, 120\)')

    def test_every_panel_uses_the_provisioned_datasource(self):
        uids = re.findall(r'^\s+uid:\s*(\S+)\s*$', (MONITORING/'grafana-values.yaml').read_text(encoding='utf-8'), re.M)
        self.assertEqual(len(uids), 1)
        for panel in self.document['panels']:
            if panel['type'] == 'text':
                continue
            self.assertEqual(panel['datasource'], {'type': 'prometheus', 'uid': uids[0]})
            for target in panel['targets']:
                self.assertEqual(target['datasource'], panel['datasource'])

    def test_every_gateflow_metric_exists(self):
        source = OBSERVABILITY.read_text(encoding='utf-8')
        names = {re.sub(r'_(bucket|count|sum)$', '', n)
                 for e in self.expressions for n in re.findall(r'\bgateflow_[a-z0-9_]+', e)}
        self.assertGreaterEqual(len(names), 6)
        for name in names:
            self.assertIn('"'+name+'"', source)

    def test_dashboard_distinguishes_formal_and_observed_completion(self):
        text = json.dumps(self.document)
        self.assertIn('not the proposal completion SLO', text)
        self.assertIn('not metric labels or data filters', text)
        self.assertIn('not paired client added latency', text)
        ids = [p['id'] for p in self.document['panels']]
        self.assertEqual(len(ids), len(set(ids)))

    def test_grafana_has_no_kubernetes_api_access(self):
        values = (MONITORING/'grafana-values.yaml').read_text(encoding='utf-8')
        self.assertRegex(values, r'(?m)^rbac:\n  create: false$')
        self.assertRegex(values, r'(?m)^serviceAccount:\n  automountServiceAccountToken: false$')

    def test_prometheus_is_configured_only_by_its_own_values(self):
        overlay = MONITORING/'dashboard-prometheus-values.yaml'
        self.assertFalse(overlay.exists() and uncommented(overlay))
        for path in MONITORING.glob('*.yaml'):
            if path.name != 'prometheus-values.yaml':
                self.assertFalse(any('kube-state-metrics' in l for l in uncommented(path)), path.name)
        self.assertNotIn('dashboard-prometheus-values', (HERE/'dashboard.py').read_text(encoding='utf-8'))

    def test_install_checks_prometheus_and_upgrades_only_grafana(self):
        cluster = FakeCluster()
        with patch.object(dashboard, 'run', cluster), patch('builtins.print'):
            dashboard.install(HELM, KUBE, 'gateflow', 'gf')
        upgrades = cluster.helm_upgrades()
        self.assertEqual(len(upgrades), 1)
        self.assertEqual(upgrades[0][upgrades[0].index('--install')+1], 'grafana')
        self.assertIn([*HELM, 'status', 'prometheus', '-n', 'monitoring', '-o', 'json'], cluster.calls)
        checked = {c[c.index('deployment')+1] for c in cluster.calls if 'deployment' in c}
        self.assertEqual(checked, {'prometheus-server', 'prometheus-kube-state-metrics'})

    def test_unavailable_prometheus_stops_before_grafana(self):
        for cluster in (FakeCluster(prometheus='failed'), FakeCluster(available=0)):
            with patch.object(dashboard, 'run', cluster), self.assertRaises(ValueError):
                dashboard.install(HELM, KUBE, 'gateflow', 'gf')
            self.assertEqual(cluster.helm_upgrades(), [])

    def test_missing_dashboard_configmap_is_reported(self):
        cluster = FakeCluster()
        cluster.answers['configmap'] = ''
        with patch.object(dashboard, 'run', cluster), self.assertRaisesRegex(ValueError, 'dashboard.enabled=true'):
            dashboard.install(HELM, KUBE, 'gateflow', 'gf')
        self.assertEqual(cluster.helm_upgrades(), [])

    def test_existing_admin_secret_is_not_replaced(self):
        with patch.object(dashboard, 'run') as run:
            run.return_value.stdout = SECRET+'\n'
            dashboard.ensure_secret(KUBE, 'gateflow')
            self.assertEqual(run.call_count, 1)

    def test_existing_admin_secret_without_both_keys_is_refused(self):
        for data in ({'admin-user': 'YWRtaW4='}, {'admin-password': 'c2VjcmV0'}, {}):
            with patch.object(dashboard, 'run') as run:
                run.return_value.stdout = json.dumps({'data': data})
                with self.assertRaisesRegex(ValueError, 'lacks') as raised:
                    dashboard.ensure_secret(KUBE, 'gateflow')
                self.assertEqual(run.call_count, 1)
                for value in data.values():
                    self.assertNotIn(value, str(raised.exception))
        with patch.object(dashboard, 'run') as run:
            run.return_value.stdout = json.dumps({'metadata': {'name': dashboard.ADMIN_SECRET}})
            with self.assertRaisesRegex(ValueError, 'admin-user, admin-password'):
                dashboard.ensure_secret(KUBE, 'gateflow')

    def test_new_admin_secret_is_random_and_sent_only_on_stdin(self):
        with patch.object(dashboard, 'run') as run:
            run.return_value.stdout = ''
            dashboard.ensure_secret(KUBE, 'gateflow')
            command = run.call_args.args[0]
            self.assertNotIn('admin-password', ' '.join(command))
            secret = json.loads(run.call_args.kwargs['input'])
            self.assertIn('admin-password', secret['data'])
            self.assertEqual(secret['metadata']['namespace'], 'gateflow')


if __name__ == '__main__':
    unittest.main()
