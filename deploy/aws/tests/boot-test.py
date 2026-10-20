#!/usr/bin/env python3
"""Real boot scripts/units, fake cloud sources, owned Redis Cluster, real audio call.

No AWS SDK/network calls occur. --systemd needs a disposable Linux CI runner.
The plain-process mode uses the same unit ExecStart and EnvironmentFile entries.
"""
import argparse
import datetime
import functools
import hashlib
import http.server
import json
import os
from pathlib import Path
import random
import shlex
import shutil
import signal
import socket
import ssl
import subprocess
import sys
import tarfile
import threading
import time
import urllib.request

from template import ROOT, load, resolve


class Refusal(Exception):
    pass


class Redis:
    def __init__(self, port, user, password):
        self.port, self.user, self.password = port, user, password

    def command(self, *args):
        with socket.create_connection(('127.0.0.1', self.port), timeout=2) as sock:
            stream = sock.makefile('rb')
            def send(parts):
                parts = [str(p).encode() for p in parts]
                sock.sendall(b'*%d\r\n' % len(parts) + b''.join(b'$%d\r\n%s\r\n' % (len(p), p) for p in parts))
                return receive()
            def receive():
                kind, data = stream.read(1), stream.readline().rstrip(b'\r\n')
                if kind == b'-':
                    raise Refusal(data.decode())
                if kind == b'+':
                    return data.decode()
                if kind == b':':
                    return int(data)
                if kind == b'$':
                    n = int(data)
                    if n == -1:
                        return None
                    result = stream.read(n)
                    stream.read(2)
                    return result.decode()
                if kind == b'*':
                    return [receive() for _ in range(int(data))]
                raise Refusal('invalid Redis response')
            send(('AUTH', self.user, self.password))
            return send(args)


class Trial:
    def __init__(self, args, work):
        self.args, self.work = args, Path(work)
        self.children, self.logs, self.units, self.reservations = [], {}, [], []
        self.named_children = {}
        self.passwords = {r: os.urandom(32).hex() for r in ('admin', 'relay', 'worker', 'control', 'token', 'snapshot')}
        # EnvironmentFile validation intentionally accepts only generated alphanumeric secrets.
        self.env = {k: v for k, v in os.environ.items() if not k.startswith(('RELAIS_', 'AWS_'))}
        self.env['PATH'] = self.minimal_path()
        self.template = load()

    def minimal_path(self):
        path = self.work / 'path'
        path.mkdir()
        for tool in ('bash', 'curl', 'mktemp', 'rm', 'shasum', 'sha256sum', 'python3', 'mkdir', 'install', 'id', 'useradd', 'systemctl'):
            executable = str(Path(sys.executable).resolve()) if tool == 'python3' else shutil.which(tool)
            if executable:
                (path / tool).symlink_to(executable)
        return str(path)

    def port(self, cluster=False):
        # Same six-node topology, reserved client/bus pair policy as #190's Go fixture.
        # The Go helper requires testing.T and has no CLI; this non-Go harness owns its children.
        for _ in range(500):
            n = random.SystemRandom().randrange(20000, 22000)
            if n in (6379, *range(7000, 7006), 9101, 9201):
                continue
            sockets = []
            try:
                for p in (n, n + 10000) if cluster else (n,):
                    for kind in (socket.SOCK_STREAM, socket.SOCK_DGRAM):
                        s = socket.socket(socket.AF_INET, kind)
                        sockets.append(s)
                        s.bind(('127.0.0.1', p))
                self.reservations.extend(sockets)
                return n, sockets
            except OSError:
                for s in sockets:
                    s.close()
        raise Refusal('cannot reserve owned test ports')

    def start(self, name, argv, env=None):
        log = self.work / (name + '.log')
        stream = log.open('w')
        try:
            child = subprocess.Popen(argv, env=env or self.env, stdout=stream, stderr=subprocess.STDOUT)
        finally:
            stream.close()
        self.children.append(child)
        self.named_children[name] = child
        self.logs[name] = log
        return child

    def run(self, argv, env, timeout):
        child = subprocess.Popen(argv, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
        try:
            stdout, stderr = child.communicate(timeout=timeout)
        except subprocess.TimeoutExpired:
            os.killpg(child.pid, signal.SIGKILL)
            child.communicate()
            raise
        return subprocess.CompletedProcess(argv, child.returncode, stdout, stderr)

    def wait(self, predicate, label, seconds=15):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            try:
                if predicate():
                    return
            except (OSError, ValueError, Refusal):
                pass
            time.sleep(0.1)
        raise Refusal('readiness failed: ' + label)

    def cluster(self):
        ports = []
        for i in range(6):
            port, reserved = self.port(cluster=True)
            directory = self.work / ('redis-' + str(i))
            directory.mkdir()
            config = directory / 'redis.conf'
            config.write_text(f'''bind 127.0.0.1
port {port}
cluster-enabled yes
cluster-config-file nodes.conf
cluster-node-timeout 1000
save ""
appendonly no
dir {directory}
user default off
user test-admin on >{self.passwords['admin']} ~* +@all
masteruser test-admin
masterauth {self.passwords['admin']}
''')
            config.chmod(0o600)
            for s in reserved:
                s.close()
            self.start('redis-' + str(i), [shutil.which('redis-server'), str(config)])
            admin = Redis(port, 'test-admin', self.passwords['admin'])
            self.wait(lambda: admin.command('PING') == 'PONG', 'Redis listener')
            ports.append(port)
        command = [shutil.which('redis-cli'), '--user', 'test-admin', '--cluster', 'create',
                   *('127.0.0.1:' + str(p) for p in ports), '--cluster-replicas', '1', '--cluster-yes']
        result = subprocess.run(command, env={**self.env, 'REDISCLI_AUTH': self.passwords['admin']}, capture_output=True, timeout=25)
        assert result.returncode == 0, 'owned Cluster creation refused'
        for port in ports:
            admin = Redis(port, 'test-admin', self.passwords['admin'])
            self.wait(lambda: 'cluster_state:ok' in admin.command('CLUSTER', 'INFO'), 'Cluster convergence')
            for role in ('relay', 'worker', 'control'):
                access = self.template['Resources'][role.title() + 'StoreUser']['Properties']['AccessString'].split()
                if self.args.mutation == 'acl' and role == 'relay':
                    access.append('-hmget')
                assert admin.command('ACL', 'SETUSER', role, 'reset', '>' + self.passwords[role], *access) == 'OK'
        # Verify the real ACL boundary independently of successful startup.
        for port in ports:
            relay = Redis(port, 'relay', self.passwords['relay'])
            for command in [('GET', 'relais:preview:state:{sess:acl}'), ('HGET', 'relais:preview:frames:{sess:acl}', 'x'), ('CONFIG', 'GET', '*')]:
                try:
                    relay.command(*command)
                except Refusal as exc:
                    assert 'NOPERM' in str(exc)
                else:
                    raise Refusal('relay escaped role ACL')
        self.seed = ','.join('127.0.0.1:' + str(p) for p in ports)

    def archive(self):
        directory = self.work / 'relais_0.3.0_linux_arm64'
        directory.mkdir()
        for name in ('relais-relay', 'relais-worker', 'relais-control', 'relais-signal', 'relais-load'):
            shutil.copy2(Path(self.args.bin) / name, directory / name)
        artifact_dir = self.work / 'artifacts'
        artifact_dir.mkdir(mode=0o700)
        archive = self.archive_path = artifact_dir / (directory.name + '.tar.gz')
        with tarfile.open(archive, 'w:gz') as tar:
            tar.add(directory, arcname=directory.name)
        class Handler(http.server.SimpleHTTPRequestHandler):
            def log_message(self, *_):
                pass
        self.server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(Handler, directory=str(artifact_dir)))
        self.server_thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.server_thread.start()
        self.env['RELAIS_TEST_ARCHIVE_URL'] = f'http://127.0.0.1:{self.server.server_port}/{archive.name}'
        return hashlib.sha256(archive.read_bytes()).hexdigest()

    def boot(self):
        image = self.work / 'image'
        for folder in ('opt/relais/boot', 'etc/systemd/system', 'etc/relais', 'run/relais'):
            (image / folder).mkdir(parents=True)
        for source in (ROOT / 'boot').iterdir():
            if source.is_file():
                shutil.copy2(source, image / 'opt/relais/boot' / source.name)
        for source in (ROOT / 'units').glob('*.service'):
            shutil.copy2(source, image / 'etc/systemd/system' / source.name)
        config = {'version': '0.3.0', 'architecture': 'arm64', 'sha256': self.archive(), 'ssm': 'false',
                  'region': 'offline', 'components': ['relay', 'worker', 'control', 'signal'],
                  'secrets': {r: r for r in ('relay', 'worker', 'control', 'token', 'snapshot')}}
        node = image / 'etc/relais/node.json'
        node.write_text(json.dumps(config))
        source = self.work / 'fake-secrets.json'
        source.write_text(json.dumps(self.passwords))
        source.chmod(0o600)
        self.env['RELAIS_TEST_SECRET_FILE'] = str(source)
        manager = self.work / 'service-manager'
        manager.write_text('#!/usr/bin/env bash\nset -eu\nprintf "%s\\n" "$*" >> "$RELAIS_TEST_MANAGER_LOG"\n')
        manager.chmod(0o755)
        self.env.update(RELAIS_TEST_SYSTEMCTL=str(manager), RELAIS_TEST_MANAGER_LOG=str(self.work / 'manager.log'),
                        RELAIS_INSTALL_ROOT=str(image), RELAIS_BOOT_PYTHON=sys.executable)
        # Check refusal before any service-manager invocation, not just downloader's return code.
        config['sha256'] = '0' * 64
        node.write_text(json.dumps(config))
        bad = self.run([str(ROOT / 'boot/install.sh')], self.env, 30)
        assert bad.returncode != 0 and b'digest mismatch' in bad.stderr
        assert not (self.work / 'manager.log').exists() and not (image / 'opt/relais/bin/relais-relay').exists()
        print('PASS: bad digest refuses boot before any service starts')
        config['sha256'] = self.archive_digest = hashlib.sha256(self.archive_path.read_bytes()).hexdigest()
        node.write_text(json.dumps(config))
        result = self.run([str(ROOT / 'boot/install.sh')], self.env, 30)
        assert result.returncode == 0, 'valid boot script failed: ' + result.stderr.decode()
        runtime = image / 'run/relais'
        for p in runtime.iterdir():
            assert p.stat().st_mode & 0o777 == 0o600
        assert (runtime / 'relay.env').read_text().find('SESSIONSTORE_KEY') == -1
        assert set(p.name for p in runtime.iterdir()) == {'relay.env', 'worker.env', 'control.env', 'signal.token'}
        commands = (self.work / 'manager.log').read_text()
        assert 'mask amazon-ssm-agent.service' in commands and 'enable --now relais-relay-standby.service' in commands
        self.image, self.runtime = image, runtime
        print('PASS: valid archive, fake-secret boot, role files mode 0600, SSM off, minimal PATH')

    def node_environment(self, role):
        instance = resolve(self.template)['RelayHost' if role.startswith('relay') else 'WorkerOne' if role.startswith('worker') else 'SharedHost']
        text = instance['Metadata']['RelaisFiles']['/etc/relais/node.env']['content']
        values = dict(line.split('=', 1) for line in text.splitlines() if line)
        values.update(RELAIS_REDIS_ADDR=self.seed, RELAIS_REDIS_TLS='0', RELAIS_PRIVATE_NETS='10.42.0.0/16',
                      RELAIS_BIN_DIR=str(self.image / 'opt/relais/bin'), NODE_PRIVATE='127.0.0.1',
                      RELAY_PRIVATE='127.0.0.1', RELAY_PUBLIC='127.0.0.1', CONTROL_PRIVATE='127.0.0.1',
                      WORKER_ONE='127.0.0.1', WORKER_TWO='127.0.0.1', WORKER_THREE='', WORKER_CALL_CAP='2')
        values.update({k: str(v) for k, v in self.ports.items()})
        if role == 'worker-two':
            values.update(WORKER_NAME='w2', WORKER_HTTP_PORT=str(self.ports['WORKER_TWO_PORT']), WORKER_MEDIA_PORT=str(self.worker_two_media))
        else:
            values.update(WORKER_NAME='w1', WORKER_HTTP_PORT=str(self.ports['WORKER_ONE_PORT']), WORKER_MEDIA_PORT=str(self.worker_one_media))
        return values

    def service(self, name):
        role = 'worker' if name.startswith('worker') else name
        unit = (ROOT / 'units' / ('relais-' + role + '.service')).read_text()
        if self.args.mutation == 'unit' and name == 'relay':
            unit = unit.replace('ExecStart=/opt/relais/boot/run.sh relay', 'ExecStart=/usr/bin/false')
        environment = self.node_environment(name)
        envfile = self.work / (name + '.env')
        envfile.write_text(''.join(f'{k}={v}\n' for k, v in environment.items()))
        envfile.chmod(0o644)
        if self.args.systemd:
            unit_name = 'relais-test-' + name + '.service'
            destination = Path('/etc/systemd/system') / unit_name
            destination.write_text(unit)
            self.units.append(unit_name)
            dropins = Path(str(destination) + '.d')
            dropins.mkdir()
            # Preserve the original unit and its hardening; override only test fixture paths/settings.
            dropin = '[Service]\nEnvironmentFile=' + str(envfile) + '\n'
            (dropins / 'fixture.conf').write_text(dropin)
            subprocess.run(['systemctl', 'daemon-reload'], check=True)
            subprocess.run(['systemctl', 'start', unit_name], check=True)
        else:
            actual_environment = {}
            for line in unit.splitlines():
                if line.startswith('EnvironmentFile='):
                    path = line.removeprefix('EnvironmentFile=')
                    source = envfile if path == '/etc/relais/node.env' else self.image / path.lstrip('/')
                    for setting in source.read_text().splitlines():
                        if setting and not setting.startswith('#'):
                            key, value = setting.split('=', 1)
                            actual_environment[key] = value
                if line.startswith('LoadCredential='):
                    name, source = line.removeprefix('LoadCredential=').split(':', 1)
                    credentials = self.work / 'credentials-signal'
                    credentials.mkdir(mode=0o700)
                    shutil.copy2(self.image / source.lstrip('/'), credentials / name)
                    actual_environment['CREDENTIALS_DIRECTORY'] = str(credentials)
            environment = actual_environment
            command = next(line.removeprefix('ExecStart=') for line in unit.splitlines() if line.startswith('ExecStart='))
            command = command.replace('/opt/relais/boot/', str(ROOT / 'boot') + '/')
            self.start(name, shlex.split(command), {**self.env, **environment})

    def services(self):
        names = ['RELAY_MEDIA_PORT', 'RELAY_LEG_PORT', 'RELAY_HTTP_PORT', 'CONTROL_HTTP_PORT', 'WORKER_ONE_PORT', 'WORKER_TWO_PORT', 'SIGNAL_PORT']
        self.ports = {name: self.port()[0] for name in names}
        self.worker_one_media, _ = self.port()
        self.worker_two_media, _ = self.port()
        for s in self.reservations:
            s.close()
        if self.args.systemd:
            assert sys.platform == 'linux' and os.geteuid() == 0
            # CI runner is disposable. Local Mac never enters this branch.
            for role in ('relay', 'worker', 'control', 'signal'):
                subprocess.run(['useradd', '--system', '--no-create-home', '--shell', '/usr/sbin/nologin', 'relais-' + role], check=True)
            shutil.copytree(self.image / 'opt/relais', '/opt/relais', dirs_exist_ok=True)
            Path('/opt/relais/sdk/bin').mkdir(parents=True, exist_ok=True)
            Path('/opt/relais/sdk/bin/python').symlink_to(sys.executable)
            Path('/etc/relais').mkdir(exist_ok=True)
            Path('/etc/relais/node.env').write_text('# Fixture public settings are appended by each unit drop-in.\n')
            shutil.copy2(self.image / 'etc/relais/node.json', '/etc/relais/node.json')
            shutil.copy2(ROOT / 'units/relais-prepare.service', '/etc/systemd/system/relais-prepare.service')
            self.units.append('relais-prepare.service')
            dropins = Path('/etc/systemd/system/relais-prepare.service.d')
            dropins.mkdir()
            (dropins / 'fixture.conf').write_text('[Service]\nEnvironment=RELAIS_TEST_SECRET_FILE=' + self.env['RELAIS_TEST_SECRET_FILE'] + '\n')
            # No binaries in /tmp: ProtectHome/PrivateTmp otherwise correctly hide fixtures.
            self.image = Path('/')
        self.service('relay')
        self.wait(lambda: urllib.request.urlopen(f'http://127.0.0.1:{self.ports["RELAY_HTTP_PORT"]}/status', timeout=1).status == 200,
                  'primary relay unit')
        for name in ('relay-standby', 'worker', 'worker-two', 'control', 'signal'):
            self.service(name)
        for port in ('WORKER_ONE_PORT', 'WORKER_TWO_PORT'):
            self.wait(lambda: urllib.request.urlopen(f'http://127.0.0.1:{self.ports[port]}/status', timeout=1).status == 200,
                      'worker unit')
        def ready():
            data = json.load(urllib.request.urlopen(f'http://127.0.0.1:{self.ports["CONTROL_HTTP_PORT"]}/status', timeout=1))
            return data.get('ready') is True
        self.wait(ready, 'production fixed bootstrap', seconds=20)
        self.wait(lambda: self.get_certificate() is not None, 'signaling TLS')
        print('PASS: same unit ExecStart commands boot relay + standby, two workers, control, signaling')

    def get_certificate(self):
        try:
            return ssl.get_server_certificate(('127.0.0.1', self.ports['SIGNAL_PORT']), timeout=1)
        except (OSError, ValueError):
            return None

    def call(self):
        cert = self.work / 'signal.pem'
        cert.write_text(self.get_certificate())
        out = self.work / 'caller.jsonl'
        child = self.start('caller', [str(self.image / 'opt/relais/bin/relais-load'), '-external-caller',
                    f'-signaling=https://127.0.0.1:{self.ports["SIGNAL_PORT"]}/calls',
                    '-signaling-cert=' + str(cert), '-bearer-token-file=' + str(self.runtime / 'signal.token'),
                    f'-relay=127.0.0.1:{self.ports["RELAY_MEDIA_PORT"]}', '-caller-bind=127.0.0.1:0',
                    '-callers=1', '-duration=4s', '-sample-every=1s', '-video=false', '-output=' + str(out)])
        endpoint = []
        def caller_ready():
            for line in self.logs['caller'].read_text().splitlines():
                try:
                    message = json.loads(line)
                except ValueError:
                    continue
                if message.get('type') == 'ready':
                    endpoint.append(message['http'])
                    return True
            return False
        self.wait(caller_ready, 'real caller setup')
        now = datetime.datetime.now(datetime.timezone.utc).isoformat()
        request = urllib.request.Request(endpoint[0] + '/start', json.dumps({'at': now}).encode(), {'Content-Type': 'application/json'})
        assert urllib.request.urlopen(request, timeout=2).status == 200
        assert child.wait(timeout=25) == 0, 'real caller exited with failure'
        messages = [json.loads(line) for line in out.read_text().splitlines()]
        calls = [m for m in messages if m.get('type') == 'call']
        assert len(calls) == 1 and not calls[0].get('error')
        assert any(m.get('type') == 'done' for m in messages)
        report = calls[0]['call']
        (self.work / 'call-report.json').write_text(json.dumps(report, indent=2))
        # Do not infer successful media from a successful SDP exchange or process exit.
        assert report['ConnectedAt'] > 0
        assert any(t['Kind'] == 'audio' and t['Packets'] >= 50 for t in report['Tracks'])
        assert sum(report['DecryptionFailures'].values()) == 0
        assert report['OfferAnswerExchanges'] == 1 and report['ICERestarts'] == 0
        print('PASS: connected caller decrypted >=50 audio packets with zero decryption failures')
        print('PASS: real external caller completed an authenticated audio call through all four components')

    def close(self):
        unit_order = ['relais-test-' + n + '.service' for n in
                      ('signal', 'worker-two', 'worker', 'control', 'relay-standby', 'relay')]
        for unit in unit_order + ['relais-prepare.service']:
            if unit in self.units:
                subprocess.run(['systemctl', 'stop', unit], capture_output=True, timeout=25)
                result = subprocess.run(['journalctl', '--unit', unit, '--no-pager', '--output=cat'], capture_output=True)
                log = self.work / (unit + '.log')
                log.write_bytes(result.stdout + result.stderr)
                self.logs[unit] = log
        order = [self.named_children[n] for n in ('caller', 'signal', 'worker-two', 'worker', 'control', 'relay-standby', 'relay')
                 if n in self.named_children]
        order += [p for p in reversed(self.children) if p not in order]
        for child in order:
            if child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait(timeout=5)
        if hasattr(self, 'server'):
            self.server.shutdown()
            self.server.server_close()
        for s in self.reservations:
            s.close()
        for path in self.logs.values():
            content = path.read_text(errors='replace')
            assert not any(secret in content for secret in self.passwords.values()), 'secret appeared in service log'


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--bin', required=True)
    parser.add_argument('--systemd', action='store_true')
    parser.add_argument('--mutation', choices=('acl', 'unit'))
    parser.add_argument('--work', required=True)
    args = parser.parse_args()
    Path(args.work).mkdir(mode=0o700, parents=True, exist_ok=False)
    trial = Trial(args, args.work)
    try:
        trial.cluster()
        trial.boot()
        trial.services()
        trial.call()
    except Exception as exc:
        message = str(exc)
        for password in trial.passwords.values():
            message = message.replace(password, '[REDACTED]')
        print('FAIL:', message, file=sys.stderr)
        if args.mutation:
            if args.mutation == 'acl':
                logs = '\n'.join(p.read_text() for n, p in trial.logs.items() if n.startswith('relay'))
                matched = any(word in logs.lower() for word in ('noperm', 'no permissions', 'can\'t run this command'))
            else:
                matched = any(p.args == ['/usr/bin/false'] and p.poll() == 1 for p in trial.children)
            if matched:
                print('EXPECTED REFUSAL: ' + args.mutation)
                return 42
        return 1
    finally:
        trial.close()
    return 0


if __name__ == '__main__':
    sys.exit(main())
