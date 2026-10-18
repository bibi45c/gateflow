"""Collect the cluster-side configuration and the A3 evidence of a cloud day, before teardown.

Operator-run (bibi45c only), read-only: kubectl, helm and aws describe calls, and three
GET requests through the ALB. Writes to --out, which must be ignored by git when it is
inside the repository (it holds load balancer, security group and node names, and with
--terraform-output the account's ARNs):

  helm-values-<release>.json  `helm get values -a` of each --helm-release, with the
                              value of every credential-like key replaced by <removed>
  helm-list.txt               `helm list -A`
  nodes.json, nodes.txt       per node: workload, instance type and zone labels, taints,
                              allocatable resources and Ready
  pods.txt                    every Pod of the cluster, wide
  images.json                 image and imageID of every container in --namespace
  init-and-restarts.json      per Pod: node, init container exit codes and restarts
  events-<namespace>.txt      the namespace's events, oldest first
  secret-keys.json            Secret names and key names; the values never reach this
                              process
  ingress.txt                 the namespace's Ingresses
  alb-security-groups.json    inbound rules of the security groups of the release's ALB
  alb-status-codes.json       HTTP status of /metrics, /healthz and /readyz through the ALB
  tf-output.json              a copy of --terraform-output
  collect-errors.json         each step that failed, with its error

Each --copy glob, relative to --from, copies the matching files and folders unchanged,
for example the day's plans, smoke results, live checks and database reset evidence.
A step that fails is recorded in collect-errors.json and the other steps still run:
the evidence can be collected only once, before teardown. Exit codes: 0 everything
collected, 1 a step failed or a --copy glob matched nothing (see collect-errors.json),
2 the output path is not ignored by git.
"""
import argparse
import json
from pathlib import Path
import re
import shutil
import subprocess
import sys
import urllib.error
import urllib.request

from common import REGION, ROOT, OperatorError, capture, kubectl, redact_urls, require_ignored

HELM_RELEASES = ('gf:gateflow', 'prometheus:monitoring', 'keda:keda', 'aws-load-balancer-controller:kube-system')
SECRETISH = re.compile(r'(password|secret|token|pepper|database_url|credential|key$)', re.IGNORECASE)
NODE_LABELS = ('gateflow.io', 'node.kubernetes.io/instance-type', 'topology.kubernetes.io')
ALB_PATHS = ('/metrics', '/healthz', '/readyz')
# Secret names and key names only: kubectl renders this, so no value is ever returned.
SECRET_KEYS = '{{range .items}}{{.metadata.name}}{{range $key, $value := .data}} {{$key}}{{end}}{{"\\n"}}{{end}}'


def scrub(value):
    """value without the values of credential-like keys, and without URL credentials."""
    if isinstance(value, dict):
        return {key: ('<removed>' if SECRETISH.search(key) and not isinstance(item, (dict, list)) else scrub(item))
                for key, item in value.items()}
    if isinstance(value, list):
        return [scrub(item) for item in value]
    if isinstance(value, str):
        return redact_urls(value)
    return value


def release(text):
    name, sep, namespace = text.partition(':')
    if not sep or not name or not namespace:
        raise argparse.ArgumentTypeError('expected RELEASE:NAMESPACE')
    return name, namespace


class Collector:
    def __init__(self, args, runner, urlopen):
        self.args, self.runner, self.urlopen = args, runner, urlopen
        self.out = args.out
        self.kube = kubectl(args.kubeconfig, args.kube_context)
        self.helm = ['helm', '--kubeconfig', str(args.kubeconfig), '--kube-context', args.kube_context]
        self.errors = {}
        self.host = None

    def run(self, command):
        return capture(self.runner, command)

    def write(self, name, data):
        text = data if isinstance(data, str) else json.dumps(data, indent=2) + '\n'
        (self.out/name).write_text(text, encoding='utf-8')

    def step(self, name, function):
        try:
            function()
        except Exception as error:  # recorded, never fatal: the evidence is collected once, before teardown
            self.errors[name] = f'{type(error).__name__}: {redact_urls(str(error))[:300]}'

    def helm_values(self):
        for name, namespace in self.args.helm_release or [release(item) for item in HELM_RELEASES]:
            values = json.loads(self.run([*self.helm, 'get', 'values', name, '-n', namespace, '-a', '-o', 'json']))
            self.write(f'helm-values-{name}.json', scrub(values))
        self.write('helm-list.txt', self.run([*self.helm, 'list', '-A']))

    def nodes(self):
        items = json.loads(self.run([*self.kube, 'get', 'nodes', '-o', 'json']))['items']
        self.write('nodes.json', [{
            'name': node['metadata']['name'],
            'labels': {key: value for key, value in node['metadata'].get('labels', {}).items() if key.startswith(NODE_LABELS)},
            'taints': node['spec'].get('taints', []), 'allocatable': node['status']['allocatable'],
            'ready': any(c['type'] == 'Ready' and c['status'] == 'True' for c in node['status'].get('conditions', []))}
            for node in items])
        self.write('nodes.txt', self.run([*self.kube, 'get', 'nodes', '-o', 'wide', '-L',
                                          'gateflow.io/workload,node.kubernetes.io/instance-type']))

    def pods(self):
        namespace = self.args.namespace
        self.write('pods.txt', self.run([*self.kube, 'get', 'pods', '-A', '-o', 'wide']))
        items = json.loads(self.run([*self.kube, '-n', namespace, 'get', 'pods', '-o', 'json']))['items']
        images = sorted({(c['image'], c.get('imageID', '')) for pod in items
                         for c in pod.get('status', {}).get('containerStatuses', [])})
        self.write('images.json', [{'image': image, 'imageID': digest} for image, digest in images])
        self.write('init-and-restarts.json', {pod['metadata']['name']: {
            'node': pod['spec'].get('nodeName'),
            'init_exit_codes': {c['name']: c.get('state', {}).get('terminated', {}).get('exitCode')
                                for c in pod.get('status', {}).get('initContainerStatuses', [])},
            'restarts': sum(c.get('restartCount', 0) for c in pod.get('status', {}).get('containerStatuses', []))}
            for pod in items})
        self.write(f'events-{namespace}.txt', self.run([*self.kube, '-n', namespace, 'get', 'events',
                                                        '--sort-by=.lastTimestamp']))

    def secrets(self):
        text = self.run([*self.kube, '-n', self.args.namespace, 'get', 'secrets', '-o', f'go-template={SECRET_KEYS}'])
        keys = {}
        for line in text.splitlines():
            if line.strip():
                name, *names = line.split()
                keys[name] = sorted(names)
        self.write('secret-keys.json', keys)

    def ingress(self):
        kube = [*self.kube, '-n', self.args.namespace]
        self.write('ingress.txt', self.run([*kube, 'get', 'ingress', '-o', 'wide']))
        host = self.run([*kube, 'get', 'ingress', self.args.release, '-o',
                         'jsonpath={.status.loadBalancer.ingress[0].hostname}']).strip()
        if not host:
            raise OperatorError(f'Ingress {self.args.release} has no load balancer hostname')
        self.host = host

    def alb(self):
        if not self.host:
            raise OperatorError('no ALB hostname: the ingress step failed')
        aws = ['--region', self.args.region, '--output', 'json']
        balancers = json.loads(self.run(['aws', 'elbv2', 'describe-load-balancers', *aws]))
        groups = [group for item in balancers.get('LoadBalancers', []) if item.get('DNSName') == self.host
                  for group in item.get('SecurityGroups', [])]
        if not groups:
            raise OperatorError(f'no load balancer with security groups has the DNS name {self.host}')
        rules = json.loads(self.run(['aws', 'ec2', 'describe-security-groups', '--group-ids', *groups, '--query',
                                     'SecurityGroups[].{name:GroupName,inbound:IpPermissions}', *aws]))
        self.write('alb-security-groups.json', rules)
        codes = {}
        for path in ALB_PATHS:
            try:
                with self.urlopen(f'http://{self.host}{path}', timeout=10) as response:
                    codes[path] = response.status
            except urllib.error.HTTPError as error:
                codes[path] = error.code
                error.close()
        self.write('alb-status-codes.json', codes)

    def terraform_output(self):
        shutil.copy2(self.args.terraform_output, self.out/'tf-output.json')

    def copy(self, pattern):
        out = self.out.resolve()

        def overlaps(path):  # the output folder itself, a path inside it, or a folder holding it
            path = path.resolve()
            return path == out or out in path.parents or path in out.parents
        matches = [path for path in sorted(self.args.source.glob(pattern)) if not overlaps(path)]
        if not matches:
            raise OperatorError(f'nothing in {self.args.source} matches {pattern}')
        for path in matches:
            if path.is_dir():
                shutil.copytree(path, self.out/path.name, dirs_exist_ok=True)
            else:
                shutil.copy2(path, self.out/path.name)

    def collect(self):
        for name, function in (('helm', self.helm_values), ('nodes', self.nodes), ('pods', self.pods),
                               ('secrets', self.secrets), ('ingress', self.ingress), ('alb', self.alb)):
            self.step(name, function)
        if self.args.terraform_output:
            self.step('terraform-output', self.terraform_output)
        for pattern in self.args.copy:
            self.step(f'copy {pattern}', lambda: self.copy(pattern))
        self.write('collect-errors.json', self.errors)
        return self.errors


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--kubeconfig', type=Path, required=True)
    parser.add_argument('--kube-context', required=True)
    parser.add_argument('--namespace', default='gateflow')
    parser.add_argument('--release', default='gf', help='GateFlow release, also the name of its Ingress (default: gf)')
    parser.add_argument('--region', default=REGION)
    parser.add_argument('--out', type=Path, required=True,
                        help='output folder, for example .local/cloud-day/day2/day2-config; must be ignored by git')
    parser.add_argument('--helm-release', type=release, action='append', metavar='RELEASE:NAMESPACE',
                        help=f'Helm release to save (repeatable; default: {", ".join(HELM_RELEASES)})')
    parser.add_argument('--terraform-output', type=Path, metavar='JSON', help='file of `terraform output -json` to copy')
    parser.add_argument('--from', dest='source', type=Path, default=ROOT/'.local'/'cloud-day',
                        help='folder of the --copy globs (default: .local/cloud-day)')
    parser.add_argument('--copy', action='append', default=[], metavar='GLOB',
                        help="files or folders to copy, for example 'live-d2-*.json' (repeatable)")
    return parser.parse_args(argv)


def main(argv=None, runner=subprocess.run, urlopen=urllib.request.urlopen):
    args = parse_args(argv)
    try:
        require_ignored(args.out, runner, 'output')
    except OperatorError as error:
        print(f'FAIL {error}', file=sys.stderr)
        return 2
    args.out.mkdir(parents=True, exist_ok=True)
    errors = Collector(args, runner, urlopen).collect()
    print(f'wrote {args.out}; errors: {", ".join(errors) if errors else "none"}')
    for name, error in errors.items():
        print(f'  {name}: {error}', file=sys.stderr)
    return 1 if errors else 0


if __name__ == '__main__':
    sys.exit(main())
