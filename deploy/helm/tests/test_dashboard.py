"""Check dashboard provisioning and the semantics that could misreport experiments."""
import importlib.util
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('dashboard', HERE/'dashboard.py')
dashboard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(dashboard)


class DashboardTests(unittest.TestCase):
    def setUp(self):
        self.document = json.loads((HERE/'gateflow/files/gateflow-dashboard.json').read_text(encoding='utf-8'))
        self.expressions = [t['expr'] for p in self.document['panels'] for t in p.get('targets', [])]

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

    def test_missing_data_never_becomes_idle_success(self):
        for expr in self.expressions:
            self.assertNotIn('or vector(0)', expr)
            self.assertNotIn('clamp_min', expr)
        percentile = [e for e in self.expressions if e.startswith('histogram_quantile')]
        self.assertEqual(len(percentile), 4)
        self.assertTrue(all('sum by (le) (rate(' in e for e in percentile))
        self.assertTrue(any('kube_deployment_spec_replicas' in e for e in self.expressions))
        self.assertTrue(any('kube_deployment_status_replicas_available' in e for e in self.expressions))

    def test_dashboard_distinguishes_formal_and_observed_completion(self):
        text = json.dumps(self.document)
        self.assertIn('not the proposal completion SLO', text)
        self.assertIn('not metric labels or data filters', text)
        self.assertIn('not paired client added latency', text)
        ids = [p['id'] for p in self.document['panels']]
        self.assertEqual(len(ids), len(set(ids)))

    def test_existing_admin_secret_is_not_replaced(self):
        with patch.object(dashboard, 'run') as run:
            run.return_value.stdout = 'secret/gateflow-grafana-admin\n'
            dashboard.ensure_secret(['kubectl', '--context', 'kind-gateflow74'], 'gateflow')
            self.assertEqual(run.call_count, 1)

    def test_new_admin_secret_is_random_and_sent_only_on_stdin(self):
        with patch.object(dashboard, 'run') as run:
            run.return_value.stdout = ''
            dashboard.ensure_secret(['kubectl', '--context', 'kind-gateflow74'], 'gateflow')
            command = run.call_args.args[0]
            self.assertNotIn('admin-password', ' '.join(command))
            secret = json.loads(run.call_args.kwargs['input'])
            self.assertIn('admin-password', secret['data'])
            self.assertEqual(secret['metadata']['namespace'], 'gateflow')


if __name__ == '__main__':
    unittest.main()
