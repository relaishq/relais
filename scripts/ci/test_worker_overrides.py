"""Pre-launch driver validation. Build with make build before running."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

BIN = Path(os.environ.get('DRIVER_BIN_DIR', 'bin')).resolve()


class WorkerOverrides(unittest.TestCase):
    def run_driver(self, name, *flags):
        result = subprocess.run([str(BIN / name), '-bin=/nonexistent/relais-bin', *flags],
                                capture_output=True, text=True, check=False)
        self.assertNotEqual(0, result.returncode)
        return result.stdout + result.stderr

    def test_all_drivers_require_explicit_agent(self):
        for name in ('crash-run', 'relais-load', 'relais-demo'):
            with self.subTest(driver=name):
                self.assertIn('-agent is required with -worker-bin',
                              self.run_driver(name, '-worker-bin=/bin/sh'))

    def test_echo_specific_crash_modes_refuse_custom(self):
        for flag in ('-sigterm', '-buffer', '-buffer-lossy', '-compare-buffer', '-relay-restart', '-relay-standby', '-scenario=move'):
            with self.subTest(flag=flag):
                self.assertIn('these modes require echo audio semantics',
                              self.run_driver('crash-run', '-worker-bin=/bin/sh', '-agent=custom/v1', flag))

    def test_unknown_custom_store_stall_refused(self):
        self.assertIn('needs delayed-loopback/v1 semantics', self.run_driver('crash-run', '-agent=custom/v1', '-store-stall=live'))

    def test_local_start_requires_explicit_agent_before_state_creation(self):
        with tempfile.TemporaryDirectory() as tmp:
            state = str(Path(tmp) / 'state')
            env = dict(os.environ, AGENT='', WORKER_BIN='')
            result = subprocess.run(['bash', os.environ.get('LOCAL_START_SCRIPT', 'deploy/local/start.sh'), '-worker-bin', '/bin/sh',
                                     '-bin', '/nonexistent/relais-bin', '-state', state],
                                    env=env, capture_output=True, text=True, check=False)
            self.assertNotEqual(0, result.returncode)
            self.assertIn('-agent is required with -worker-bin', result.stderr)
            self.assertFalse(Path(state).exists())
