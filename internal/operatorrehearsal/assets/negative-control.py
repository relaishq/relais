# The Go launcher supplies control and clock_check. Never log file contents.
import hashlib
from pathlib import Path
import shlex
import subprocess
import sys
import time


class Failure(Exception):
    def __init__(self, reason, status=70):
        super().__init__(reason)
        self.status = status


def command(args, accepted=(0,), input=None, quiet=False):
    result = subprocess.run(args, input=input, capture_output=True, text=True)
    if not quiet or result.returncode not in accepted:
        print('action=%s exit_status=%d' % (shlex.join(args), result.returncode), flush=True)
        if result.stdout:
            print('stdout:\n' + result.stdout, flush=True)
        if result.stderr:
            print('stderr:\n' + result.stderr, flush=True)
    if result.returncode not in accepted:
        raise Failure('command-failed:' + args[0])
    return result


last_state = None
properties = ('LoadState', 'ActiveState', 'SubState', 'MainPID', 'Result', 'ExecMainCode', 'ExecMainStatus')


def state(allow_missing=False):
    global last_state
    args = ['systemctl', 'show', '--no-pager']
    for name in properties:
        args += ['-p', name]
    result = command(args + [unit], accepted=(0, 1) if allow_missing else (0,), quiet=True)
    values = dict(line.split('=', 1) for line in result.stdout.splitlines() if '=' in line)
    if (stage, values) != last_state or result.returncode != 0:
        print('unit=%s stage=%s show_status=%d %s' % (unit, stage, result.returncode,
              ' '.join(name + '=' + values.get(name, 'unknown') for name in properties)), flush=True)
        last_state = (stage, values)
    return values


def healthy():
    for _ in range(50):
        values = state()
        if (values.get('LoadState') == 'loaded' and values.get('ActiveState') == 'active'
                and values.get('SubState') == 'running' and int(values.get('MainPID', '0')) > 0):
            return
        time.sleep(0.1)
    raise Failure('unit-not-running')


path = Path(control_path)
print('control=%s unit=%s credential=%s path=%s expected=%s' %
      (control, unit, credential, path, expected), flush=True)
saved = Path(str(path) + '.saved')
changed, touched, status, stage, original = False, False, 0, 'precondition', None
try:
    healthy()
    if not path.is_file() or saved.exists():
        raise Failure('control-input-missing-or-backup-exists')
    original = hashlib.sha256(path.read_bytes()).digest()
    stage = 'stop-unit' if control != 'wrong-time-source' else 'save-time-source'
    if control != 'wrong-time-source':
        touched = True
        command(['systemctl', 'stop', unit])
    path.rename(saved)
    changed = True
    print('action=hide-file path=%s backup=%s path_absent=%s' % (path, saved, not path.exists()), flush=True)
    stage = 'apply-negative-input'
    if control == 'wrong-time-source':
        path.write_text('server 192.0.2.123 iburst\ndriftfile /var/lib/chrony/drift\n')
        print('action=replace-time-source path=%s source=192.0.2.123' % path, flush=True)
        command(['systemctl', 'restart', unit])
        command(['chronyc', '-n', 'tracking'])
        result = command(['sh', '-eu'], input=clock_check, accepted=(0, 1, 42))
        state()
        if result.returncode != 42:
            raise Failure('wrong-time-source-not-specifically-rejected', 71)
    else:
        if path.exists():
            raise Failure('negative-input-still-present')
        if control == 'missing-unit':
            command(['systemctl', 'daemon-reload'])
        result = command(['systemctl', 'start', unit], accepted=(0, 1, 4, 5))
        print('unit=%s start_status=%d expected=%s' % (unit, result.returncode, expected), flush=True)
        stage = 'observe-refusal'
        if control == 'missing-credential':
            # Type=simple completes the start job before credential setup/exec.
            # systemd's EXIT_CREDENTIALS (243), not the job status, proves refusal.
            for _ in range(50):
                values = state()
                if values.get('ActiveState') == 'failed':
                    break
                time.sleep(0.1)
            if (values.get('ActiveState') != 'failed' or values.get('MainPID') != '0'
                    or values.get('Result') != 'exit-code' or values.get('ExecMainStatus') != '243'):
                raise Failure('missing-credential-not-specifically-rejected', 71)
        else:
            values = state(allow_missing=True)
            if result.returncode == 0 or values.get('LoadState') != 'not-found' or values.get('MainPID') != '0':
                raise Failure('missing-unit-not-specifically-rejected', 71)
except Exception as error:
    status = error.status if isinstance(error, Failure) else 70
    print('control=%s stage=%s reason=%s exit_status=%d' % (control, stage, error, status), flush=True)
finally:
    if changed or touched:
        stage = 'restore'
        print('control=%s stage=restore expected=original-file-and-running-unit' % control, flush=True)

        def restore_file():
            if changed:
                saved.replace(path)
            if hashlib.sha256(path.read_bytes()).digest() != original or saved.exists():
                raise Failure('restored-file-differs')
            print('path=%s original_bytes_restored=true backup_absent=true' % path, flush=True)

        actions = []
        if control == 'missing-credential':
            actions.append(lambda: command(['systemctl', 'stop', unit]))
        actions.append(restore_file)
        if control == 'missing-unit':
            actions.append(lambda: command(['systemctl', 'daemon-reload']))
        if control != 'wrong-time-source':
            actions.append(lambda: command(['systemctl', 'reset-failed', unit]))
        actions.append(lambda: command(['systemctl', 'start' if control == 'missing-credential' else 'restart', unit]))
        if control == 'wrong-time-source':
            actions += [lambda: command(['chronyc', '-n', 'waitsync', '240', '0', '20', '1']),
                        lambda: command(['sh', '-eu'], input=clock_check)]
        actions.append(healthy)
        for action in actions:
            try:
                action()
            except Exception as error:
                status = 72
                print('control=%s stage=restore reason=%s exit_status=72' % (control, error), flush=True)
        print('control=%s restore_verified=%s' % (control, str(status != 72).lower()), flush=True)
    print('control=%s unit=%s credential=%s exit_status=%d' % (control, unit, credential, status), flush=True)
sys.exit(status)
