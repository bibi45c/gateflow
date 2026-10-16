"""Install pinned official Helm/kind/kubectl binaries into a task-local directory."""
import argparse
import hashlib
import io
from pathlib import Path
import platform
import tarfile
import urllib.request

HELM='3.16.3'
KIND='0.33.0'
KUBECTL='1.35.0'


def fetch(url):
    with urllib.request.urlopen(url,timeout=120) as response:
        return response.read()


def checked(url, checksum_url):
    data=fetch(url)
    expected=fetch(checksum_url).decode().split()[0].lower()
    if hashlib.sha256(data).hexdigest()!=expected:
        raise ValueError('Checksum mismatch for '+url)
    return data


def install(directory):
    system={'Windows':'windows','Darwin':'darwin','Linux':'linux'}[platform.system()]
    arch={'AMD64':'amd64','x86_64':'amd64','arm64':'arm64','aarch64':'arm64'}[platform.machine()]
    suffix='.exe' if system=='windows' else ''
    directory.mkdir(parents=True,exist_ok=True)
    for name,url,checksum in [
        ('kind',f'https://github.com/kubernetes-sigs/kind/releases/download/v{KIND}/kind-{system}-{arch}',f'https://github.com/kubernetes-sigs/kind/releases/download/v{KIND}/kind-{system}-{arch}.sha256sum'),
        ('kubectl',f'https://dl.k8s.io/release/v{KUBECTL}/bin/{system}/{arch}/kubectl{suffix}',f'https://dl.k8s.io/release/v{KUBECTL}/bin/{system}/{arch}/kubectl{suffix}.sha256')]:
        dest=directory/(name+suffix)
        dest.write_bytes(checked(url,checksum))
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
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--directory',type=Path,required=True)
    install(parser.parse_args().directory)
