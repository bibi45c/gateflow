import importlib.util
from pathlib import Path
import unittest

class RehearsalTests(unittest.TestCase):
    def test_context_is_mandatory_and_cluster_is_dedicated(self):
        path=Path(__file__).resolve().parents[1]/'rehearse.py'
        spec=importlib.util.spec_from_file_location('rehearse',path)
        module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
        self.assertEqual(module.kube_command('gateflow71',Path('test-config'),'get','pods')[:5],['kubectl','--kubeconfig','test-config','--context','kind-gateflow71'])
        with self.assertRaises(ValueError): module.kube_command('production',Path('config'),'get','pods')
