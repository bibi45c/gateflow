"""Install the pinned Trivy release into a local directory, after checking its SHA-256.

The checksums are pinned here, not fetched from the release, so a replaced release
asset fails the check. Update VERSION and SHA256 together, from the release's
trivy_<version>_checksums.txt, for a release at least two weeks old.

It tries the release download URL, then the GitHub API's asset URL, each up to three
times: the download host sometimes answers 504 while the API still serves the asset.
The pinned checksum makes either source safe. GITHUB_TOKEN, when set, authenticates
the API calls.

Usage: python tools/repo/install_trivy.py --directory .local/bin
"""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import tarfile
import time
import urllib.error
import urllib.request
import zipfile

VERSION = '0.76.1'
SHA256 = {
    'Linux-64bit.tar.gz': 'c7cbe3137fb8a48a17574bede3dd6beb16742d1e9772929940fc90082c39d9c0',
    'Linux-ARM64.tar.gz': 'e174dff2a6dba5e3ae41f8d0537a6324405bd08132497340304c6b20e0f16a1f',
    'macOS-64bit.tar.gz': '7aa762eacd582052d053b62dcf972a1ab058abe39a57ccae234ed8894945b6b7',
    'macOS-ARM64.tar.gz': '11a9b6cd2752011b54f01ea4bd0dbfa9f0c41dc6538d5a3f9d23955f471bf4e5',
    'windows-64bit.zip': '1051f49cb22d5284f43425f72aad9cda63ca12ceb92e7caa06903490c82dc9bf',
}


def asset(system, machine):
    """Return the release asset suffix for a platform.system() and platform.machine()."""
    os_name = {'Linux': 'Linux', 'Darwin': 'macOS', 'Windows': 'windows'}.get(system)
    arch = {'x86_64': '64bit', 'AMD64': '64bit', 'amd64': '64bit',
            'arm64': 'ARM64', 'aarch64': 'ARM64'}.get(machine)
    suffix = 'zip' if os_name == 'windows' else 'tar.gz'
    name = f'{os_name}-{arch}.{suffix}'
    if name not in SHA256:
        raise ValueError(f'no pinned Trivy build for {system} {machine}')
    return name


def verified(data, name):
    """Return data if its SHA-256 is the pinned one for the asset name."""
    if hashlib.sha256(data).hexdigest() != SHA256[name]:
        raise ValueError(f'checksum mismatch for trivy_{VERSION}_{name}')
    return data


def binary(archive, name):
    """Extract the trivy executable from a verified release archive."""
    if name.endswith('.zip'):
        with zipfile.ZipFile(io.BytesIO(archive)) as bundle:
            return bundle.read('trivy.exe')
    with tarfile.open(fileobj=io.BytesIO(archive)) as bundle:
        return bundle.extractfile('trivy').read()


def _get(url, accept=None):
    request = urllib.request.Request(url, headers={'User-Agent': 'gateflow-install-trivy'})
    if accept:
        request.add_header('Accept', accept)
    token = os.environ.get('GITHUB_TOKEN')
    if token and url.startswith('https://api.github.com/'):
        request.add_header('Authorization', 'Bearer ' + token)
    with urllib.request.urlopen(request, timeout=120) as response:
        return response.read()


def _api_asset(name):
    """Download the asset through the GitHub API, as `gh release download` does."""
    release = json.loads(_get(f'https://api.github.com/repos/aquasecurity/trivy/releases/tags/v{VERSION}'))
    wanted = f'trivy_{VERSION}_{name}'
    asset = next((a for a in release.get('assets', []) if a.get('name') == wanted), None)
    if asset is None:
        raise ValueError(f'release v{VERSION} has no asset {wanted}')
    return _get(asset['url'], accept='application/octet-stream')


def download(name, attempts=3, sleep=time.sleep):
    """Return the asset's bytes from the first source that answers, retrying each."""
    url = f'https://github.com/aquasecurity/trivy/releases/download/v{VERSION}/trivy_{VERSION}_{name}'
    errors = []
    for source in (lambda: _get(url), lambda: _api_asset(name)):
        for attempt in range(attempts):
            try:
                return source()
            except (urllib.error.URLError, TimeoutError, ValueError) as err:
                errors.append(str(err))
                sleep(2 * (attempt + 1))
    raise RuntimeError('could not download Trivy: ' + '; '.join(errors))


def install(directory, fetch=None):
    name = asset(platform.system(), platform.machine())
    data = verified(fetch(name) if fetch else download(name), name)
    directory.mkdir(parents=True, exist_ok=True)
    dest = directory / ('trivy.exe' if name.endswith('.zip') else 'trivy')
    dest.write_bytes(binary(data, name))
    dest.chmod(0o755)
    return dest


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--directory', type=Path, required=True)
    print(install(parser.parse_args().directory))
