import contextlib
import datetime
import importlib.util
import io
import json
from pathlib import Path
import re
import tempfile
import unittest

HELM = Path(__file__).resolve().parents[1]
CHART = HELM/'gateflow'


def load(name):
    spec=importlib.util.spec_from_file_location(name,HELM/(name+'.py'))
    module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
    return module


class TemplateSourceTests(unittest.TestCase):
    """Checks that need no helm; test_render.py renders the same template."""

    def setUp(self):
        self.template=(CHART/'templates/scaledobject.yaml').read_text(encoding='utf-8')
        self.values=json.loads((CHART/'values.yaml').read_text(encoding='utf-8'))['autoscaling']

    def test_defaults(self):
        self.assertFalse(self.values['enabled'])
        self.assertEqual((self.values['minReplicas'],self.values['maxReplicas']),(2,4))
        self.assertEqual(self.values['scaleDownStabilizationSeconds'],60)
        self.assertGreater(self.values['targetPerReplica'],0)
        self.assertGreater(self.values['requestDurationSeconds'],0)

    def test_rendered_only_when_enabled(self):
        self.assertTrue(self.template.startswith('{{- $a := .Values.autoscaling -}}\n{{- if $a.enabled -}}\n'))
        self.assertTrue(self.template.rstrip().endswith('{{- end }}'))

    def test_two_average_value_triggers_share_threshold_from_values(self):
        triggers=[line for line in self.template.splitlines() if '"type" "prometheus"' in line]
        self.assertEqual(len(triggers),2)
        self.assertEqual({re.search(r'"name" "([a-z-]+)"',t).group(1) for t in triggers},{'active-admitted','eligible-demand'})
        for trigger in triggers:
            self.assertIn('"metricType" "AverageValue"',trigger)
            self.assertIn('"threshold" $t ',trigger)
            self.assertIn('"ignoreNullValues" "false"',trigger)
            self.assertIn('"serverAddress" $a.serverAddress',trigger)
        self.assertIn('{{- $t := toString $a.targetPerReplica -}}',self.template)
        self.assertEqual(self.template.count('$t :='),1)

    def test_queries_come_from_values_without_division_by_replicas(self):
        self.assertIn('printf "{job=%q}" $a.job',self.template)
        self.assertIn('$a.rateWindow',self.template)
        self.assertIn('* %v %s',self.template)
        self.assertIn('$a.requestDurationSeconds',self.template)
        self.assertIn('$active := printf "gateflow_gateway_active_admitted_requests%s" $sel',self.template)
        self.assertIn('$arrivals := printf "rate(gateflow_gateway_eligible_arrivals_total%s[%s])" $sel $a.rateWindow',self.template)
        self.assertIn('printf "sum(%s) %s and on()',self.template)
        self.assertIn('printf "sum(%s) * %v %s and on()',self.template)
        self.assertNotIn(' / ',self.template)

    def test_missing_data_stays_missing(self):
        self.assertIn('min(up%s) == 1',self.template)
        self.assertIn('timestamp(up%s)',self.template)
        self.assertEqual(self.template.count('absent(up%s unless on(instance) %s)'),2)
        for path in [*CHART.rglob('*'),*HELM.glob('values-*.yaml'),*(HELM/'monitoring').glob('*')]:
            if path.is_file():
                text=path.read_text(encoding='utf-8')
                self.assertNotRegex(text,r'vector\(\s*0\s*\)',path)
                self.assertNotIn('ignoreNullValues" "true',text)
                self.assertNotIn('fallback',text)

    def test_replicas_and_downscale_window(self):
        self.assertIn('"minReplicaCount" (int $a.minReplicas) "maxReplicaCount" (int $a.maxReplicas)',self.template)
        self.assertIn('"scaleDown" (dict "stabilizationWindowSeconds" (int $a.scaleDownStabilizationSeconds))',self.template)
        self.assertIn('fail "autoscaling.maxReplicas must be >= autoscaling.minReplicas"',self.template)
        # The HPA is named after the gateway, as autoscale.Cluster reads it.
        self.assertIn('$hpa := dict "name" $name',self.template)
        self.assertIn('"scaleTargetRef" (dict "name" $name)',self.template)

    def test_deployment_omits_replicas_for_autoscaled_gateway(self):
        text=(CHART/'templates/deployments.yaml').read_text(encoding='utf-8')
        self.assertNotIn('"replicas" $service.replicas "selector"',text)
        lines=text.splitlines()
        guard=lines.index('{{- if not (and (eq $name "gateway") $.Values.autoscaling.enabled) -}}')
        self.assertEqual(lines[guard+1],'{{- $_ := set $spec "replicas" $service.replicas -}}')
        self.assertEqual(lines[guard+2],'{{- end }}')
        self.assertIn('"spec" $spec | toJson',text)

    def test_schema_covers_autoscaling(self):
        schema=json.loads((CHART/'values.schema.json').read_text(encoding='utf-8'))['properties']['autoscaling']['properties']
        self.assertEqual(set(schema),set(self.values))
        job=re.compile(schema['job']['pattern'])
        self.assertTrue(job.match('gateflow-gateway'))
        self.assertFalse(job.match('x"} or vector(0) #'))
        window=re.compile(schema['rateWindow']['pattern'])
        self.assertTrue(window.match('1m'))
        self.assertFalse(window.match('1m] or'))

    def test_environment_values(self):
        aws=(HELM/'values-aws.yaml').read_text(encoding='utf-8')
        self.assertIn('autoscaling:\n  enabled: true\n',aws)
        self.assertIn('  targetPerReplica: ',aws)
        self.assertIn('  requestDurationSeconds: ',aws)
        kind=(HELM/'values-kind.yaml').read_text(encoding='utf-8')
        self.assertNotIn('enabled',kind)
        self.assertIn('  targetPerReplica: 4\n',kind)


class MonitoringSourceTests(unittest.TestCase):
    def test_versions_are_pinned(self):
        versions=json.loads((HELM/'monitoring/versions.json').read_text(encoding='utf-8'))
        self.assertEqual(set(versions),{'prometheus','keda'})
        for chart in versions.values():
            self.assertRegex(chart['version'],r'^\d+\.\d+\.\d+$')
            self.assertTrue(chart['repository'].startswith('https://'))
            self.assertTrue((HELM/'monitoring'/chart['values']).is_file())

    def test_prometheus_server_on_empty_dir_without_other_components(self):
        # kube-state-metrics is enabled on purpose; test_monitoring.py checks it.
        text=(HELM/'monitoring/prometheus-values.yaml').read_text(encoding='utf-8')
        for chart in ['alertmanager','prometheus-node-exporter','prometheus-pushgateway']:
            self.assertIn(f'{chart}:\n  enabled: false\n',text)
        self.assertIn('  persistentVolume:\n    enabled: false\n',text)
        self.assertIn('  emptyDir:\n',text)
        self.assertIn('      - job_name: gateflow-gateway\n',text)
        self.assertIn('server:\n  nodeSelector:\n    gateflow.io/workload: services\n',text)

    def test_keda_on_service_nodes(self):
        text=(HELM/'monitoring/keda-values.yaml').read_text(encoding='utf-8')
        self.assertIn('nodeSelector:\n  gateflow.io/workload: services\n',text)

    def test_install_command_pins_version_and_context(self):
        module=load('autoscale')
        helm=['helm','--kubeconfig','cfg','--kube-context','kind-gateflow72']
        chart=module.load_versions()['keda']
        command=module.install_command(helm,chart)
        self.assertEqual(command[:5],helm)
        self.assertEqual(command[command.index('--version')+1],chart['version'])
        self.assertEqual(command[command.index('--repo')+1],chart['repository'])
        self.assertTrue(command[command.index('-f')+1].endswith('keda-values.yaml'))


class FakeTime:
    def __init__(self):
        self.now=0.0

    def clock(self):
        return self.now

    def sleep(self, seconds):
        self.now+=seconds


class FakeLoad:
    def __init__(self):
        self.running=False
        self.starts=0

    def start(self):
        self.running=True; self.starts+=1

    def stop(self):
        self.running=False

    def active(self):
        return 18 if self.running else 0

    def stats(self):
        return {'started':self.starts,'completed':0,'failed':0}


class FakeCluster:
    """A crude HPA: up a step per poll under load, down after the window when idle, frozen without Prometheus."""

    def __init__(self, load, time, window=60, drop_when_blind=False, start_polls=0):
        self.load,self.time,self.window=load,time,window
        self.desired=1; self.idle_since=None; self.prometheus=True
        self.drop_when_blind=drop_when_blind
        # Polls before the HPA first raises the newly autoscaled gateway from one replica.
        self.start_polls=start_polls
        self.scaled=[]

    def state(self):
        if self.start_polls:
            self.start_polls-=1
            return {'desired':1,'ready':1,'hpa_desired':None,'metrics':[]}
        if not self.prometheus:
            if self.drop_when_blind: self.desired=2
        elif self.load.running:
            self.idle_since=None
            self.desired=min(4,self.desired+1)
        elif self.desired<2:
            self.desired=2
        else:
            self.idle_since=self.time.now if self.idle_since is None else self.idle_since
            if self.time.now-self.idle_since>=self.window: self.desired=2
        return {'desired':self.desired,'ready':self.desired,'hpa_desired':self.desired,'metrics':['1']}

    def conditions(self):
        status='True' if self.prometheus else 'False'
        return [{'type':'ScalingActive','status':status,'reason':'r','message':'m'}]

    def scale_prometheus(self, replicas):
        self.scaled.append(replicas)
        self.prometheus=bool(replicas)


SETTINGS={'minReplicas':2,'maxReplicas':4,'scaleDownStabilizationSeconds':60,'poll':5,'scaleUpTimeout':300}


class RehearsalLogicTests(unittest.TestCase):
    def setUp(self):
        self.module=load('autoscale')

    def run_rehearsal(self, **options):
        time=FakeTime(); generator=FakeLoad()
        fake=FakeCluster(generator,time,**options)
        logs=[]
        record=self.module.rehearse(fake,generator,SETTINGS,clock=time.clock,sleep=time.sleep,log=logs.append)
        return record,fake,logs

    def test_healthy_cluster_passes_every_phase(self):
        record,fake,logs=self.run_rehearsal()
        self.assertTrue(record['passed'])
        self.assertEqual(list(record['phases']),['baseline','scale_up','scale_down','missing_metrics','recovery'])
        self.assertTrue(all(p['passed'] for p in record['phases'].values()))
        self.assertEqual(record['phases']['scale_up']['max_desired'],4)
        self.assertTrue(record['phases']['scale_up']['reached_max'])
        outage=record['phases']['missing_metrics']
        self.assertEqual(outage['desired_at_outage'],3)
        self.assertGreaterEqual(outage['min_desired'],3)
        self.assertGreaterEqual(outage['observed_seconds'],SETTINGS['scaleDownStabilizationSeconds'])
        self.assertTrue(outage['metrics_unavailable'])
        self.assertEqual(fake.scaled,[0,1])
        self.assertFalse(record['phases']['recovery']['required'])
        self.assertEqual(len(logs),5)
        sample=record['samples'][0]
        self.assertEqual(set(sample),{'t','phase','streams','desired','ready','hpa_desired','metrics'})
        self.assertEqual({s['phase'] for s in record['samples']},
                         {'baseline','scale_up','scale_down','missing_metrics_load','missing_metrics','recovery'})
        self.assertTrue(all(s['streams']==0 for s in record['samples'] if s['phase']=='missing_metrics'))
        json.dumps(record)

    def test_baseline_waits_for_hpa_to_raise_one_replica(self):
        record,_,_=self.run_rehearsal(start_polls=3)
        baseline=[s for s in record['samples'] if s['phase']=='baseline']
        self.assertEqual([s['desired'] for s in baseline],[1,1,1,2,2])
        self.assertTrue(record['phases']['baseline']['passed'])
        self.assertEqual(record['phases']['baseline']['final_desired'],2)
        self.assertEqual(record['phases']['baseline']['seconds_to_min'],15.0)
        self.assertTrue(record['passed'])

    def test_scale_down_while_prometheus_is_down_fails(self):
        record,fake,_=self.run_rehearsal(drop_when_blind=True)
        self.assertFalse(record['passed'])
        self.assertFalse(record['phases']['missing_metrics']['passed'])
        self.assertEqual(record['phases']['missing_metrics']['min_desired'],2)
        self.assertEqual(fake.scaled,[0,1])

    def test_prometheus_is_restored_after_an_error(self):
        time=FakeTime(); generator=FakeLoad(); fake=FakeCluster(generator,time)
        state=fake.state
        def failing():
            if fake.prometheus is False: raise RuntimeError('kubectl failed')
            return state()
        fake.state=failing
        with self.assertRaises(RuntimeError):
            self.module.rehearse(fake,generator,SETTINGS,clock=time.clock,sleep=time.sleep,log=lambda _:None)
        self.assertEqual(fake.scaled,[0,1])
        self.assertFalse(generator.running)

    def test_judgements(self):
        def samples(*desired):
            return [{'t':5.0*i,'desired':d,'ready':d} for i,d in enumerate(desired)]
        self.assertTrue(self.module.judge_baseline(samples(1,1,2,2),2,2)['passed'])
        self.assertFalse(self.module.judge_baseline(samples(1,1,2),2,2)['passed'])
        self.assertFalse(self.module.judge_baseline(samples(2,2,1),2,2)['passed'])
        self.assertFalse(self.module.judge_baseline(samples(1,1,1),2,2)['passed'])
        self.assertFalse(self.module.judge_baseline([{'t':0.0,'desired':2,'ready':1},{'t':5.0,'desired':2,'ready':1}],2,2)['passed'])
        up=self.module.judge_scale_up(samples(2,3,4),2,4)
        self.assertEqual((up['passed'],up['reached_max'],up['seconds_to_max']),(True,True,10.0))
        self.assertFalse(self.module.judge_scale_up(samples(2,2,2),2,4)['passed'])
        self.assertFalse(self.module.judge_scale_up(samples(1,3),2,4)['passed'])
        self.assertTrue(self.module.settled(samples(4,3,2),2)['passed'])
        self.assertEqual(self.module.settled(samples(4,3,2),2)['seconds_to_min'],10.0)
        self.assertFalse(self.module.settled(samples(4,4,3),2)['passed'])
        self.assertFalse(self.module.settled(samples(3,1,2),2)['passed'])
        held=self.module.judge_outage(samples(2,4),samples(4,4,4),2,[])
        self.assertTrue(held['passed'])
        self.assertFalse(held['metrics_unavailable'])
        self.assertFalse(self.module.judge_outage(samples(2,4),samples(4,3,3),2,[])['passed'])
        never=self.module.judge_outage(samples(2,2),[],2,[])
        self.assertFalse(never['passed'])
        self.assertIn('never rose',never['reason'])

    def test_load_size_and_keys(self):
        self.assertEqual(self.module.stream_count(4,4),18)
        self.assertEqual(self.module.stream_count(70,4),self.module.MAX_STREAMS)
        rehearse=load('rehearse')
        smoke=importlib.util.spec_from_file_location('smoke',HELM.parents[1]/'test/e2e/smoke.py')
        smoke_module=importlib.util.module_from_spec(smoke); smoke.loader.exec_module(smoke_module)
        secret=rehearse.SEED_SECRET
        self.assertEqual(self.module.derive_key(secret,'tenant-03'),smoke_module.derive_key(secret.encode(),'tenant-03'))

    def test_record_is_written_under_given_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            now=datetime.datetime(2026,10,16,1,2,3,tzinfo=datetime.timezone.utc)
            path=self.module.write_record(Path(directory)/'autoscale',{'passed':True},now)
            self.assertEqual(path.name,'autoscale-20261016T010203Z.json')
            self.assertEqual(json.loads(path.read_text(encoding='utf-8')),{'passed':True})


class RehearseFlagTests(unittest.TestCase):
    def test_autoscale_check_requires_existing_cluster(self):
        module=load('rehearse')
        with self.assertRaises(SystemExit), contextlib.redirect_stderr(io.StringIO()):
            module.main(['--cluster','gateflow72','--kubeconfig','cfg','--autoscale-check'])


if __name__=='__main__': unittest.main()
