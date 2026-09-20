"""End-to-end shell protocol regression using disposable Linux containers."""
import json, os, pathlib, subprocess, time, urllib.request, uuid
import websocket
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
    def send(value): ws.send(json.dumps(value))
    command_id = str(uuid.uuid4())
    send({'action': 'shell_start', 'node_id': node_id, 'id': command_id, 'cols': 91, 'rows': 27})
    output = ''
    sent_input = False
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        frame = json.loads(ws.recv())
        if frame.get('id') != command_id:
            continue
        if frame.get('type') == 'error':
            raise AssertionError(frame)
        if frame.get('type') == 'shell_output':
            output += frame.get('data', '')
            if not sent_input:
                send({'action': 'shell_input', 'node_id': node_id, 'id': command_id,
                      'input': {'action': 'data', 'data': "stty size; printf '\\344'; sleep 0.1; printf '\\270\\255'; printf '\\n'; exit\n"}})
                sent_input = True
        if frame.get('type') == 'done':
            break
    else:
        raise AssertionError('shell did not finish')
    assert '27 91' in output, repr(output)
    assert '中' in output and '\ufffd' not in output, repr(output)
    # Reuse the finished shell ID immediately across native and subprocess paths.
    # Loopback is refused by the default native policy, giving a fast expected
    # failure without external traffic; ping exercises the local exec path.
    for attempt in range(20):
        kind = 'tcp' if attempt % 2 == 0 else 'ping'
        target = '127.0.0.1:1' if kind == 'tcp' else '127.0.0.1'
        send({'action': 'command', 'node_id': node_id,
              'command': {'id': command_id, 'type': kind, 'target': target,
                          'options': '' if kind == 'tcp' else 'count=1'}})
        frames = []
        while True:
            frame = json.loads(ws.recv())
            if frame.get('id') != command_id:
                continue
            frames.append(frame)
            assert 'already running' not in frame.get('data', ''), frames
            if frame.get('type') == 'done':
                break
        terminal = json.loads(frames[-1].get('data') or '{}')
        assert terminal.get('exit_ok') is (kind == 'ping'), frames
    result = {'initial_pty_dimensions': '27 rows x 91 columns', 'split_utf8': '中 preserved',
              'nonroot_linux_agent': True, 'immediate_id_reuses': 20, 'status': 'passed'}
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
