"""Exercise disposable Redis cleanup on success, failure and interruption."""
import locale
from pathlib import Path
import os
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
WRAPPER = ROOT / 'scripts/dev/with-redis.py'


class RedisLifetime(unittest.TestCase):
    def test_missing_binary_has_repair_message(self):
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run([sys.executable, str(WRAPPER), 'unused'],
                                    env=dict(os.environ, PATH=directory),
                                    capture_output=True, text=True)
        self.assertEqual(result.returncode, 1)
        self.assertIn('redis-server is required on PATH', result.stderr)

    def exercise(self, exit_code=0, interrupt=None):
        if shutil.which('redis-server') is None:
            self.skipTest('redis-server is unavailable for wrapper integration')
        previous_locale = locale.setlocale(locale.LC_ALL)
        try:
            locale.setlocale(locale.LC_ALL, 'en_US.UTF-8')
        except locale.Error:
            self.skipTest('en_US.UTF-8 is unavailable for wrapper integration')
        finally:
            locale.setlocale(locale.LC_ALL, previous_locale)
        command = ('import os,time; '
                   'assert os.environ["RELAIS_TEST_REDIS_REQUIRE"] == "1"; '
                   'assert os.environ["RELAIS_TEST_REDIS_TLS_REQUIRE"] == "1"; '
                   'assert os.environ["LC_ALL"] == "en_US.UTF-8"; '
                   'print("COMMAND_READY", flush=True); '
                   + ('time.sleep(30)' if interrupt else f'raise SystemExit({exit_code})'))
        with tempfile.TemporaryDirectory() as directory:
            process = subprocess.Popen([sys.executable, str(WRAPPER), sys.executable, '-c', command],
                                       env=dict(os.environ, TMPDIR=directory),
                                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
            try:
                # Startup and child output are line-buffered. communicate has a
                # timeout so a cleanup regression cannot hang the test suite.
                first = process.stdout.readline()
                match = re.search(r'127\.0\.0\.1:(\d+) \(pid (\d+)\)', first)
                self.assertIsNotNone(match, first)
                port, pid = map(int, match.groups())
                self.assertNotEqual(port, 6379)
                if interrupt:
                    self.assertIn('COMMAND_READY', process.stdout.readline())
                    process.send_signal(interrupt)
                rest, _ = process.communicate(timeout=20)
                self.assertEqual(process.returncode, 128 + interrupt if interrupt else exit_code, rest)
                self.assertIn('pid ' + str(pid) + ' reaped', rest)
                with self.assertRaises(ProcessLookupError):
                    os.kill(pid, 0)
                with socket.socket() as client:
                    self.assertNotEqual(client.connect_ex(('127.0.0.1', port)), 0)
                self.assertFalse(list(Path(directory).iterdir()), 'Redis temporary directory leaked')
            finally:
                if process.poll() is None:
                    process.terminate()
                    process.communicate(timeout=20)

    def test_success(self):
        self.exercise()

    def test_failure(self):
        self.exercise(exit_code=7)

    def test_interrupt(self):
        self.exercise(interrupt=signal.SIGINT)

    def test_terminate(self):
        self.exercise(interrupt=signal.SIGTERM)


if __name__ == '__main__':
    unittest.main()
