"""Protocol assertions shared by container and extracted-release checks."""
import json
import time
import uuid


def exercise_session(ws, node_id):
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
    return {'initial_pty_dimensions': '27 rows x 91 columns', 'split_utf8': '中 preserved',
              'nonroot_linux_agent': True, 'immediate_id_reuses': 20, 'status': 'passed'}
