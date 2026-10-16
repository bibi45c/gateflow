"""Prepare disposable kind fixtures and verify a FlowGate Helm installation.

Requires kind, kubectl, helm and Docker on PATH. Never uses the default kubeconfig.
"""
import argparse
import base64
import json
from pathlib import Path
import secrets
import subprocess
import sys

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
NODE_IMAGE = 'kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0'
INGRESS = 'https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.15.1/deploy/static/provider/kind/deploy.yaml'
CREDENTIAL_KEYS = ['admin','superuser','access','metering','tokenAccess','tokenMetering','pepper']
# Created by kind/postgres.yaml like the RDS master role: LOGIN CREATEROLE, database owner, not a superuser.
BOOTSTRAP_USER = 'gateflow_admin'


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


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cluster', required=True)
    parser.add_argument('--kubeconfig', type=Path, required=True)
    parser.add_argument('--namespace', default='gateflow')
    parser.add_argument('--prepare-only', action='store_true')
    parser.add_argument('--existing-cluster', action='store_true')
    args=parser.parse_args(argv)
    kube=kube_command(args.cluster,args.kubeconfig)
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
    # Port-forward the ingress controller, not the gateway, so requests exercise Ingress rules.
    forward=subprocess.Popen([*kube,'-n','ingress-nginx','port-forward','service/ingress-nginx-controller','18071:80'])
    try:
        import time
        import urllib.request
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
        run([sys.executable,str(ROOT/'test/e2e/smoke.py'),'--kube-context','kind-'+args.cluster,
             '--namespace',args.namespace,'--postgres-pod','postgres-0','--port','18071'],
            env=smoke_environment(dict(__import__('os').environ), args.kubeconfig))
    finally:
        forward.terminate()
        forward.wait(timeout=10)
    return 0


if __name__=='__main__': sys.exit(main())
