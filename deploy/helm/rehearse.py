"""Prepare disposable kind fixtures and verify a FlowGate Helm installation.

Requires kind, kubectl, helm and Docker on PATH. Never uses the default kubeconfig.
"""
import argparse
import base64
import contextlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
NODE_IMAGE = 'kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0'
INGRESS = 'https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.15.1/deploy/static/provider/kind/deploy.yaml'
CREDENTIAL_KEYS = ['admin','superuser','access','metering','tokenAccess','tokenMetering','pepper']
# Created by kind/postgres.yaml like the RDS master role: LOGIN CREATEROLE, database owner, not a superuser.
BOOTSTRAP_USER = 'gateflow_admin'
# A published gateway image built from 75da74a, older than the chart's default imageTag.
DEPLOY_CHECK_SERVICE = 'gateway'
DEPLOY_CHECK_DIGEST = 'sha256:daa59ab42b40fa40ec45579ec12dcacf6ed953bd1f509d2aebce1fdd1c74f7db'


def kube_command(cluster, config, *args):
    if not cluster.startswith('gateflow') or not cluster.replace('-', '').isalnum():
        raise ValueError('Use a dedicated cluster name beginning with gateflow')
    return ['kubectl', '--kubeconfig', str(config), '--context', 'kind-'+cluster, *args]


def run(command, **kwargs):
    subprocess.run(command, check=True, **kwargs)


def apply(kube, namespace, document):
    run([*kube, '-n', namespace, 'apply', '-f', '-'], input=json.dumps(document), text=True,
        stdout=subprocess.DEVNULL)


def secret(kube, namespace, name, values):
    apply(kube, namespace, {'apiVersion':'v1','kind':'Secret','metadata':{'name':name},
                           'type':'Opaque','data':{k:base64.b64encode(v.encode()).decode() for k,v in values.items()}})


def smoke_environment(environment, config):
    return environment | {'KUBECONFIG':str(config), 'ACCESS_SEED_SECRET':'seed-secret-local-only'}


def load_credentials(path):
    # Preserve generated credentials across repeat runs and add keys missing from older files.
    keys=json.loads(path.read_text(encoding='utf-8')) if path.exists() else {}
    missing=[k for k in CREDENTIAL_KEYS if k not in keys]
    if missing:
        keys|={k:secrets.token_hex(24) for k in missing}
        path.write_text(json.dumps(keys),encoding='utf-8')
    return keys


def bootstrap_values(keys):
    return {'PGHOST':'postgres','PGPORT':'5432','PGDATABASE':'gateflow','PGUSER':BOOTSTRAP_USER,'PGPASSWORD':keys['admin'],
        'PGSSLMODE':'disable','ACCESS_PASSWORD':keys['access'],'METERING_PASSWORD':keys['metering']}


@contextlib.contextmanager
def ingress_forward(kube):
    # Port-forward the ingress controller, not the gateway, so requests exercise Ingress rules.
    forward=subprocess.Popen([*kube,'-n','ingress-nginx','port-forward','service/ingress-nginx-controller','18071:80'])
    try:
        for attempt in range(60):
            try:
                urllib.request.urlopen('http://127.0.0.1:18071/metrics',timeout=2)
            except urllib.error.HTTPError as error:
                if error.code==404: break
                raise
            except OSError:
                time.sleep(1)
        else:
            raise RuntimeError('Ingress did not reject /metrics with 404')
        yield
    finally:
        forward.terminate()
        forward.wait(timeout=10)


def changed(record):
    before,after=record.get('images_before') or {},record.get('images_after') or {}
    return {name for name in before.keys()|after.keys() if before.get(name)!=after.get(name)}


def deploy_check_problems(deployed, rolled_back, service, digest):
    """Problems with the records of a one-service deploy and its rollback."""
    problems=[]
    for label,record,operation in (('deploy',deployed,'deploy'),('rollback',rolled_back,'rollback')):
        if record.get('operation')!=operation or record.get('service')!=service:
            problems.append(f"{label} record is {record.get('operation')} of {record.get('service')}")
        if record.get('error') or not record.get('verified') or record.get('smoke',{}).get('result')!='passed':
            problems.append(f"{label} record is not verified: {record.get('error')}, smoke {record.get('smoke',{}).get('result')}")
        if changed(record)!={service}:
            problems.append(f'{label} changed images of {sorted(changed(record))}, not only {service}')
    if deployed.get('new_digest')!=digest or not (deployed.get('images_after') or {}).get(service,'').endswith('@'+digest):
        problems.append(f"deploy record did not move {service} to {digest}")
    if rolled_back.get('new_digest')!=deployed.get('previous_digest'):
        problems.append('rollback did not restore the previous digest')
    if rolled_back.get('images_before')!=deployed.get('images_after') or rolled_back.get('images_after')!=deployed.get('images_before'):
        problems.append('rollback did not return every image to its state before the deploy')
    return problems


def deploy_check(args, kube):
    """Move one service with deploy.py, smoke, roll back from the record, smoke again."""
    folder=args.kubeconfig.parent/'deployments'
    folder.mkdir(parents=True,exist_ok=True)
    deployed,rolled_back=folder/'rehearsal-deploy.json',folder/'rehearsal-rollback.json'
    for path in (deployed,rolled_back): path.unlink(missing_ok=True)
    common=[sys.executable,str(HERE/'deploy.py'),'--kubeconfig',str(args.kubeconfig),'--kube-context','kind-'+args.cluster,
            '--namespace',args.namespace,'-f',str(HERE/'values-kind.yaml'),'--smoke-port','18071','--smoke-postgres-pod','postgres-0']
    environment=smoke_environment(dict(os.environ), args.kubeconfig)
    with ingress_forward(kube):
        run([*common,'--service',DEPLOY_CHECK_SERVICE,'--digest',DEPLOY_CHECK_DIGEST,'--record',str(deployed)],env=environment)
        run([*common,'--rollback-from',str(deployed),'--record',str(rolled_back)],env=environment)
    problems=deploy_check_problems(json.loads(deployed.read_text(encoding='utf-8')),
                                   json.loads(rolled_back.read_text(encoding='utf-8')),DEPLOY_CHECK_SERVICE,DEPLOY_CHECK_DIGEST)
    if problems:
        raise RuntimeError('; '.join(problems))
    print(f'Deployed and rolled back {DEPLOY_CHECK_SERVICE} only; records in {folder}')
    return 0


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cluster', required=True)
    parser.add_argument('--kubeconfig', type=Path, required=True)
    parser.add_argument('--namespace', default='gateflow')
    parser.add_argument('--prepare-only', action='store_true')
    parser.add_argument('--existing-cluster', action='store_true')
    parser.add_argument('--deploy-check', action='store_true',
                        help='on an installed cluster, deploy one gateway digest with deploy.py and roll it back')
    args=parser.parse_args(argv)
    if args.deploy_check and (not args.existing_cluster or args.prepare_only):
        parser.error('--deploy-check needs --existing-cluster with an installed release')
    kube=kube_command(args.cluster,args.kubeconfig)
    if args.deploy_check:
        return deploy_check(args,kube)
    args.kubeconfig.parent.mkdir(parents=True,exist_ok=True)
    if not args.existing_cluster:
        run(['kind','create','cluster','--name',args.cluster,'--kubeconfig',str(args.kubeconfig),
             '--config',str(HERE/'kind/config.yaml'),'--image',NODE_IMAGE,'--wait','180s'])
    apply(kube,args.namespace,{'apiVersion':'v1','kind':'Namespace','metadata':{'name':args.namespace}})
    # Values stay outside the repo's tracked files.
    keys=load_credentials(args.kubeconfig.with_suffix('.credentials.json'))
    secret(kube,args.namespace,'gateflow-postgres',{'POSTGRES_PASSWORD':keys['superuser']})
    secret(kube,args.namespace,'gateflow-bootstrap',bootstrap_values(keys))
    secret(kube,args.namespace,'gateflow-runtime',{'ACCESS_DATABASE_URL':f"postgres://access:{keys['access']}@postgres:5432/gateflow?sslmode=disable",
        'METERING_DATABASE_URL':f"postgres://metering:{keys['metering']}@postgres:5432/gateflow?sslmode=disable",
        'ACCESS_INTERNAL_TOKEN':keys['tokenAccess'],'METERING_INTERNAL_TOKEN':keys['tokenMetering'],
        'ACCESS_KEY_PEPPER':keys['pepper'],'ACCESS_SEED_SECRET':'seed-secret-local-only'})
    run([*kube,'-n',args.namespace,'apply','-f',str(HERE/'kind/postgres.yaml')])
    run([*kube,'-n',args.namespace,'wait','--for=condition=Ready','pod/postgres-0','--timeout=180s'])
    run([*kube,'apply','-f',INGRESS])
    run([*kube,'-n','ingress-nginx','rollout','status','deployment/ingress-nginx-controller','--timeout=240s'])
    if args.prepare_only:
        return 0
    helm=['helm','--kubeconfig',str(args.kubeconfig),'--kube-context','kind-'+args.cluster]
    run([*helm,'upgrade','--install','gf',str(HERE/'gateflow'),'-n',args.namespace,'-f',str(HERE/'values-kind.yaml'),
         '--wait','--wait-for-jobs','--timeout','6m'])
    with ingress_forward(kube):
        run([sys.executable,str(ROOT/'test/e2e/smoke.py'),'--kube-context','kind-'+args.cluster,
             '--namespace',args.namespace,'--postgres-pod','postgres-0','--port','18071'],
            env=smoke_environment(dict(os.environ), args.kubeconfig))
    return 0


if __name__=='__main__': sys.exit(main())
