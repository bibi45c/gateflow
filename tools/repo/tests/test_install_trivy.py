"""Unit tests of tools/repo/install_trivy.py; they download nothing."""
import hashlib
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock
import zipfile

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import install_trivy  # noqa: E402


class InstallTrivyTests(unittest.TestCase):
    def test_every_supported_platform_has_a_pinned_checksum(self):
        cases = {('Linux', 'x86_64'): 'Linux-64bit.tar.gz', ('Linux', 'aarch64'): 'Linux-ARM64.tar.gz',
                 ('Darwin', 'arm64'): 'macOS-ARM64.tar.gz', ('Darwin', 'x86_64'): 'macOS-64bit.tar.gz',
                 ('Windows', 'AMD64'): 'windows-64bit.zip'}
        for (system, machine), name in cases.items():
            with self.subTest(system=system, machine=machine):
                self.assertEqual(install_trivy.asset(system, machine), name)
                self.assertRegex(install_trivy.SHA256[name], r'^[0-9a-f]{64}$')

    def test_unknown_platform_is_refused(self):
        with self.assertRaises(ValueError):
            install_trivy.asset('Windows', 'ARM64')

    def test_a_changed_asset_fails_the_check(self):
        with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
            install_trivy.verified(b'not the release', 'Linux-64bit.tar.gz')

    def test_installs_only_a_verified_archive(self):
        buffer = io.BytesIO()
        with zipfile.ZipFile(buffer, 'w') as bundle:
            bundle.writestr('trivy.exe', b'binary')
        archive = buffer.getvalue()
        pinned = dict(install_trivy.SHA256, **{'windows-64bit.zip': hashlib.sha256(archive).hexdigest()})
        with tempfile.TemporaryDirectory() as tmp, \
                mock.patch.object(install_trivy, 'SHA256', pinned), \
                mock.patch('platform.system', return_value='Windows'), \
                mock.patch('platform.machine', return_value='AMD64'):
            names = []
            dest = install_trivy.install(Path(tmp) / 'bin', fetch=lambda name: names.append(name) or archive)
            self.assertEqual(dest.read_bytes(), b'binary')
            self.assertEqual(names, ['windows-64bit.zip'])

    def test_download_falls_back_to_the_api_after_retries(self):
        calls = []

        def get(url, accept=None):
            calls.append((url, accept))
            if url.startswith('https://github.com/'):
                raise install_trivy.urllib.error.HTTPError(url, 504, 'Gateway Time-out', None, None)
            if url.endswith(f'/releases/tags/v{install_trivy.VERSION}'):
                return json.dumps({'assets': [{'name': f'trivy_{install_trivy.VERSION}_Linux-64bit.tar.gz',
                                               'url': 'https://api.github.com/assets/1'}]}).encode()
            return b'archive'

        with mock.patch.object(install_trivy, '_get', get):
            data = install_trivy.download('Linux-64bit.tar.gz', sleep=lambda s: None)
        self.assertEqual(data, b'archive')
        self.assertEqual(sum(1 for url, _ in calls if url.startswith('https://github.com/')), 3)
        self.assertEqual(calls[-1], ('https://api.github.com/assets/1', 'application/octet-stream'))

    def test_download_gives_up_with_every_error(self):
        def get(url, accept=None):
            raise install_trivy.urllib.error.URLError('offline')

        with mock.patch.object(install_trivy, '_get', get), self.assertRaisesRegex(RuntimeError, 'offline'):
            install_trivy.download('Linux-64bit.tar.gz', sleep=lambda s: None)


if __name__ == '__main__':
    unittest.main()
