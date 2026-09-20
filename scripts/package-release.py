#!/usr/bin/env python3
"""Build portable Linux bundles without local paths or Git author metadata."""
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def main():
    arch = os.environ['RELEASE_ARCH']
    version = os.environ['RELEASE_VERSION']
    if arch not in {'amd64', 'arm64'} or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9.-]{0,99}', version):
        raise SystemExit('Invalid release architecture or version')
    if not (ROOT / 'frontend/dist/index.html').is_file():
        raise SystemExit('Build frontend/dist before packaging')
    output = ROOT / 'release-dist'
    output.mkdir(exist_ok=True)
    env = dict(os.environ, GOOS='linux', GOARCH=arch, CGO_ENABLED='0')
    epoch = int(subprocess.check_output(['git', 'show', '-s', '--format=%ct', 'HEAD'], cwd=ROOT))
    for component, module in [('server', 'backend'), ('agent', 'agent')]:
        name = f'parallax-{component}-{version}-linux-{arch}'
        with tempfile.TemporaryDirectory() as tmp:
            bundle = Path(tmp) / name
            bundle.mkdir()
            flags = '-s -w'
            if component == 'agent':
                flags += f' -X main.version={version}'
            subprocess.run(['go', 'build', '-trimpath', '-buildvcs=false', '-ldflags', flags, '-o', str(bundle / f'parallax-{component}'), '.'], cwd=ROOT / module, env=env, check=True)
            shutil.copy(ROOT / 'LICENSE', bundle / 'LICENSE')
            (bundle / 'README.txt').write_text(
                f'Parallax {component} {version} (Linux {arch})\n'
                'Documentation: https://github.com/EMOEMOJAI/Parallax/tree/main/docs\n'
                + ('Start ./parallax-server from this extracted directory to serve frontend/dist.\n' if component == 'server' else 'Run ./parallax-agent -h for connection and authentication options.\n'))
            if component == 'server':
                shutil.copytree(ROOT / 'frontend/dist', bundle / 'frontend/dist')
            def sanitize(info):
                info.uid = info.gid = 0
                info.uname = info.gname = ''
                info.mtime = epoch
                info.pax_headers = {}
                return info
            with tarfile.open(output / f'{name}.tar.gz', 'w:gz', format=tarfile.PAX_FORMAT) as archive:
                archive.add(bundle, arcname=name, filter=sanitize)
    print(f'Created Linux {arch} release bundles.')


if __name__ == '__main__':
    main()
