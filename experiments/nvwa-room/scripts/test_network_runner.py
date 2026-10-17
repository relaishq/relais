"""Run the actual Python runner against controlled, owned driver processes."""
import json
import os
import pathlib
import signal
import subprocess
import sys
import tempfile
import time
import unittest

RUNNER = pathlib.Path(__file__).with_name('run-network.py').resolve()
FAKE = '''#!/usr/bin/env python3
import argparse,json,pathlib,signal,sys,time
p=argparse.ArgumentParser();p.add_argument('-out');a,_=p.parse_known_args()
out=pathlib.Path(a.out);out.mkdir()
(out/'cell-status.json').write_text(json.dumps({'phase':'startup','ok':False}))
if 'startup-failure' in str(out): sys.exit(1)
def stopped(number,_):
 (out/'cleaned-after-signal').write_text(str(number))
 sys.exit(128+number)
signal.signal(signal.SIGTERM,stopped);signal.signal(signal.SIGINT,stopped)
(out/'ready').touch()
while True:time.sleep(.01)
'''


class RunnerTests(unittest.TestCase):
    def prepare(self, root, name):
        build = root / 'build'
        (build / 'bin').mkdir(parents=True)
        driver = build / 'bin/network-driver'
        driver.write_text(FAKE)
        driver.chmod(0o700)
        out = root / name
        return [sys.executable, str(RUNNER), '--scratch', str(build), '--out', str(out),
                '--count', '4', '--shutdown-grace', '2'], out

    def test_stops_after_two_consecutive_startup_failures(self):
        with tempfile.TemporaryDirectory() as folder:
            cmd, out = self.prepare(pathlib.Path(folder), 'startup-failure')
            run = subprocess.run(cmd, capture_output=True, text=True, timeout=5)
            self.assertNotEqual(run.returncode, 0)
            self.assertEqual(len(json.loads((out / 'ledger.json').read_text())), 2)
            self.assertEqual(json.loads((out / 'runner-status.json').read_text())['stopped_reason'], '2 consecutive startup failures')

    def test_term_and_int_forward_to_owned_driver_and_wait_for_cleanup(self):
        for number in [signal.SIGTERM, signal.SIGINT]:
            with self.subTest(number=number), tempfile.TemporaryDirectory() as folder:
                cmd, out = self.prepare(pathlib.Path(folder), 'signal-case')
                with subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL) as process:
                    deadline = time.monotonic() + 5
                    while not list(out.glob('*/ready')):
                        if time.monotonic() > deadline:
                            process.kill()
                            self.fail('controlled driver did not start')
                        time.sleep(.01)
                    process.send_signal(number)
                    self.assertEqual(process.wait(timeout=5), 128 + number)
                    self.assertEqual(len(list(out.glob('*/cleaned-after-signal'))), 1)
                    self.assertEqual(len(json.loads((out / 'ledger.json').read_text())), 1)

    def test_budget_terminates_driver_gracefully(self):
        with tempfile.TemporaryDirectory() as folder:
            cmd, out = self.prepare(pathlib.Path(folder), 'budget-case')
            run = subprocess.run(cmd + ['--budget-seconds', '.3'], capture_output=True, text=True, timeout=5)
            self.assertNotEqual(run.returncode, 0)
            self.assertEqual(len(list(out.glob('*/cleaned-after-signal'))), 1)
            self.assertEqual(json.loads((out / 'ledger.json').read_text())[0]['exit_code'], 124)


if __name__ == '__main__':
    unittest.main()
