#!/usr/bin/env python3
"""Run a command with an owned, persistence-free Redis and CI unit-test settings."""
import locale
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time


def free_port():
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    if port == 6379:
        return free_port()
    return port


def ready(port):
    try:
        with socket.create_connection(('127.0.0.1', port), timeout=0.1) as client:
            client.sendall(b'*1\r\n$4\r\nPING\r\n')
            return client.recv(64) == b'+PONG\r\n'
    except OSError:
        return False


def signal_group(process, signum):
    try:
        os.killpg(process.pid, signum)
    except ProcessLookupError:
        pass
    except PermissionError:
        # A just-exited, unreaped leader can produce EPERM on macOS. Reap
        # that leader before retrying; never ignore an error for a live one.
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            raise
        try:
            os.killpg(process.pid, signum)
        except ProcessLookupError:
            pass


def stop(process, *, process_group=False, timeout=5):
    if process is None:
        return
    if process_group:
        # The leader may have exited while a background descendant remains.
        # Every group here was created with start_new_session=True.
        process.poll()
        signal_group(process, signal.SIGTERM)
        try:
            process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            pass
        # Kill the entire group even after its leader exits. A background
        # descendant can ignore TERM or outlive a successful command.
        signal_group(process, signal.SIGKILL)
        process.wait()
    else:
        if process.poll() is None:
            process.terminate()
        try:
            process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def check_locale():
    locale.setlocale(locale.LC_ALL, 'en_US.UTF-8')


def ci_environment(environ):
    # Strip opt-ins before starting either Redis or the test command. In
    # particular, inherited update flags must never rewrite golden fixtures.
    env = {key: value for key, value in environ.items()
           if not key.startswith(('RELAIS_TEST_', 'RELAIS_REDIS_', 'RELAIS_UPDATE_'))}
    env.update(RELAIS_TEST_REDIS_REQUIRE='1', RELAIS_TEST_REDIS_TLS_REQUIRE='1',
               LC_ALL='en_US.UTF-8')
    return env


def main(argv, *, locale_check=check_locale, stop_timeout=5):
    if not argv:
        print('Usage: scripts/dev/with-redis.py <command> [args...]', file=sys.stderr)
        return 2
    binary = shutil.which('redis-server')
    if binary is None:
        print('redis-server is required on PATH; provide it before running make check. '
              'Nothing was installed.', file=sys.stderr)
        return 1
    # Redis locale tests select en_US.UTF-8 themselves. Check it is available,
    # rather than silently skipping the CI condition or installing system data.
    try:
        locale_check()
    except locale.Error:
        print('en_US.UTF-8 is required for the Redis tests; generate that locale '
              'before running make check.', file=sys.stderr)
        return 1
    env = ci_environment(os.environ)
    command = None
    server = None
    port = None
    directory = None
    first_signal = None
    repeated_signal = False
    cleaning_up = False
    command_status = None

    def interrupted(signum, _frame):
        nonlocal first_signal, repeated_signal
        if first_signal is None:
            first_signal = signum
        else:
            repeated_signal = True
        # Never unwind a Popen assignment or cleanup from a signal handler.
        # Forward only to the group we own, without waiting or raising if its
        # leader has just exited. Cleanup will stop and reap the entire group.
        if command is not None and not cleaning_up:
            try:
                os.killpg(command.pid, signum)
            except OSError:
                pass

    def run():
        nonlocal command, server, port
        log_path = Path(directory.name) / 'redis.log'
        # Another process can claim a released ephemeral port. Retry only
        # our own failed startup; never shut down an existing listener.
        for _ in range(10):
            if first_signal is not None:
                return None
            port = free_port()
            with log_path.open('w') as log:
                server = subprocess.Popen(
                    [binary, '--bind', '127.0.0.1', '--port', str(port),
                     '--save', '', '--appendonly', 'no', '--dir', directory.name],
                    stdout=log, stderr=subprocess.STDOUT, env=env,
                    start_new_session=True)
            if first_signal is not None:
                return None
            deadline = time.monotonic() + 10
            while (first_signal is None and server.poll() is None
                   and time.monotonic() < deadline):
                if "Ready to accept connections" in log_path.read_text() and ready(port):
                    break
                time.sleep(0.05)
            if first_signal is not None:
                return None
            if server.poll() is None and "Ready to accept connections" in log_path.read_text() and ready(port):
                break
            stop(server)
        else:
            print('Could not start a disposable Redis:\n' + log_path.read_text(),
                  file=sys.stderr)
            return None
        if first_signal is not None:
            return None
        env['RELAIS_TEST_REDIS_ADDR'] = f'127.0.0.1:{port}'
        print(f'Throwaway Redis started on 127.0.0.1:{port} (pid {server.pid})',
              flush=True)
        command = subprocess.Popen(argv, env=env, start_new_session=True)
        while first_signal is None:
            try:
                status = command.wait(timeout=0.1)
            except subprocess.TimeoutExpired:
                continue
            # A forwarded signal may have ended wait(); retain the conventional
            # 128 + signal result for an interrupted, previously running command.
            if first_signal is None:
                return status
        return None

    def cleanup():
        nonlocal cleaning_up
        if cleaning_up:
            return
        cleaning_up = True
        try:
            stop(command, process_group=True, timeout=stop_timeout)
        finally:
            try:
                stop(server)
                if server is not None:
                    print(f'Throwaway Redis stopped on 127.0.0.1:{port} '
                          f'(pid {server.pid} reaped)', flush=True)
            finally:
                if directory is not None:
                    directory.cleanup()

    previous = {sig: signal.signal(sig, interrupted)
                for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)}
    try:
        directory = tempfile.TemporaryDirectory(prefix='relais-test-redis-')
        command_status = run()
    except OSError as exc:
        print(f'Could not run Redis test command: {exc}', file=sys.stderr)
    finally:
        try:
            cleanup()
        finally:
            for sig, handler in previous.items():
                signal.signal(sig, handler)
    if command_status is not None:
        return command_status
    return 128 + first_signal if first_signal is not None else 1


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
