"""Exercise cleanup without requiring Redis or a generated locale in CI."""
import importlib.util
import json
import locale
from pathlib import Path
import os
import re
import selectors
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parents[2]
WRAPPER = ROOT / 'scripts/dev/with-redis.py'
SPEC = importlib.util.spec_from_file_location('with_redis', WRAPPER)
WRAPPER_MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(WRAPPER_MODULE)
# The CLI exposes no test overrides. Record owned groups before Popen returns,
# then optionally deliver a signal in exactly the command-assignment window.
BOOTSTRAP = r'''import json,os,runpy,signal,sys,time
ns = runpy.run_path(sys.argv[1])
subprocess = ns["main"].__globals__["subprocess"]
original_popen = subprocess.Popen

def tracked_popen(argv, **kwargs):
    process = original_popen(argv, **kwargs)
    with open(os.environ["WRAPPER_PROCESS_LOG"], "a") as log:
        log.write(json.dumps({"pid": process.pid, "group": True}) + "\n")
    if argv[0] == sys.executable and os.environ.get("WRAPPER_SIGNAL_DURING_START"):
        deadline = time.monotonic() + 10
        while not os.path.exists(os.environ["WRAPPER_COMMAND_READY"]):
            if time.monotonic() >= deadline:
                raise RuntimeError("Command did not become ready during Popen")
            time.sleep(0.01)
        os.kill(os.getpid(), signal.SIGTERM)
    return process

subprocess.Popen = tracked_popen
sys.exit(ns["main"](sys.argv[2:], locale_check=lambda:None, stop_timeout=0.3))
'''


def read_line(process):
    with selectors.DefaultSelector() as selector:
        selector.register(process.stdout, selectors.EVENT_READ)
        if not selector.select(timeout=15):
            raise AssertionError('Timed out waiting for wrapper output')
    return process.stdout.readline().decode()


def gone(pid):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            return True
        time.sleep(0.05)
    return False


class RedisLifetime(unittest.TestCase):
    def test_missing_binary_has_repair_message(self):
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run([sys.executable, str(WRAPPER), 'unused'],
                                    env=dict(os.environ, PATH=directory),
                                    capture_output=True, text=True, timeout=15)
        self.assertEqual(result.returncode, 1)
        self.assertIn('redis-server is required on PATH', result.stderr)

    def test_environment_removes_all_opt_ins(self):
        inherited = {'RELAIS_TEST_REDIS_CLUSTER_ADDRS': 'unused:16379',
                     'RELAIS_TEST_REDIS_PASSWORD': 'unused',
                     'RELAIS_TEST_ARTIFACT_DIR': 'unused',
                     'RELAIS_REDIS_ALLOW_LOCAL_6379': '1',
                     'RELAIS_REDIS_OTHER': '1', 'RELAIS_UPDATE_COMPONENT_GOLDEN': '1',
                     'RELAIS_UPDATE_FUTURE_FLAG': '1', 'PATH': 'retained'}
        result = WRAPPER_MODULE.ci_environment(inherited)
        self.assertEqual(result, {'PATH': 'retained', 'RELAIS_TEST_REDIS_REQUIRE': '1',
                                 'RELAIS_TEST_REDIS_TLS_REQUIRE': '1', 'LC_ALL': 'en_US.UTF-8'})
        self.assertEqual(inherited['RELAIS_UPDATE_COMPONENT_GOLDEN'], '1')

    def exercise(self, exit_code=0, interrupt=None, stubborn=False, background=False,
                 real=False, during_start=False, kill_wrapper=False):
        child_code = ('import os,signal,time; signal.signal(signal.SIGINT,signal.SIG_IGN); '
                      'signal.signal(signal.SIGTERM,signal.SIG_IGN); '
                      'print(os.getpid(),flush=True); time.sleep(60)')
        command = '''import json,os,signal,subprocess,sys,time
allowed = {"RELAIS_TEST_REDIS_ADDR", "RELAIS_TEST_REDIS_REQUIRE", "RELAIS_TEST_REDIS_TLS_REQUIRE"}
assert {k for k in os.environ if k.startswith(("RELAIS_TEST_", "RELAIS_REDIS_", "RELAIS_UPDATE_"))} == allowed
assert os.environ["RELAIS_TEST_REDIS_REQUIRE"] == "1"
assert os.environ["RELAIS_TEST_REDIS_TLS_REQUIRE"] == "1"
assert os.environ["LC_ALL"] == "en_US.UTF-8"
'''
        if stubborn:
            command += 'signal.signal(signal.SIGINT, signal.SIG_IGN)\nsignal.signal(signal.SIGTERM, signal.SIG_IGN)\n'
        if background:
            command += ('child = subprocess.Popen([sys.executable, "-c", ' + repr(child_code)
                        + '], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)\n'
                        'pid = int(child.stdout.readline())\n')
        else:
            command += 'pid = None\n'
        command += ('if pid is not None:\n'
                    '    with open(os.environ["WRAPPER_PROCESS_LOG"], "a") as log:\n'
                    '        log.write(json.dumps({"pid": pid, "group": False}) + "\\n")\n'
                    'print("COMMAND_READY " + json.dumps(pid), flush=True)\n'
                    'if "WRAPPER_COMMAND_READY" in os.environ:\n'
                    '    open(os.environ["WRAPPER_COMMAND_READY"], "w").close()\n')
        command += 'time.sleep(60)\n' if interrupt or kill_wrapper else f'raise SystemExit({exit_code})\n'
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            temp = root / 'temp'
            temp.mkdir()
            bindir = root / 'bin'
            bindir.mkdir()
            argv = [sys.executable, '-c', BOOTSTRAP, str(WRAPPER)]
            if real:
                path = os.environ.get('PATH', '')
            else:
                stub = bindir / 'redis-server'
                stub.write_text('#!' + sys.executable + '\n' + (ROOT / 'scripts/ci/redis_stub.py').read_text())
                stub.chmod(0o700)
                path = str(bindir)  # Only the stub is available; no real Redis or locale tool.
            process_log = root / 'processes.jsonl'
            env = dict(os.environ, TMPDIR=str(temp), PATH=path,
                       WRAPPER_PROCESS_LOG=str(process_log),
                       WRAPPER_COMMAND_READY=str(root / 'command-ready'),
                       RELAIS_TEST_REDIS_CLUSTER='1', RELAIS_TEST_REDIS_PASSWORD='unused',
                       RELAIS_REDIS_ALLOW_LOCAL_6379='1', RELAIS_UPDATE_COMPONENT_GOLDEN='1')
            if during_start:
                env['WRAPPER_SIGNAL_DURING_START'] = '1'
            else:
                env.pop('WRAPPER_SIGNAL_DURING_START', None)
            process = subprocess.Popen(argv + [sys.executable, '-c', command], env=env,
                                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT, bufsize=0)
            descendant = None
            pid = None
            try:
                first = read_line(process)
                match = re.search(r'127\.0\.0\.1:(\d+) \(pid (\d+)\)', first)
                self.assertIsNotNone(match, first)
                port, pid = map(int, match.groups())
                self.assertNotEqual(port, 6379)
                marker = read_line(process)
                self.assertTrue(marker.startswith('COMMAND_READY '), marker)
                descendant = json.loads(marker.split(' ', 1)[1])
                if kill_wrapper:
                    process.kill()
                elif interrupt and not during_start:
                    process.send_signal(interrupt)
                if kill_wrapper:
                    # SIGKILL cannot run wrapper cleanup; stop its recorded command
                    # group ourselves, leaving the stub to detect parent death.
                    for record in map(json.loads, process_log.read_text().splitlines()):
                        if record['group'] and record['pid'] != pid:
                            os.killpg(record['pid'], signal.SIGKILL)
                rest, _ = process.communicate(timeout=20)
                rest = rest.decode()
                expected = -signal.SIGKILL if kill_wrapper else 128 + interrupt if interrupt else exit_code
                self.assertEqual(process.returncode, expected, rest)
                if not kill_wrapper:
                    self.assertIn('pid ' + str(pid) + ' reaped', rest)
                self.assertTrue(gone(pid), 'Redis process remained')
                if descendant:
                    self.assertTrue(gone(descendant), 'Background grandchild remained')
                for record in map(json.loads, process_log.read_text().splitlines()):
                    self.assertTrue(gone(record['pid']), 'Owned process remained after wrapper exit')
                with socket.socket() as client:
                    self.assertNotEqual(client.connect_ex(('127.0.0.1', port)), 0)
                if not kill_wrapper:
                    self.assertFalse(list(temp.iterdir()), 'Redis temporary directory leaked')
            finally:
                if process.poll() is None:
                    process.terminate()
                try:
                    process.communicate(timeout=20)
                except subprocess.TimeoutExpired:
                    process.kill()
                # Read groups even when Popen never returned to the wrapper or
                # an assertion failed before COMMAND_READY could be consumed.
                records = ([json.loads(line) for line in process_log.read_text().splitlines()]
                           if process_log.exists() else [])
                for record in records:
                    if record['group']:
                        try:
                            os.killpg(record['pid'], signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                process.communicate(timeout=20)
                for record in records:
                    if not gone(record['pid']):
                        os.kill(record['pid'], signal.SIGKILL)
                        self.assertTrue(gone(record['pid']), 'Test cleanup leaked an owned process')
                # Optional external evidence includes only processes this test started.
                if os.environ.get('WRAPPER_TEST_LEDGER'):
                    with open(os.environ['WRAPPER_TEST_LEDGER'], 'a') as ledger:
                        for record in records + [{'pid': process.pid, 'group': False}]:
                            ledger.write(json.dumps(record) + '\n')

    def test_signal_during_command_start(self):
        self.exercise(interrupt=signal.SIGTERM, stubborn=True, background=True, during_start=True)

    def test_stub_exits_when_wrapper_is_killed(self):
        self.exercise(kill_wrapper=True, stubborn=True, background=True)

    def test_success(self):
        self.exercise()

    def test_failure(self):
        self.exercise(exit_code=7)

    def test_interrupt(self):
        self.exercise(interrupt=signal.SIGINT)

    def test_terminate(self):
        self.exercise(interrupt=signal.SIGTERM)

    def test_stubborn_command_and_grandchild(self):
        for sig in (signal.SIGINT, signal.SIGTERM):
            with self.subTest(signal=sig):
                self.exercise(interrupt=sig, stubborn=True, background=True)

    def test_exited_leader_leaves_no_grandchild(self):
        self.exercise(background=True)

    def test_real_redis_extra(self):
        if shutil.which('redis-server') is None:
            self.skipTest('optional real-Redis extra: executable unavailable')
        previous = locale.setlocale(locale.LC_ALL)
        try:
            locale.setlocale(locale.LC_ALL, 'en_US.UTF-8')
        except locale.Error:
            self.skipTest('optional real-Redis extra: locale unavailable')
        finally:
            locale.setlocale(locale.LC_ALL, previous)
        self.exercise(real=True)


if __name__ == '__main__':
    unittest.main()
