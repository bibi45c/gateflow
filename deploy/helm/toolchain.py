"""Install pinned official Helm/kind/kubectl binaries into a task-local directory.

Every download is retried. kind also falls back to the GitHub API's asset URL, which
keeps serving when the release download host answers 5xx or times out (#166). Each
binary is still checked against its published SHA-256, whatever the source.
GITHUB_TOKEN, when set, authenticates the API calls.
"""
import argparse
import hashlib
import http.client
import io
import json
import os
from pathlib import Path
import platform
import tarfile
import time
import urllib.error
import urllib.request

HELM='3.16.3'
KIND='0.33.0'
KUBECTL='1.35.0'


def get(url, accept=None):
    request=urllib.request.Request(url,headers={'User-Agent':'gateflow-toolchain'})
    if accept:
        request.add_header('Accept',accept)
    token=os.environ.get('GITHUB_TOKEN')
    if token and url.startswith('https://api.github.com/'):
        request.add_header('Authorization','Bearer '+token)
    with urllib.request.urlopen(request,timeout=120) as response:
        return response.read()


def github_asset(repo, tag, name):
    """Return a release asset through the GitHub API, as `gh release download` does."""
    release=json.loads(get(f'https://api.github.com/repos/{repo}/releases/tags/{tag}'))
    asset=next((a for a in release.get('assets',[]) if a.get('name')==name),None)
    if asset is None:
        raise ValueError(f'{repo} {tag} has no asset {name}')
    return get(asset['url'],accept='application/octet-stream')


def fetch(url, fallback=None, attempts=3, sleep=time.sleep):
    """Return url's bytes, retrying it and then fallback, each up to attempts times."""
    errors=[]
    for source in [lambda: get(url)]+([fallback] if fallback else []):
        for attempt in range(attempts):
            try:
                return source()
            except (urllib.error.URLError,http.client.HTTPException,TimeoutError,ConnectionError,ValueError) as err:
                errors.append(f'{type(err).__name__}: {err}')
                sleep(2*(attempt+1))
    raise RuntimeError(f'could not download {url}: '+'; '.join(errors))


def checked(url, checksum_url, fallback=None, checksum_fallback=None, sleep=time.sleep):
    data=fetch(url,fallback,sleep=sleep)
    expected=fetch(checksum_url,checksum_fallback,sleep=sleep).decode().split()[0].lower()
    if hashlib.sha256(data).hexdigest()!=expected:
        raise ValueError('Checksum mismatch for '+url)
    return data


def kind_asset(name):
    return lambda: github_asset('kubernetes-sigs/kind',f'v{KIND}',name)


def install(directory):
    system={'Windows':'windows','Darwin':'darwin','Linux':'linux'}[platform.system()]
    arch={'AMD64':'amd64','x86_64':'amd64','arm64':'arm64','aarch64':'arm64'}[platform.machine()]
    suffix='.exe' if system=='windows' else ''
    directory.mkdir(parents=True,exist_ok=True)
    kind=f'kind-{system}-{arch}'
    kind_url=f'https://github.com/kubernetes-sigs/kind/releases/download/v{KIND}/{kind}'
    kubectl_url=f'https://dl.k8s.io/release/v{KUBECTL}/bin/{system}/{arch}/kubectl{suffix}'
    for name,url,checksum,fallbacks in [
        ('kind',kind_url,kind_url+'.sha256sum',(kind_asset(kind),kind_asset(kind+'.sha256sum'))),
        ('kubectl',kubectl_url,kubectl_url+'.sha256',(None,None))]:
        dest=directory/(name+suffix)
        dest.write_bytes(checked(url,checksum,*fallbacks))
        dest.chmod(0o755)
    # Helm ships Windows as zip and Unix as tar; extract just the executable.
    ext='zip' if system=='windows' else 'tar.gz'
    url=f'https://get.helm.sh/helm-v{HELM}-{system}-{arch}.{ext}'
    archive=checked(url,url+'.sha256sum')
    if ext=='zip':
        import zipfile
        with zipfile.ZipFile(io.BytesIO(archive)) as bundle:
            binary=bundle.read(f'{system}-{arch}/helm.exe')
    else:
        with tarfile.open(fileobj=io.BytesIO(archive)) as bundle:
            binary=bundle.extractfile(f'{system}-{arch}/helm').read()
    dest=directory/('helm'+suffix)
    dest.write_bytes(binary)
    dest.chmod(0o755)


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--directory',type=Path,required=True)
    install(parser.parse_args().directory)
