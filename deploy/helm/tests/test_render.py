import json
from pathlib import Path
import subprocess
import unittest

CHART = Path(__file__).resolve().parents[1] / 'gateflow'

def render(*args):
    p = subprocess.run(['helm', 'template', 'gf', str(CHART), '--kube-version', '1.35.0', *args], capture_output=True, text=True, encoding='utf-8')
    if p.returncode:
        raise AssertionError(p.stderr)
    return [json.loads('\n'.join(line for line in doc.splitlines() if not line.startswith('#'))) for doc in p.stdout.split('---') if doc.strip()]

class ChartTests(unittest.TestCase):
    def test_workloads_and_private_routes(self):
        docs = render()
        deployments = [d for d in docs if d['kind'] == 'Deployment']
        self.assertEqual(len(deployments), 4)
        self.assertEqual(len([d for d in docs if d['kind'] == 'Service']), 4)
        self.assertFalse(any(d['kind'] == 'Secret' for d in docs))
        for d in deployments:
            name=d['metadata']['labels']['app.kubernetes.io/component']
            pod=d['spec']['template']['spec']; container=pod['containers'][0]
            self.assertEqual(pod['nodeSelector']['gateflow.io/workload'], 'load-generator' if name=='mock-upstream' else 'services')
            self.assertEqual(container['readinessProbe']['httpGet']['path'],'/readyz')
            self.assertEqual(container['livenessProbe']['httpGet']['path'],'/healthz')
            self.assertGreaterEqual(pod['terminationGracePeriodSeconds'],85)
            self.assertIn('sleep',container['lifecycle']['preStop'])
        ingress=next(d for d in docs if d['kind']=='Ingress')
        paths=ingress['spec']['rules'][0]['http']['paths']
        self.assertEqual({p['path'] for p in paths},{'/v1/messages','/v1/responses'})
        self.assertTrue(all(p['pathType']=='Exact' for p in paths))

    def test_ordered_jobs(self):
        jobs={d['metadata']['name']:d for d in render() if d['kind']=='Job'}
        self.assertEqual(set(jobs),{'gf-bootstrap','gf-seed-access','gf-seed-metering'})
        weights=[]
        for name in ['gf-bootstrap','gf-seed-access','gf-seed-metering']:
            job=jobs[name]
            weights.append(int(job['metadata']['annotations']['helm.sh/hook-weight']))
            self.assertEqual(job['spec']['activeDeadlineSeconds'],180)
            self.assertFalse(job['spec']['template']['spec']['automountServiceAccountToken'])
        self.assertEqual(weights,[-20,10,20])
        bootstrap=jobs['gf-bootstrap']['spec']['template']['spec']['containers'][0]
        self.assertTrue(all('valueFrom' in e for e in bootstrap['env']))
        self.assertNotIn('local-only',json.dumps(jobs))

    def test_invalid_configuration_rejected(self):
        for setting in ['environment=production', 'imageTag=', 'services.gateway.image=', 'shutdown.timeout=10']:
            with self.subTest(setting=setting):
                p=subprocess.run(['helm','template','gf',str(CHART),'--kube-version','1.35.0','--set',setting],capture_output=True,text=True)
                self.assertNotEqual(p.returncode,0)

    def test_invalid_grace_rejected(self):
        p=subprocess.run(['helm','template','gf',str(CHART),'--kube-version','1.35.0','--set','shutdown.grace=10'],capture_output=True,text=True)
        self.assertNotEqual(p.returncode,0)
        self.assertIn('grace',p.stderr)

    def test_aws_annotations_and_secret_references(self):
        docs=render('--set','environment=aws')
        ingress=next(d for d in docs if d['kind']=='Ingress')
        self.assertIn('idle_timeout.timeout_seconds=300',ingress['metadata']['annotations']['alb.ingress.kubernetes.io/load-balancer-attributes'])
        access=next(d for d in docs if d['kind']=='Deployment' and d['metadata']['name']=='gf-access')
        env=access['spec']['template']['spec']['containers'][0]['env']
        self.assertTrue(next(e for e in env if e['name']=='OLLAMA_BASE_URL')['value'])
        self.assertTrue(next(e for e in env if e['name']=='OLLAMA_MODEL')['value'])
        self.assertIn('valueFrom',next(e for e in env if e['name']=='DATABASE_URL'))
        init=access['spec']['template']['spec']['initContainers'][0]
        self.assertIn('sslmode=require',init['command'][-1])
        self.assertIn('pg_stat_ssl',init['command'][-1])
        self.assertEqual(init['securityContext']['runAsUser'],65532)

if __name__=='__main__': unittest.main()

