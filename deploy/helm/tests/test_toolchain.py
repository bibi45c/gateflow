"""Unit tests of deploy/helm/toolchain.py's downloads (#166); they use no network."""
import hashlib
import importlib.util
import json
from pathlib import Path
import unittest
from unittest import mock
import urllib.error

HELM = Path(__file__).resolve().parents[1]

def load_toolchain():
    spec=importlib.util.spec_from_file_location('toolchain',HELM/'toolchain.py')
    module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
    return module

toolchain=load_toolchain()
NO_SLEEP=lambda seconds: None

def http_error(url, code):
    return urllib.error.HTTPError(url, code, 'error', None, None)

class FetchTests(unittest.TestCase):
    def test_a_transient_error_is_retried(self):
        replies=[http_error('u',503), TimeoutError('read timed out'), b'data']
        def get(url, accept=None):
            reply=replies.pop(0)
            if isinstance(reply, Exception):
                raise reply
            return reply
        with mock.patch.object(toolchain,'get',get):
            self.assertEqual(toolchain.fetch('https://example/x', sleep=NO_SLEEP), b'data')
        self.assertEqual(replies, [])

    def test_kind_falls_back_to_the_api_asset(self):
        calls=[]
        def get(url, accept=None):
            calls.append((url, accept))
            if url.startswith('https://github.com/'):
                raise http_error(url, 504)
            if url.endswith('/releases/tags/v0.33.0'):
                return json.dumps({'assets':[{'name':'kind-linux-amd64','url':'https://api.github.com/assets/7'}]}).encode()
            return b'kind-binary'
        url='https://github.com/kubernetes-sigs/kind/releases/download/v0.33.0/kind-linux-amd64'
        with mock.patch.object(toolchain,'get',get):
            data=toolchain.fetch(url, toolchain.kind_asset('kind-linux-amd64'), sleep=NO_SLEEP)
        self.assertEqual(data, b'kind-binary')
        self.assertEqual(sum(1 for u,_ in calls if u.startswith('https://github.com/')), 3)
        self.assertEqual(calls[-1], ('https://api.github.com/assets/7','application/octet-stream'))

    def test_the_final_error_lists_every_attempt(self):
        def get(url, accept=None):
            raise http_error(url, 503)
        with mock.patch.object(toolchain,'get',get), self.assertRaises(RuntimeError) as caught:
            toolchain.fetch('https://example/x', lambda: get('fallback'), sleep=NO_SLEEP)
        self.assertEqual(str(caught.exception).count('HTTPError'), 6)

    def test_the_checksum_is_enforced_on_a_fallback_too(self):
        good=hashlib.sha256(b'real').hexdigest()
        def get(url, accept=None):
            if url.endswith('.sha256sum'):
                return f'{good}  kind'.encode()
            raise http_error(url, 503)
        with mock.patch.object(toolchain,'get',get):
            with self.assertRaisesRegex(ValueError,'Checksum mismatch'):
                toolchain.checked('https://example/kind','https://example/kind.sha256sum', lambda: b'tampered', sleep=NO_SLEEP)
            self.assertEqual(toolchain.checked('https://example/kind','https://example/kind.sha256sum', lambda: b'real', sleep=NO_SLEEP), b'real')

if __name__=='__main__':
    unittest.main()
