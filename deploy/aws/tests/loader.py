"""Loads the deploy/aws scripts without putting deploy/aws on sys.path, where its
secrets.py would shadow the standard library module of that name."""
import importlib.util
from pathlib import Path
import subprocess
import sys

AWS = Path(__file__).resolve().parents[1]


def _exec(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def load(script):
    # The scripts import their helpers as `common`, as they do when run from deploy/aws.
    common = sys.modules.get('common')
    if common is None or Path(getattr(common, '__file__', '')).resolve() != AWS/'common.py':
        sys.modules['common'] = _exec('common', AWS/'common.py')
    return _exec(f'gateflow_aws_{script}', AWS/f'{script}.py')


def completed(command, returncode=0, stdout='', stderr=''):
    return subprocess.CompletedProcess(command, returncode, stdout, stderr)
