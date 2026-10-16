import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

HELM = Path(__file__).resolve().parents[1]

def load_rehearse():
    spec=importlib.util.spec_from_file_location('rehearse',HELM/'rehearse.py')
    module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
    return module

def kind_postgres():
    return [json.loads(doc) for doc in (HELM/'kind/postgres.yaml').read_text(encoding='utf-8').split('\n---\n')]

class RehearsalTests(unittest.TestCase):
    def test_context_is_mandatory_and_cluster_is_dedicated(self):
        path=Path(__file__).resolve().parents[1]/'rehearse.py'
        spec=importlib.util.spec_from_file_location('rehearse',path)
        module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
        self.assertEqual(module.kube_command('gateflow71',Path('test-config'),'get','pods')[:5],['kubectl','--kubeconfig','test-config','--context','kind-gateflow71'])
        with self.assertRaises(ValueError): module.kube_command('production',Path('config'),'get','pods')

    def test_smoke_seed_matches_fixture(self):
        path=Path(__file__).resolve().parents[1]/'rehearse.py'
        spec=importlib.util.spec_from_file_location('rehearse',path)
        module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
        env=module.smoke_environment({'ACCESS_SEED_SECRET':'unrelated-compose-secret'},Path('local-config'))
        self.assertEqual(env['ACCESS_SEED_SECRET'],'seed-secret-local-only')
        self.assertEqual(env['KUBECONFIG'],'local-config')

    def test_bootstrap_runs_as_non_superuser_database_owner(self):
        module=load_rehearse()
        values=module.bootstrap_values({k:'unused' for k in module.CREDENTIAL_KEYS})
        docs=kind_postgres()
        pod=next(d for d in docs if d['kind']=='Pod')
        container=pod['spec']['containers'][0]
        env={e['name']:e for e in container['env']}
        self.assertNotEqual(values['PGUSER'],env['POSTGRES_USER']['value'])
        script=next(d for d in docs if d['kind']=='ConfigMap')['data']['10-gateflow-admin.sh']
        self.assertIn(f"CREATE ROLE {values['PGUSER']} LOGIN CREATEROLE NOSUPERUSER ",script)
        self.assertIn(f"ALTER DATABASE {values['PGDATABASE']} OWNER TO {values['PGUSER']};",script)
        self.assertEqual(env['GATEFLOW_ADMIN_PASSWORD']['valueFrom']['secretKeyRef'],{'name':'gateflow-bootstrap','key':'PGPASSWORD'})
        self.assertNotEqual(env['POSTGRES_PASSWORD']['valueFrom']['secretKeyRef']['name'],'gateflow-bootstrap')
        self.assertEqual(container['volumeMounts'][0]['mountPath'],'/docker-entrypoint-initdb.d')
        self.assertTrue(all('valueFrom' in e for name,e in env.items() if 'PASSWORD' in name))

    def test_old_credentials_are_completed_and_preserved(self):
        module=load_rehearse()
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'kubeconfig.credentials.json'
            old={k:k+'-old' for k in ['admin','access','metering','tokenAccess','tokenMetering','pepper']}
            path.write_text(json.dumps(old),encoding='utf-8')
            keys=module.load_credentials(path)
            self.assertEqual(set(keys),set(module.CREDENTIAL_KEYS))
            self.assertEqual({k:keys[k] for k in old},old)
            self.assertNotEqual(keys['superuser'],keys['admin'])
            self.assertEqual(module.load_credentials(path),keys)
            self.assertEqual(json.loads(path.read_text(encoding='utf-8')),keys)

    def test_new_credentials_are_random_and_persisted(self):
        module=load_rehearse()
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'kubeconfig.credentials.json'
            keys=module.load_credentials(path)
            self.assertEqual(set(keys),set(module.CREDENTIAL_KEYS))
            self.assertEqual(len(set(keys.values())),len(keys))
            self.assertEqual(module.load_credentials(path),keys)
