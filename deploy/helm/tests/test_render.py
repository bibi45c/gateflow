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

    def test_database_max_conns_unset_by_default(self):
        for args in [(),('--set','environment=aws',*AWS_CIDRS)]:
            with self.subTest(args=args):
                self.assertNotIn('DATABASE_MAX_CONNS',json.dumps(render(*args)))

    def test_database_max_conns_only_on_the_service_set(self):
        docs=render('--set','services.metering.databaseMaxConns=20')
        deployments={d['metadata']['name']:d for d in docs if d['kind']=='Deployment'}
        for name,d in deployments.items():
            env=d['spec']['template']['spec']['containers'][0]['env']
            found=[e for e in env if e['name']=='DATABASE_MAX_CONNS']
            with self.subTest(name=name):
                self.assertEqual(found,[{'name':'DATABASE_MAX_CONNS','value':'20'}] if name=='gf-metering' else [])
        seed=next(d for d in docs if d['metadata']['name']=='gf-seed-access')
        self.assertNotIn('DATABASE_MAX_CONNS',json.dumps(seed))

    def test_database_max_conns_leaves_the_tls_check_url(self):
        base=render('--set','environment=aws',*AWS_CIDRS)
        capped=render('--set','environment=aws',*AWS_CIDRS,'--set','services.access.databaseMaxConns=20','--set','services.metering.databaseMaxConns=20')
        for name in ['gf-access','gf-metering']:
            with self.subTest(name=name):
                before,after=[next(d for d in docs if d['kind']=='Deployment' and d['metadata']['name']==name)['spec']['template']['spec'] for docs in (base,capped)]
                self.assertEqual(after['initContainers'],before['initContainers'])
                self.assertEqual([e['name'] for e in after['initContainers'][0]['env']],['DATABASE_URL'])
                self.assertNotIn('pool_max_conns',json.dumps(after))
                url=lambda spec: next(e for e in spec['containers'][0]['env'] if e['name']=='DATABASE_URL')
                self.assertEqual(url(after),url(before))
                self.assertIn({'name':'DATABASE_MAX_CONNS','value':'20'},after['containers'][0]['env'])

    def test_invalid_database_max_conns_rejected(self):
        for value in ['0','-1','abc','1.5','2147483648']:
            with self.subTest(value=value):
                p=subprocess.run(['helm','template','gf',str(CHART),'--kube-version','1.35.0','--set',f'services.metering.databaseMaxConns={value}'],capture_output=True,text=True)
                self.assertNotEqual(p.returncode,0)
                self.assertIn('databaseMaxConns',p.stderr)

    def test_aws_gateway_resources(self):
        docs=render('-f',str(CHART.parent/'values-aws.yaml'),*AWS_CIDRS)
        deployments={d['metadata']['name']:d for d in docs if d['kind']=='Deployment'}
        gateway=deployments['gf-gateway']['spec']['template']['spec']['containers'][0]
        # CPU limit = request (proposal 5.6), so a Pod cannot borrow idle node CPU.
        self.assertEqual(gateway['resources'],{'requests':{'cpu':'1','memory':'2Gi'},'limits':{'cpu':'1','memory':'2Gi'}})
        access=deployments['gf-access']['spec']['template']['spec']['containers'][0]
        self.assertEqual(access['resources']['requests']['cpu'],'50m')

    def test_autoscaling_off_by_default(self):
        for args in [(), ('-f',str(CHART.parent/'values-kind.yaml'))]:
            with self.subTest(args=args):
                docs=render(*args)
                self.assertFalse(any(d['kind']=='ScaledObject' for d in docs))
                deployments={d['metadata']['name']:d for d in docs if d['kind']=='Deployment'}
                self.assertTrue(all(d['spec']['replicas']==1 for d in deployments.values()))

    def test_scaled_object(self):
        docs=render('--set','autoscaling.enabled=true','--set','autoscaling.targetPerReplica=5','--set','autoscaling.requestDurationSeconds=25')
        scaled=[d for d in docs if d['kind']=='ScaledObject']
        self.assertEqual(len(scaled),1)
        spec=scaled[0]['spec']
        self.assertEqual(scaled[0]['apiVersion'],'keda.sh/v1alpha1')
        self.assertEqual(spec['scaleTargetRef'],{'name':'gf-gateway'})
        self.assertEqual((spec['minReplicaCount'],spec['maxReplicaCount']),(2,4))
        hpa=spec['advanced']['horizontalPodAutoscalerConfig']
        self.assertEqual(hpa['name'],'gf-gateway')
        self.assertEqual(hpa['behavior']['scaleDown']['stabilizationWindowSeconds'],60)
        self.assertNotIn('fallback',spec)
        triggers={t['name']:t for t in spec['triggers']}
        self.assertEqual(set(triggers),{'active-admitted','eligible-demand'})
        for trigger in triggers.values():
            meta=trigger['metadata']
            self.assertEqual((trigger['type'],trigger['metricType']),('prometheus','AverageValue'))
            self.assertEqual(meta['threshold'],'5')
            self.assertEqual(meta['ignoreNullValues'],'false')
            self.assertEqual(meta['serverAddress'],'http://prometheus-server.monitoring.svc.cluster.local')
            self.assertNotIn('vector(0)',meta['query'])
            self.assertNotIn(' or ',meta['query'])
            self.assertIn('and on() (min(up{job="gateflow-gateway"}) == 1)',meta['query'])
            self.assertIn('and on() (max(time() - timestamp(up{job="gateflow-gateway"})) < 30)',meta['query'])
        self.assertTrue(triggers['active-admitted']['metadata']['query'].startswith('sum(gateflow_gateway_active_admitted_requests{job="gateflow-gateway"}) and on()'))
        self.assertIn('absent(up{job="gateflow-gateway"} unless on(instance) gateflow_gateway_active_admitted_requests{job="gateflow-gateway"})',
                      triggers['active-admitted']['metadata']['query'])
        self.assertTrue(triggers['eligible-demand']['metadata']['query'].startswith('sum(rate(gateflow_gateway_eligible_arrivals_total{job="gateflow-gateway"}[1m])) * 25 and on()'))
        deployments={d['metadata']['name']:d for d in docs if d['kind']=='Deployment'}
        self.assertNotIn('replicas',deployments['gf-gateway']['spec'])
        for name in ['gf-access','gf-metering','gf-mock-upstream']:
            self.assertEqual(deployments[name]['spec']['replicas'],1)

    def test_scaled_object_on_aws_and_kind(self):
        for values,threshold in [('values-aws.yaml','70'),('values-kind.yaml','4')]:
            with self.subTest(values=values):
                args=['-f',str(CHART.parent/values)]+(list(AWS_CIDRS) if values=='values-aws.yaml' else ['--set','autoscaling.enabled=true'])
                docs=render(*args)
                scaled=next(d for d in docs if d['kind']=='ScaledObject')
                self.assertEqual({t['metadata']['threshold'] for t in scaled['spec']['triggers']},{threshold})
                gateway=next(d for d in docs if d['kind']=='Deployment' and d['metadata']['name']=='gf-gateway')
                self.assertNotIn('replicas',gateway['spec'])

    def test_invalid_autoscaling_rejected(self):
        for setting in ['autoscaling.maxReplicas=1','autoscaling.targetPerReplica=0','autoscaling.job=a"b',
                        'autoscaling.rateWindow=1m]','autoscaling.serverAddress=prometheus:9090']:
            with self.subTest(setting=setting):
                p=subprocess.run(['helm','template','gf',str(CHART),'--kube-version','1.35.0','--set','autoscaling.enabled=true','--set',setting],capture_output=True,text=True)
                self.assertNotEqual(p.returncode,0)

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
    # One vCPU and 2 GiB per gateway Pod (proposal 5.6). The CPU limit keeps a Pod
    # from borrowing idle node CPU, so replica comparisons hold; frozen from before
    # the single-Pod calibration, and every later run reuses it.
    resources:
      requests:
        cpu: "1"
        memory: 2Gi
      limits:
        cpu: "1"
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
