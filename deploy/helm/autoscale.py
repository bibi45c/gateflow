"""Rehearse KEDA autoscaling of the gateway on an existing kind cluster (#72).

Run through `rehearse.py --existing-cluster --autoscale-check`. Installs the pinned
Prometheus and KEDA charts, turns autoscaling on, then drives mock-load streams
through the port-forwarded ingress and records the gateway's replicas: scale-up,
scale-down after the load stops, and no scale-down while Prometheus is down.
"""
import base64
import datetime
import hashlib
import hmac
import http.client
import json
import math
from pathlib import Path
import subprocess
import threading
import time

HERE = Path(__file__).resolve().parent
MONITORING = HERE/'monitoring'
# The kind node runs out of room before this many streams matter to the check.
MAX_STREAMS = 64


def load_versions():
    return json.loads((MONITORING/'versions.json').read_text(encoding='utf-8'))


def install_command(helm, chart):
    """The helm command that installs one pinned monitoring chart from versions.json."""
    return [*helm, 'upgrade', '--install', chart['release'], chart['chart'], '--repo', chart['repository'],
            '--version', chart['version'], '-n', chart['namespace'], '--create-namespace',
            '-f', str(MONITORING/chart['values']), '--wait', '--timeout', '5m']


def derive_key(secret, tenant):
    """The seeded gateway key of tenant, as seed.DeriveKey computes it."""
    mac = hmac.new(secret.encode(), tenant.encode(), hashlib.sha256).digest()
    return 'gf_' + base64.urlsafe_b64encode(mac).decode('ascii').rstrip('=')


def stream_count(target, maximum):
    """Concurrent streams for an average of just over target per replica at maximum."""
    return min(MAX_STREAMS, math.ceil(target*maximum) + 2)


class StreamLoad:
    """Keeps a number of mock-load streams open, each worker reopening when one ends."""

    def __init__(self, port, keys, streams):
        self.port, self.keys, self.streams = port, keys, streams
        self.body = json.dumps({'model': 'mock-load', 'max_tokens': 256, 'stream': True,
                                'messages': [{'role': 'user', 'content': 'x'*1024}]}).encode()
        self.lock = threading.Lock()
        self.stopping = threading.Event()
        self.threads = []
        self.open = 0
        self.counts = {'started': 0, 'completed': 0, 'failed': 0}

    def start(self):
        self.wait()
        self.stopping.clear()
        self.threads = [threading.Thread(target=self.worker, args=(self.keys[i % len(self.keys)],), daemon=True)
                        for i in range(self.streams)]
        for thread in self.threads:
            thread.start()

    def stop(self):
        """Sends no new requests and returns at once; open streams end on their own."""
        self.stopping.set()

    def wait(self, timeout=90):
        self.stopping.set()
        for thread in self.threads:
            thread.join(timeout)

    def active(self):
        with self.lock:
            return self.open

    def stats(self):
        with self.lock:
            return dict(self.counts)

    def worker(self, key):
        headers = {'content-type': 'application/json', 'anthropic-version': '2023-06-01', 'x-api-key': key}
        while not self.stopping.is_set():
            with self.lock:
                self.open += 1
                self.counts['started'] += 1
            connection = http.client.HTTPConnection('127.0.0.1', self.port, timeout=90)
            try:
                connection.request('POST', '/v1/messages', self.body, headers)
                response = connection.getresponse()
                text = response.read()
                ok = response.status == 200 and b'event: message_stop' in text and b'event: error' not in text
            except (OSError, http.client.HTTPException):
                ok = False
            finally:
                connection.close()
                with self.lock:
                    self.open -= 1
                    self.counts['completed' if ok else 'failed'] += 1
            if not ok:
                self.stopping.wait(1)


class Cluster:
    """Reads the gateway's replicas and HPA, and stops or starts Prometheus, via kubectl."""

    def __init__(self, kube, namespace, release, prometheus):
        self.kube, self.namespace, self.name = kube, namespace, release+'-gateway'
        self.prometheus = prometheus

    def get(self, namespace, kind, name):
        output = subprocess.run([*self.kube, '-n', namespace, 'get', kind, name, '-o', 'json'],
                                capture_output=True, text=True)
        return json.loads(output.stdout) if output.returncode == 0 else None

    def state(self):
        deployment = self.get(self.namespace, 'deployment', self.name)
        if deployment is None:
            raise RuntimeError('Deployment '+self.name+' not found')
        hpa = self.hpa() or {}
        status = hpa.get('status', {})
        return {'desired': deployment['spec'].get('replicas'),
                'ready': deployment.get('status', {}).get('readyReplicas', 0),
                'hpa_desired': status.get('desiredReplicas'),
                'metrics': [m.get('external', {}).get('current', {}).get('averageValue')
                            for m in status.get('currentMetrics') or []]}

    def hpa(self):
        # The ScaledObject names its HPA after the gateway Deployment.
        return self.get(self.namespace, 'hpa', self.name)

    def conditions(self):
        return [{k: c.get(k) for k in ('type', 'status', 'reason', 'message')}
                for c in (self.hpa() or {}).get('status', {}).get('conditions', [])]

    def scale_prometheus(self, replicas):
        namespace = self.prometheus['namespace']
        deployment = 'deployment/'+self.prometheus['release']+'-server'
        subprocess.run([*self.kube, '-n', namespace, 'scale', deployment, '--replicas', str(replicas)],
                       check=True, stdout=subprocess.DEVNULL)
        if replicas:
            subprocess.run([*self.kube, '-n', namespace, 'rollout', 'status', deployment, '--timeout=180s'], check=True)
            return
        for _ in range(90):
            current = self.get(namespace, 'deployment', self.prometheus['release']+'-server') or {}
            if not current.get('status', {}).get('replicas'):
                return
            time.sleep(2)
        raise RuntimeError('Prometheus did not stop')


def settled(samples, minimum):
    """The result of a phase that must end at minimum without going below it."""
    desired = [s['desired'] for s in samples]
    reached = next((s['t'] for s in samples if s['desired'] == minimum), None)
    return {'passed': bool(desired) and desired[-1] == minimum and min(desired) >= minimum,
            'final_desired': desired[-1] if desired else None,
            'seconds_to_min': None if reached is None else round(reached - samples[0]['t'], 1)}


def judge_baseline(samples, minimum, hold):
    """Passes when the last hold samples all have minimum desired and ready replicas.
    Fewer replicas before that do not count: an autoscaled gateway starts at one until
    the HPA raises it."""
    tail = samples[-hold:]
    steady = len(tail) == hold and all(s['desired'] == minimum and s['ready'] == minimum for s in tail)
    return {'passed': steady, 'final_desired': samples[-1]['desired'] if samples else None,
            'seconds_to_min': round(tail[0]['t'] - samples[0]['t'], 1) if steady else None}


def judge_scale_up(samples, minimum, maximum):
    desired = [s['desired'] for s in samples]
    peak = max(desired) if desired else None
    at_max = next((s['t'] for s in samples if s['desired'] >= maximum), None)
    return {'passed': bool(desired) and peak > minimum and min(desired) >= minimum,
            'max_desired': peak, 'reached_max': peak is not None and peak >= maximum,
            'seconds_to_max': None if at_max is None else round(at_max - samples[0]['t'], 1)}


def judge_outage(before, samples, minimum, conditions):
    """Prometheus went down while the gateway was above minimum: it must not scale down."""
    at_outage = before[-1]['desired'] if before else None
    desired = [s['desired'] for s in samples]
    lowest = min(desired) if desired else None
    started = at_outage is not None and at_outage > minimum
    return {'passed': started and bool(desired) and lowest >= at_outage,
            'desired_at_outage': at_outage, 'min_desired': lowest,
            'observed_seconds': round(samples[-1]['t'] - samples[0]['t'], 1) if samples else 0,
            'metrics_unavailable': any(c['type'] == 'ScalingActive' and c['status'] == 'False' for c in conditions),
            'hpa_conditions': conditions,
            'reason': None if started else 'gateway never rose above minReplicas before the outage'}


REQUIRED = ('baseline', 'scale_up', 'scale_down', 'missing_metrics')
# Consecutive samples at minReplicas, desired and ready, that make a stable baseline.
BASELINE_HOLD = 2


def rehearse(cluster, load, settings, clock=time.monotonic, sleep=time.sleep, log=print):
    """Runs the phases in order and returns the record. Prometheus is always restarted."""
    minimum, maximum, poll = settings['minReplicas'], settings['maxReplicas'], settings['poll']
    window = settings['scaleDownStabilizationSeconds']
    settle, rise = window + 180, settings['scaleUpTimeout']
    start = clock()
    record = {'samples': [], 'phases': {}}

    def watch(phase, seconds, until=None):
        seen, deadline = [], clock() + seconds
        while True:
            sample = {'t': round(clock() - start, 1), 'phase': phase, 'streams': load.active(), **cluster.state()}
            record['samples'].append(sample)
            seen.append(sample)
            if (until and until(sample)) or clock() >= deadline:
                return seen
            sleep(poll)

    def finish(phase, result):
        result['hpa_conditions'] = result.get('hpa_conditions') or cluster.conditions()
        record['phases'][phase] = result
        log(f"{phase}: {'passed' if result['passed'] else 'FAILED'}")

    steady = []

    def at_minimum(sample):
        steady.append(sample['desired'] == minimum and sample['ready'] == minimum)
        return steady[-BASELINE_HOLD:] == [True]*BASELINE_HOLD

    seen = watch('baseline', settle, at_minimum)
    finish('baseline', judge_baseline(seen, minimum, BASELINE_HOLD))

    load.start()
    try:
        seen = watch('scale_up', rise, lambda s: s['desired'] >= maximum)
    finally:
        load.stop()
    finish('scale_up', judge_scale_up(seen, minimum, maximum) | {'load': load.stats()})

    seen = watch('scale_down', settle, lambda s: s['desired'] == minimum)
    finish('scale_down', settled(seen, minimum) | {'window_seconds': settle})

    # Demand rises, Prometheus stops, then demand ends: missing data must not read as zero.
    load.start()
    stopped = False
    try:
        before = watch('missing_metrics_load', rise, lambda s: s['desired'] > minimum)
        if before[-1]['desired'] > minimum:
            stopped = True
            cluster.scale_prometheus(0)
        load.stop()
        seen = watch('missing_metrics', settle) if stopped else []
        finish('missing_metrics', judge_outage(before, seen, minimum, cluster.conditions() if stopped else []))
    finally:
        load.stop()
        if stopped:
            cluster.scale_prometheus(1)

    seen = watch('recovery', settle + 120, lambda s: s['desired'] == minimum)
    finish('recovery', settled(seen, minimum) | {'required': False})
    record['passed'] = all(record['phases'][p]['passed'] for p in REQUIRED)
    return record


def write_record(directory, record, now=None):
    now = now or datetime.datetime.now(datetime.timezone.utc)
    directory.mkdir(parents=True, exist_ok=True)
    path = directory/('autoscale-'+now.strftime('%Y%m%dT%H%M%SZ')+'.json')
    path.write_text(json.dumps(record, indent=2), encoding='utf-8')
    return path


def check(kube, helm, namespace, chart, values, port, seed_secret, output, node_image):
    """Installs monitoring, enables autoscaling, runs the phases and writes the record."""
    versions = load_versions()
    for name in ('prometheus', 'keda'):
        subprocess.run(install_command(helm, versions[name]), check=True)
    subprocess.run([*helm, 'upgrade', '--install', 'gf', str(chart), '-n', namespace, '-f', str(values),
                    '--set', 'autoscaling.enabled=true', '--wait', '--wait-for-jobs', '--timeout', '6m'], check=True)
    release = json.loads(subprocess.run([*helm, 'get', 'values', 'gf', '-n', namespace, '--all', '-o', 'json'],
                                        check=True, capture_output=True, text=True).stdout)
    settings = release['autoscaling'] | {'poll': 5, 'scaleUpTimeout': 300}
    keys = [derive_key(seed_secret, f'tenant-{i:02d}') for i in range(1, 11)]
    load = StreamLoad(port, keys, stream_count(settings['targetPerReplica'], settings['maxReplicas']))
    cluster = Cluster(kube, namespace, 'gf', versions['prometheus'])
    # Through ingress, as rehearse.py does, so each new stream can reach any gateway Pod.
    forward = subprocess.Popen([*kube, '-n', 'ingress-nginx', 'port-forward', 'service/ingress-nginx-controller', f'{port}:80'])
    try:
        time.sleep(3)
        record = rehearse(cluster, load, settings)
    finally:
        load.wait()
        forward.terminate()
        forward.wait(timeout=10)
    record = {'versions': {'charts': {k: v['version'] for k, v in versions.items()}, 'kind_node_image': node_image,
                           'gateflow_image_tag': release.get('imageTag')},
              'T': settings['targetPerReplica'], 'D': settings['requestDurationSeconds'],
              'settings': settings, 'streams': load.streams} | record
    path = write_record(output, record)
    print('Autoscaling record:', path)
    return 0 if record['passed'] else 1
