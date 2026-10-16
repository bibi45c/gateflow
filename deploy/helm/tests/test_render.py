import json
from pathlib import Path
import re
import subprocess
import unittest

CHART = Path(__file__).resolve().parents[1] / 'gateflow'
INBOUND = 'alb.ingress.kubernetes.io/inbound-cidrs'
# Documentation address (RFC 5737); aws renders fail without inbound CIDRs.
AWS_CIDRS = ('--set-json', 'ingress.inboundCidrs=["192.0.2.1/32"]')

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
        docs=render('--set','environment=aws',*AWS_CIDRS)
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

    def test_aws_gateway_resources(self):
        docs=render('-f',str(CHART.parent/'values-aws.yaml'),*AWS_CIDRS)
        deployments={d['metadata']['name']:d for d in docs if d['kind']=='Deployment'}
        gateway=deployments['gf-gateway']['spec']['template']['spec']['containers'][0]
        self.assertEqual(gateway['resources'],{'requests':{'cpu':'1','memory':'2Gi'},'limits':{'memory':'2Gi'}})
        access=deployments['gf-access']['spec']['template']['spec']['containers'][0]
        self.assertEqual(access['resources']['requests']['cpu'],'50m')

    def test_aws_requires_inbound_cidrs(self):
        for args in [['--set','environment=aws'],['-f',str(CHART.parent/'values-aws.yaml')]]:
            with self.subTest(args=args):
                p=subprocess.run(['helm','template','gf',str(CHART),'--kube-version','1.35.0',*args],capture_output=True,text=True)
                self.assertNotEqual(p.returncode,0)
                self.assertIn('ingress.inboundCidrs',p.stderr)

    def test_aws_inbound_cidrs_annotation(self):
        docs=render('--set','environment=aws','--set-json','ingress.inboundCidrs=["198.51.100.7/32","203.0.113.0/24"]')
        ingress=next(d for d in docs if d['kind']=='Ingress')
        self.assertEqual(ingress['metadata']['annotations'][INBOUND],'198.51.100.7/32,203.0.113.0/24')
        self.assertEqual(ingress['metadata']['annotations']['alb.ingress.kubernetes.io/scheme'],'internet-facing')

    def test_aws_any_source_needs_explicit_switch(self):
        args=['helm','template','gf',str(CHART),'--kube-version','1.35.0','--set','environment=aws','--set-json','ingress.inboundCidrs=["198.51.100.7/32","0.0.0.0/0"]']
        p=subprocess.run(args,capture_output=True,text=True)
        self.assertNotEqual(p.returncode,0)
        self.assertIn('allowAnySource',p.stderr)
        docs=render(*args[6:],'--set','ingress.allowAnySource=true')
        ingress=next(d for d in docs if d['kind']=='Ingress')
        self.assertEqual(ingress['metadata']['annotations'][INBOUND],'198.51.100.7/32,0.0.0.0/0')

    def test_invalid_inbound_cidrs_rejected(self):
        for cidrs in ['["198.51.100.7"]','["2001:db8::/32"]','"198.51.100.7/32"']:
            with self.subTest(cidrs=cidrs):
                p=subprocess.run(['helm','template','gf',str(CHART),'--kube-version','1.35.0','--set','environment=aws','--set-json',f'ingress.inboundCidrs={cidrs}'],capture_output=True,text=True)
                self.assertNotEqual(p.returncode,0)

    def test_kind_has_no_inbound_cidrs(self):
        for args in [[],['-f',str(CHART.parent/'values-kind.yaml')]]:
            with self.subTest(args=args):
                ingress=next(d for d in render(*args) if d['kind']=='Ingress')
                self.assertEqual(ingress['spec']['ingressClassName'],'nginx')
                self.assertNotIn(INBOUND,ingress['metadata']['annotations'])
                self.assertFalse(any(k.startswith('alb.') for k in ingress['metadata']['annotations']))

class SourceTests(unittest.TestCase):
    def test_bootstrap_grants_service_roles_before_schemas(self):
        lines=[line.strip() for line in (CHART/'files/bootstrap.sql').read_text(encoding='utf-8').splitlines()]
        grant=lines.index('GRANT access, metering TO CURRENT_USER;')
        creates=[i for i,line in enumerate(lines) if 'CREATE ROLE' in line]
        schemas=[i for i,line in enumerate(lines) if line.startswith('CREATE SCHEMA')]
        self.assertEqual(len(creates),2)
        self.assertEqual(len(schemas),2)
        self.assertLess(max(creates),grant)
        self.assertLess(grant,min(schemas))
        self.assertTrue(lines[grant-1].startswith('--'))

    def test_aws_values_size_gateway(self):
        text=(CHART.parent/'values-aws.yaml').read_text(encoding='utf-8')
        block='''services:
  gateway:
    # One vCPU and 2 GiB per gateway Pod (proposal 5.6).
    resources:
      requests:
        cpu: "1"
        memory: 2Gi
      limits:
        memory: 2Gi
'''
        self.assertIn(block,text)
        self.assertIn('environment: aws\n',text)

    def test_aws_values_keep_no_inbound_addresses(self):
        text=(CHART.parent/'values-aws.yaml').read_text(encoding='utf-8')
        self.assertIn("--set-json 'ingress.inboundCidrs=[\"<your IP>/32\"]'",text)
        active=[line for line in text.splitlines() if not line.lstrip().startswith('#')]
        self.assertFalse([line for line in active if re.search(r'\d+\.\d+\.\d+\.\d+',line) or 'inboundCidrs' in line])

    def test_ingress_template_restricts_alb_sources(self):
        text=(CHART/'templates/ingress.yaml').read_text(encoding='utf-8')
        self.assertIn('"alb.ingress.kubernetes.io/inbound-cidrs" (join "," $cidrs)',text)
        self.assertIn('{{- if not $cidrs -}}\n{{- fail `environment=aws requires ingress.inboundCidrs',text)
        self.assertIn("--set-json 'ingress.inboundCidrs=[\"203.0.113.10/32\"]'",text)
        self.assertIn('{{- if and (hasSuffix "/0" $cidr) (not $.Values.ingress.allowAnySource) -}}\n{{- fail',text)
        self.assertIn('0.0.0.0/0',text)
        # The CIDR checks sit inside the aws branch, so kind renders are unchanged.
        aws=text.index('{{- if $aws -}}')
        end=text.index('{{- end -}}\n{{- $paths')
        for needle in ['inboundCidrs','hasSuffix','inbound-cidrs']:
            self.assertTrue(aws<text.index(needle)<end,needle)

    def test_values_default_to_no_inbound_sources(self):
        values=json.loads((CHART/'values.yaml').read_text(encoding='utf-8'))
        self.assertEqual(values['ingress'],{'inboundCidrs':[],'allowAnySource':False})
        schema=json.loads((CHART/'values.schema.json').read_text(encoding='utf-8'))['properties']['ingress']['properties']
        self.assertEqual(schema['allowAnySource'],{'type':'boolean'})
        self.assertEqual(schema['inboundCidrs']['type'],'array')
        self.assertEqual(schema['inboundCidrs']['items']['type'],'string')
        pattern=re.compile(schema['inboundCidrs']['items']['pattern'])
        for cidr in ['198.51.100.7/32','203.0.113.0/24','0.0.0.0/0','10.0.0.0/8']:
            self.assertRegex(cidr,pattern)
        for cidr in ['198.51.100.7','198.51.100.7/33','2001:db8::/32','198.51.100.7/32,203.0.113.0/24','']:
            self.assertNotRegex(cidr,pattern)

if __name__=='__main__': unittest.main()
