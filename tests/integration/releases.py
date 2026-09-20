"""Extract release archives and exercise their binaries on the native Linux CPU."""
import json
import os
from pathlib import Path
import platform
import re
import signal
import socket
import struct
import subprocess
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
import uuid

import websocket
from protocol import exercise_session

ROOT = Path(__file__).resolve().parents[2]
RESULTS = ROOT / 'test-results/releases'


def unpack(archive, destination, machine):
    with tarfile.open(archive) as bundle:
        members = bundle.getmembers()
        for item in members:
            path = Path(item.name)
            if path.is_absolute() or '..' in path.parts or not (item.isfile() or item.isdir()):
                raise AssertionError('Unsafe archive member')
            assert item.uid == item.gid == 0 and not item.uname and not item.gname, 'Archive contains owner metadata'
        bundle.extractall(destination, filter='data')
    component = 'server' if '-server-' in archive.name else 'agent'
    executable = next(destination.glob(f'*/parallax-{component}'))
    with executable.open('rb') as file:
        header = file.read(20)
    assert header[:4] == b'\x7fELF' and header[5] == 1, 'Not a little-endian ELF binary'
    assert struct.unpack('<H', header[18:20])[0] == machine, 'Binary CPU differs from native runner'
    return executable


def main():
    arch = os.environ['RELEASE_ARCH']
    native = {'x86_64': ('amd64', 62), 'aarch64': ('arm64', 183)}.get(platform.machine())
    assert platform.system() == 'Linux' and native and native[0] == arch, 'Use a native Linux runner for this archive'
    assert os.geteuid() != 0, 'Release tests must run as an unprivileged user'
    RESULTS.mkdir(parents=True, exist_ok=True)
    keys = [uuid.uuid4().hex for _ in range(3)]
    client_key, agent_key, metrics_key = keys
    processes, logs = [], []
    ws = None
    with tempfile.TemporaryDirectory(prefix='parallax-release-') as tmp:
        temp = Path(tmp)
        try:
            executables = {}
            for component in ['server', 'agent']:
                archives = list((ROOT / 'release-dist').glob(f'parallax-{component}-*-linux-{arch}.tar.gz'))
                assert len(archives) == 1, 'Expected exactly one release archive per component and architecture'
                executables[component] = unpack(archives[0], temp / component, native[1])
            # The ephemeral port is local to this disposable test process.
            with socket.socket() as listener:
                listener.bind(('127.0.0.1', 0))
                port = listener.getsockname()[1]
            base = f'http://127.0.0.1:{port}'
            env = dict(os.environ)
            env.update({
                'PORT': str(port),
                'CLIENT_API_KEY': client_key,
                'AGENT_API_KEY': agent_key,
                'METRICS_TOKEN': metrics_key,
                'MESH_INTERVAL_SEC': '0',
                'SCHEDULES_FILE': str(temp / 'schedules.json'),
            })

            def start(component, args):
                log = tempfile.TemporaryFile(mode='w+b')
                logs.append((component, log))
                executable = executables[component]
                process = subprocess.Popen([str(executable), *args], cwd=executable.parent, env=env,
                                           stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
                processes.append(process)

            def request(path, key=None):
                headers = {'Authorization': 'Bearer ' + key} if key else {}
                with urllib.request.urlopen(urllib.request.Request(base + path, headers=headers), timeout=3) as response:
                    return response.read()

            def wait_for(predicate):
                for _ in range(100):
                    if any(process.poll() is not None for process in processes):
                        raise AssertionError('Released process exited unexpectedly')
                    try:
                        value = predicate()
                        if value:
                            return value
                    except (OSError, urllib.error.URLError):
                        pass
                    time.sleep(0.1)
                raise AssertionError('Released service did not become ready')

            start('server', [])
            wait_for(lambda: request('/api/nodes', client_key))
            html = request('/').decode()
            assert 'Parallax' in html
            assets = re.findall(r'(?:src|href)="(/assets/[^\"]+)"', html)
            assert assets, 'Archive dashboard does not reference built assets'
            for asset in assets:
                assert request(asset), 'Packaged dashboard asset missing'
            assert b'Parallax' in request('/llms.txt')
            for path in ['/api/nodes', '/metrics']:
                try:
                    request(path)
                except urllib.error.HTTPError as error:
                    assert error.code in (401, 403), 'Unexpected authentication status'
                else:
                    raise AssertionError('Anonymous private endpoint was accepted')
            assert b'lookingglass_' in request('/metrics', metrics_key)
            start('agent', ['-server', f'ws://127.0.0.1:{port}/ws/agent', '-name', 'Release-test', '-auto-ip=false', '-allow-shell=true'])
            nodes = wait_for(lambda: [node for node in json.loads(request('/api/nodes', client_key)) if node['online']])
            assert nodes[0].get('version') == os.environ['RELEASE_VERSION'], 'Agent build version is incorrect'
            ws = websocket.create_connection(f'ws://127.0.0.1:{port}/ws/client',
                                            header=['Authorization: Bearer ' + client_key], timeout=10)
            result = exercise_session(ws, nodes[0]['id'])
            result.update(architecture=arch, native_execution=True, packaged_dashboard=True, authentication=True)
            (RESULTS / f'{arch}.json').write_text(json.dumps(result, indent=2) + '\n')
            print(json.dumps(result))
        finally:
            if ws:
                ws.close()
            for process in reversed(processes):
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGTERM)
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        os.killpg(process.pid, signal.SIGKILL)
                        process.wait(timeout=5)
            for component, log in logs:
                log.seek(0)
                text = log.read().decode(errors='replace')
                for key in keys:
                    text = text.replace(key, '[REDACTED]')
                (RESULTS / f'{arch}-{component}.log').write_text(text)
                log.close()


if __name__ == '__main__':
    main()
