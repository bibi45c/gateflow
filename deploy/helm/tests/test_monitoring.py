"""Scrape jobs the KEDA queries and the load test report depend on. Needs no helm."""
import json
from pathlib import Path
import re
import unittest

HELM = Path(__file__).resolve().parents[1]
CHART = HELM/'gateflow'
VALUES = HELM/'monitoring/prometheus-values.yaml'
REPORT_SERIES = ['container_cpu_usage_seconds_total', 'container_memory_working_set_bytes',
                 'container_network_receive_bytes_total', 'container_network_transmit_bytes_total']


def top_level_block(text, key):
    """The lines of a top-level YAML key, up to the next top-level line."""
    lines = text.splitlines(keepends=True)
    start = lines.index(key+':\n')
    end = next((i for i in range(start+1, len(lines)) if lines[i][:1] not in (' ', '\n')), len(lines))
    return ''.join(lines[start:end])


def scrape_jobs(text):
    """Each scrape job's text by job name, in file order, without the comment lines
    that introduce the next job."""
    marker = '      - job_name: '
    body = text[text.index('    scrape_configs:\n'):]
    jobs = {}
    for part in body.split(marker)[1:]:
        name, _, rest = part.partition('\n')
        lines = rest.splitlines(keepends=True)
        while lines and lines[-1].startswith('      #'):
            lines.pop()
        jobs[name] = marker+name+'\n'+''.join(lines)
    return jobs


def pod_job(component):
    """A FlowGate Pod scrape job, exactly as #112 wrote it for the gateway."""
    comment = '''          # Running gateway Pods only, draining ones included: their streams are
          # still demand. Pending Pods have no process to scrape yet.
''' if component == 'gateway' else ''
    return f'''      - job_name: gateflow-{component}
        metrics_path: /metrics
        kubernetes_sd_configs:
          - role: pod
            namespaces:
              names:
                - gateflow
        relabel_configs:
{comment}          - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_name]
            regex: gateflow
            action: keep
          - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_component]
            regex: {component}
            action: keep
          - source_labels: [__meta_kubernetes_pod_container_port_name]
            regex: http
            action: keep
          - source_labels: [__meta_kubernetes_pod_phase]
            regex: Running
            action: keep
          - source_labels: [__meta_kubernetes_namespace]
            target_label: namespace
          - source_labels: [__meta_kubernetes_pod_name]
            target_label: pod
'''


class ScrapeConfigTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.text = VALUES.read_text(encoding='utf-8')
        cls.jobs = scrape_jobs(cls.text)

    def test_jobs(self):
        self.assertEqual(list(self.jobs), ['gateflow-gateway', 'gateflow-access', 'gateflow-metering',
                                           'kubernetes-cadvisor', 'kube-state-metrics'])
        # The chart's default jobs are replaced, so these are the only ones.
        self.assertEqual(self.text.count('job_name:'), 5)

    def test_gateway_job_unchanged(self):
        # autoscaling.job, and so both KEDA queries, select job="gateflow-gateway".
        values = json.loads((CHART/'values.yaml').read_text(encoding='utf-8'))
        self.assertEqual(values['autoscaling']['job'], 'gateflow-gateway')
        self.assertEqual(self.jobs['gateflow-gateway'], pod_job('gateway'))

    def test_access_and_metering_jobs_match_gateway_job(self):
        for component in ['access', 'metering']:
            with self.subTest(component=component):
                job = self.jobs['gateflow-'+component]
                self.assertTrue(job.startswith(pod_job(component)), job)
                self.assertNotIn('honor_labels', job)

    def test_no_target_label_overwrites_service_label(self):
        # The services export service="gateway" and so on; a target label named
        # service would rename theirs to exported_service.
        for name, job in self.jobs.items():
            with self.subTest(job=name):
                self.assertNotRegex(job, r'target_label: (service|exported_service)\b')
                self.assertNotIn('labelmap', job)

    def test_chart_labels_and_port_match_pod_jobs(self):
        helpers = (CHART/'templates/_helpers.tpl').read_text(encoding='utf-8')
        self.assertIn('"app.kubernetes.io/name" "gateflow"', helpers)
        deployments = (CHART/'templates/deployments.yaml').read_text(encoding='utf-8')
        self.assertIn('dict "app.kubernetes.io/component" $name', deployments)
        self.assertIn('(dict "name" "http" "containerPort" $service.port)', deployments)
        values = json.loads((CHART/'values.yaml').read_text(encoding='utf-8'))
        self.assertTrue({'gateway', 'access', 'metering'} <= set(values['services']))

    def test_cadvisor_through_api_server_keeps_only_report_series(self):
        job = self.jobs['kubernetes-cadvisor']
        self.assertIn('        scheme: https\n', job)
        self.assertIn('          ca_file: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt\n', job)
        self.assertNotIn('insecure_skip_verify', job)
        self.assertIn('          credentials_file: /var/run/secrets/kubernetes.io/serviceaccount/token\n', job)
        self.assertIn('          - role: node\n', job)
        self.assertIn('            replacement: kubernetes.default.svc:443\n', job)
        self.assertIn('            target_label: __metrics_path__\n'
                      '            replacement: /api/v1/nodes/$1/proxy/metrics/cadvisor\n', job)
        relabel = job[job.index('        metric_relabel_configs:\n'):]
        self.assertEqual(relabel, '''        metric_relabel_configs:
          - source_labels: [__name__]
            regex: container_cpu_usage_seconds_total|container_memory_working_set_bytes|container_network_receive_bytes_total|container_network_transmit_bytes_total
            action: keep
          - source_labels: [namespace]
            regex: gateflow
            action: keep
''')
        kept = re.search(r'regex: (container_\S+)\n', relabel).group(1).split('|')
        self.assertEqual(sorted(kept), REPORT_SERIES)

    def test_kube_state_metrics_is_minimal(self):
        block = top_level_block(self.text, 'kube-state-metrics')
        self.assertIn('  enabled: true\n', block)
        self.assertIn('  collectors:\n    - deployments\n    - horizontalpodautoscalers\n    - pods\n'
                      '  namespaces: gateflow\n', block)
        self.assertEqual(re.findall(r'^    - (\S+)$', block, re.M), ['deployments', 'horizontalpodautoscalers', 'pods'])
        self.assertIn('  nodeSelector:\n    gateflow.io/workload: services\n', block)
        self.assertIn('    limits:\n      memory: 128Mi\n', block)

    def test_kube_state_metrics_job_keeps_exported_labels(self):
        job = self.jobs['kube-state-metrics']
        self.assertIn('        honor_labels: true\n', job)
        self.assertIn('          - role: pod\n            namespaces:\n              names:\n                - monitoring\n', job)
        self.assertIn('            regex: kube-state-metrics\n            action: keep\n', job)
        self.assertIn('            regex: http\n            action: keep\n', job)
        self.assertNotIn('target_label', job)

    def test_monitoring_namespace_matches_versions(self):
        versions = json.loads((HELM/'monitoring/versions.json').read_text(encoding='utf-8'))
        self.assertEqual(versions['prometheus']['namespace'], 'monitoring')
        self.assertEqual(versions['prometheus']['values'], VALUES.name)


if __name__ == '__main__': unittest.main()
