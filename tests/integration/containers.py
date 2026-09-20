"""End-to-end shell protocol regression using disposable Linux containers."""
import json, os, pathlib, subprocess, time, urllib.request, uuid
import websocket
from protocol import exercise_session
ROOT = pathlib.Path(os.environ.get('TEST_RESULTS_DIR', 'test-results/integration'))
ROOT.mkdir(parents=True, exist_ok=True)
PREFIX = 'parallax-ci-' + uuid.uuid4().hex[:8]
CLIENT_KEY = uuid.uuid4().hex
AGENT_KEY = uuid.uuid4().hex
METRICS_KEY = uuid.uuid4().hex
SERVER_IMAGE = os.environ.get('SERVER_IMAGE', 'parallax-server:ci')
AGENT_IMAGE = os.environ.get('AGENT_IMAGE', 'parallax-agent:ci')
def docker(*args):
    return subprocess.check_output(['docker', *args], text=True).strip()
ws = None
try:
    docker('network', 'create', PREFIX)
    docker('run', '-d', '--name', PREFIX + '-server', '--network', PREFIX,
           '--network-alias', 'server', '-p', '127.0.0.1::8080',
           '-e', 'CLIENT_API_KEY=' + CLIENT_KEY,
           '-e', 'AGENT_API_KEY=' + AGENT_KEY,
           '-e', 'MESH_INTERVAL_SEC=0', '-e', 'METRICS_TOKEN=' + METRICS_KEY, SERVER_IMAGE)
    port = docker('port', PREFIX + '-server', '8080/tcp').rsplit(':', 1)[1]
    base = 'http://127.0.0.1:' + port
    def nodes():
        req = urllib.request.Request(base + '/api/nodes', headers={'Authorization': 'Bearer ' + CLIENT_KEY})
        with urllib.request.urlopen(req, timeout=3) as response:
            return json.load(response)
    for _ in range(40):
        try:
            nodes(); break
        except Exception:
            time.sleep(.25)
    else:
        raise RuntimeError('server startup timed out')
    docker('run', '-d', '--name', PREFIX + '-agent', '--network', PREFIX,
           '-e', 'AGENT_API_KEY=' + AGENT_KEY,
           AGENT_IMAGE, '-server', 'ws://server:8080/ws/agent',
           '-name', 'Integration', '-auto-ip=false', '-allow-shell=true')
    for _ in range(40):
        inventory = nodes()
        if inventory and inventory[0]['online']:
            break
        time.sleep(.25)
    else:
        raise RuntimeError('agent registration timed out')
    # Verify the shipped images really use unprivileged users.
    for role in ['server', 'agent']:
        assert docker('exec', PREFIX + '-' + role, 'id', '-u') != '0'
    # The production server must serve the UI and deny anonymous metrics.
    with urllib.request.urlopen(base, timeout=3) as response:
        assert b'Parallax' in response.read()
    try:
        urllib.request.urlopen(base + '/metrics', timeout=3)
    except urllib.error.HTTPError as error:
        assert error.code in (401, 403), error.code
    else:
        raise AssertionError('anonymous metrics request was accepted')
    req = urllib.request.Request(base + '/metrics', headers={'Authorization': 'Bearer ' + METRICS_KEY})
    with urllib.request.urlopen(req, timeout=3) as response:
        assert b'lookingglass_' in response.read()
    node_id = inventory[0]['id']
    ws = websocket.create_connection('ws://127.0.0.1:' + port + '/ws/client',
        header=['Authorization: Bearer ' + CLIENT_KEY], timeout=10)
    result = exercise_session(ws, node_id)
    (ROOT / 'shell-integration.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result))
finally:
    if ws:
        ws.close()
    for role in ['agent', 'server']:
        name = PREFIX + '-' + role
        log = subprocess.run(['docker', 'logs', name], capture_output=True, text=True)
        (ROOT / ('shell-integration-' + role + '.log')).write_text((log.stdout + log.stderr).replace(CLIENT_KEY, '[REDACTED]').replace(AGENT_KEY, '[REDACTED]').replace(METRICS_KEY, '[REDACTED]'))
        subprocess.run(['docker', 'rm', '-f', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    subprocess.run(['docker', 'network', 'rm', PREFIX], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
