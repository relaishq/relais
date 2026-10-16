#!/usr/bin/env bash
# Run only extracted programs. Python coordinates the test; Go is unreachable.
set -euo pipefail
[[ $# == 1 ]] || { echo "usage: $0 <extracted-dir>" >&2; exit 2; }
archive=$(cd "$1" && pwd)
logs=${INSTALL_TEST_LOGS:-$(mktemp -d "${TMPDIR:-/tmp}/relais-install-test.XXXXXX")}
mkdir -p "$logs/tools"
logs=$(cd "$logs" && pwd)
# Use a small, explicit tool directory even on hosts with Go in /usr/bin.
for tool in bash sh env python3 redis-server openssl curl ps nohup mkdir chmod cat awk grep sleep mv rmdir dirname; do
  path=$(command -v "$tool") || { echo "missing dependency: $tool" >&2; exit 1; }
  # pyenv/asdf shims need their own PATH; link the actual interpreter.
  if [[ $tool == python3 ]]; then path=$(python3 -c 'import sys; print(sys.executable)'); fi
  ln -sf "$path" "$logs/tools/$tool"
done
export PATH="$logs/tools"
if command -v go >/dev/null 2>&1; then echo 'Go must be absent from PATH' >&2; exit 1; fi
printf 'Go-free PATH verified; logs: %s\n' "$logs"
export INSTALL_ARCHIVE="$archive" INSTALL_LOGS="$logs"
python3 - <<'PY'
import datetime
import json
import os
import re
from pathlib import Path
import signal
import socket
import subprocess
import time
import urllib.error
import urllib.request

archive = Path(os.environ['INSTALL_ARCHIVE'])
logs = Path(os.environ['INSTALL_LOGS'])
state = logs / 'topology'
redis_port = int(os.environ.get('REDIS_PORT', '16380'))
base = int(os.environ.get('PORT_BASE', '34700'))
assert 0 < redis_port <= 65535 and redis_port != 6379, '6379 is reserved'
ctrl = f'http://127.0.0.1:{base+10}'
# Prevent proxy environment variables from redirecting local checks.
http = urllib.request.build_opener(urllib.request.ProxyHandler({}))
env = os.environ.copy()
for key in list(env):
    if key.startswith('RELAIS_'):
        del env[key]
env.update(REDIS_PORT=str(redis_port), PORT_BASE=str(base), AGENT='echo')
handles = []
demo = caller = None
demo_children = set()
success = False

def parse_timestamp(value):
    # Python 3.9/3.10 accept 3 or 6 fractional digits; Go can emit up to 9.
    value = re.sub(r'\.(\d+)(?=Z$|[+-]\d{2}:\d{2}$)',
                   lambda match: '.' + (match.group(1) + '000000')[:6], value)
    if value.endswith('Z'):
        value = value[:-1] + '+00:00'
    return datetime.datetime.fromisoformat(value)

def assert_media_resumed(report):
    for move in report['Moves']:
        tracks = move['Tracks']
        assert tracks, f"no tracks after {move['Kind']}"
        assert all(track['PacketsAfter'] > 0 for track in tracks), move
    hung_up_at = report['HungUpAt']
    assert hung_up_at > 0, report
    for field in ('ConnectionStates', 'ICEConnectionStates'):
        history = report[field]
        assert history, f'missing connection state history: {field}'
        for change in history:
            state_name, at = change['State'], change['At']
            assert state_name != 'failed' or at >= hung_up_at, (field, change)

def save(name, data):
    (logs / name).write_text(json.dumps(data, indent=2) + '\n')

def request(url, body=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, headers={'Content-Type': 'application/json'})
    with http.open(req, timeout=5) as response:
        raw = response.read()
        return json.loads(raw) if raw else None

def poll(label, fn, seconds=25):
    deadline = time.monotonic() + seconds
    last = None
    while time.monotonic() < deadline:
        try:
            result = fn()
            if result:
                return result
        except (OSError, ValueError, KeyError, IndexError, urllib.error.URLError) as exc:
            last = exc
        time.sleep(0.1)
    raise AssertionError(f'{label} timed out; last error: {last}')

def launch(args, name):
    out = open(logs / f'{name}.stdout', 'w')
    err = open(logs / f'{name}.stderr', 'w')
    handles.extend([out, err])
    return subprocess.Popen(args, cwd=archive, env=env, stdout=out, stderr=err)

def ready(process, name):
    def check():
        if process.poll() is not None:
            raise RuntimeError(f'{name} exited {process.returncode}: ' + (logs / f'{name}.stderr').read_text())
        for line in (logs / f'{name}.stdout').read_text().splitlines():
            try:
                msg = json.loads(line)
            except ValueError:
                continue
            if msg.get('http'):
                return msg
    return poll(f'{name} readiness', check)

def descendants(pid):
    rows = subprocess.check_output(['ps', '-A', '-o', 'pid=,ppid='], text=True)
    pairs = [tuple(map(int, row.split())) for row in rows.splitlines()]
    found = {pid}
    while True:
        children = {child for child, parent in pairs if parent in found}
        if children <= found:
            return found - {pid}
        found |= children

def exists(pid):
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False

def port_closed(port):
    with socket.socket() as sock:
        sock.settimeout(0.2)
        return sock.connect_ex(('127.0.0.1', port)) != 0

def stop_owned(process):
    if process is not None and process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)

try:
    for name in ('relay', 'worker', 'control', 'signal', 'demo', 'load'):
        binary = f'relais-{name}'
        result = subprocess.run([str(archive / binary), '-version'], cwd=archive, env=env,
                                text=True, capture_output=True, check=True, timeout=10)
        assert all(field in result.stdout for field in ('version=', 'commit=', 'date=')), result.stdout
        (logs / f'{binary}.version').write_text(result.stdout)
    print('(a) PASS: six version flags', flush=True)

    # Fail before starting services if the host cannot inspect process identity.
    subprocess.run(['ps', '-p', str(os.getpid()), '-o', 'lstart='],
                   check=True, stdout=subprocess.DEVNULL)
    demo = launch([str(archive / 'relais-demo'), '-bin', str(archive), '-http', '127.0.0.1:0', '-agent=demo'], 'demo')
    demo_ready = ready(demo, 'demo')
    demo_children = descendants(demo.pid)
    assert len(demo_children) == 6, f'expected Redis, relay, 3 workers and control: {demo_children}'
    save('demo-processes.json', sorted(demo_children))
    with http.open(demo_ready['http'], timeout=5) as response:
        assert response.status == 200
    demo.terminate()
    assert demo.wait(timeout=20) == 0
    poll('demo child cleanup', lambda: all(not exists(pid) for pid in demo_children), seconds=10)
    demo_redis_port = int(demo_ready['redis'].rsplit(':', 1)[1])
    assert demo_redis_port != 6379
    assert port_closed(demo_redis_port), 'demo Redis still listening'
    print('(b) PASS: demo HTTP 200, exit 0, six children gone and Redis closed', flush=True)

    with open(logs / 'start.log', 'w') as output:
        subprocess.run([str(archive / 'start.sh'), '-state', str(state)], cwd=archive, env=env,
                       stdout=output, stderr=subprocess.STDOUT, check=True, timeout=100)
    def topology_ready():
        status = request(ctrl + '/status')
        if len(status['workers']) == 2 and all(not w['dead'] and not w['recovering'] for w in status['workers']):
            return status
    save('status-start.json', poll('two live workers', topology_ready))
    print('(c) PASS: shipped start.sh; two live workers', flush=True)

    caller = launch([str(archive / 'relais-load'), '-external-caller',
                     '-signaling', f'https://127.0.0.1:{base+43}/calls',
                     '-signaling-cert', str(state / 'secrets/signal.pem'),
                     '-bearer-token-file', str(state / 'secrets/signal.token'),
                     '-relay', f'127.0.0.1:{base+80}', '-callers', '1', '-duration', '35s',
                     '-output', str(logs / 'caller.jsonl')], 'caller')
    caller_http = ready(caller, 'caller')['http']
    start_at = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=1)
    request(caller_http + '/start', {'at': start_at.isoformat()})
    time.sleep(6)
    status = request(ctrl + '/status')
    assert status['lost_count'] == 0 and len(status['calls']) == 1, status
    call = status['calls'][0]
    call_id, owner = call['id'], call['owner']
    save('status-call.json', status)
    print('(d) PASS: one external caller through authenticated HTTPS signaling', flush=True)

    move = request(f'{ctrl}/calls/{call_id}/move', {})
    save('move.json', move)
    assert move['id'] == call_id and move['kind'] == 'move'
    assert move['from'] == owner and move['to'] != owner
    assert not move.get('error') and not move.get('lost') and move['result']['Duration'] > 0, move
    request(caller_http + '/record-moves', [move])
    status = request(ctrl + '/status')
    assert status['calls'][0]['move_count'] == 1 and status['calls'][0]['last_move_kind'] == 'move', status
    print('(e) PASS: move result, move_count=1, last_move_kind=move', flush=True)

    time.sleep(6)
    status = request(ctrl + '/status')
    owner = status['calls'][0]['owner']
    records = [line.split('|') for line in (state / 'pids').read_text().splitlines()]
    owner_pid = int([r[1] for r in records if r[0] == f'worker-{owner[1:]}'][-1])
    killed_at = datetime.datetime.now(datetime.timezone.utc).isoformat()
    os.kill(owner_pid, signal.SIGKILL)
    request(caller_http + '/kill', {'worker': owner, 'at': killed_at})
    def taken_over():
        status = request(ctrl + '/status')
        if (len(status['calls']) == 1 and status['calls'][0]['owner'] != owner
                and status['calls'][0]['takeover_count'] == 1 and len(status['takeovers']) == 1):
            return status
    status = poll('crash takeover', taken_over)
    save('status-takeover.json', status)
    assert status['lost_count'] == 0 and status['calls'][0]['last_move_kind'] == 'takeover', status
    takeover = status['takeovers'][0]
    assert takeover['id'] == call_id and takeover['from'] == owner and takeover['kind'] == 'takeover'
    assert not takeover.get('lost') and not takeover.get('error'), takeover
    # The caller collects takeovers from signal /status automatically.
    # Recording one through /record-moves would falsely add another planned move.
    assert next(w for w in status['workers'] if w['name'] == owner)['dead'] is True
    print('(f) PASS: SIGKILL of recorded owner; one takeover, lost_count=0', flush=True)

    with open(logs / 'restart.log', 'w') as output:
        subprocess.run([str(archive / 'start.sh'), '-state', str(state), '-restart-worker', owner],
                       cwd=archive, env=env, stdout=output, stderr=subprocess.STDOUT, check=True, timeout=40)
    def rejoined():
        status = request(ctrl + '/status')
        worker = next(w for w in status['workers'] if w['name'] == owner)
        if not worker['dead'] and not worker['recovering'] and parse_timestamp(worker['last_heartbeat']) > parse_timestamp(killed_at):
            return status
    status = poll('worker rejoin', rejoined)
    save('status-rejoined.json', status)
    assert status['lost_count'] == 0
    print('(g) PASS: same worker restarted; dead=false, recovering=false, fresh heartbeat', flush=True)

    caller_exit = caller.wait(timeout=70)
    save('caller-exit.json', {'exit_code': caller_exit})
    assert caller_exit == 0, f'caller exited {caller_exit}'
    messages = [json.loads(line) for line in (logs / 'caller.jsonl').read_text().splitlines()]
    calls = [m for m in messages if m['type'] == 'call']
    assert len(calls) == 1 and not calls[0].get('error'), calls
    assert sum(m['type'] == 'done' for m in messages) == 1
    report = calls[0]['call']
    assert report['OfferAnswerExchanges'] == 1 and report['ConnectedAt'] > 0
    assert report['ICERestarts'] == 0 and report['Renegotiations'] == 0
    assert sum(report['DecryptionFailures'].values()) == 0
    moves = report['Moves']
    assert len(moves) == 2 and sorted(m['Kind'] for m in moves) == ['move', 'takeover'], moves
    assert all(not m.get('lost') and not m['Error'] and m['Result']['Duration'] > 0 for m in moves), moves
    assert_media_resumed(report)
    save('status-finished.json', request(ctrl + '/status'))
    assert request(ctrl + '/status')['lost_count'] == 0
    print('(h) PASS: caller exit=0, one completed call, media after move+takeover, no failed connection/reconnect/decrypt failures', flush=True)
    success = True
finally:
    stop_owned(caller)
    try:
        if demo is not None and demo.poll() is None:
            demo_children |= descendants(demo.pid)
    finally:
        stop_owned(demo)
    # The demo owns and reaps its own children. Never signal an unrelated PID.
    if (state / 'pids').exists():
        with open(logs / 'stop.log', 'w') as output:
            stopped = subprocess.run([str(archive / 'stop.sh'), '-state', str(state)], cwd=archive, env=env,
                                     stdout=output, stderr=subprocess.STDOUT, timeout=120)
        assert stopped.returncode == 0, f'stop.sh failed; see {logs / "stop.log"}'
        records = [line.split('|') for line in (state / 'pids').read_text().splitlines()]
        assert all(not exists(int(row[1])) for row in records), 'recorded process remains'
    for handle in handles:
        handle.close()
    assert all(not exists(pid) for pid in demo_children), 'demo child remains'
    if success:
        assert port_closed(redis_port), f'Redis still listening on {redis_port}'
        print('(i) PASS: shipped stop.sh; every recorded PID gone; Redis port closed', flush=True)
        print('INSTALL TEST PASS', flush=True)
PY
