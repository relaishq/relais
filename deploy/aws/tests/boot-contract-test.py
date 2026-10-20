#!/usr/bin/env python3
"""Fast regression checks for boot permissions, diagnostics and fixed-port safety."""
import contextlib
import copy
import importlib.util
import io
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from template import ROOT, load
from policy import check_component_ports

spec = importlib.util.spec_from_file_location('boot_test', Path(__file__).with_name('boot-test.py'))
boot = importlib.util.module_from_spec(spec)
spec.loader.exec_module(boot)


class BootContracts(unittest.TestCase):
    def test_program_access_rejects_owner_only_directories(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for directory in ('opt/relais', 'opt/relais/boot', 'opt/relais/bin'):
                (root / directory).mkdir(parents=True, exist_ok=True)
                (root / directory).chmod(0o755)
            for relative in ('opt/relais/boot/run.sh', *(f'opt/relais/bin/relais-{name}' for name in ('relay', 'worker', 'control', 'signal', 'load'))):
                (root / relative).touch(mode=0o755)
            boot.check_program_access(root)
            for relative in ('opt', 'opt/relais', 'opt/relais/boot', 'opt/relais/bin'):
                (root / relative).chmod(0o700)
                with self.assertRaisesRegex(AssertionError, 'service users cannot'):
                    boot.check_program_access(root)
                (root / relative).chmod(0o755)

    def test_permission_errors_in_any_service_log_refuse_success(self):
        with tempfile.TemporaryDirectory() as temporary:
            trial = boot.Trial.__new__(boot.Trial)
            trial.args = SimpleNamespace(systemd=False)
            log = Path(temporary) / 'worker.log'
            trial.logs = {'worker': log}
            for message in ('NOPERM command denied', 'WRONGPASS invalid password', 'NOAUTH Authentication required',
                            'no permissions to access a key', "can't run this command", "ERR The user executing the script can't access at least one of the keys"):
                log.write_text(message)
                with self.assertRaisesRegex(AssertionError, 'Redis permission error in worker'):
                    trial.assert_no_permission_errors()
            log.write_text('worker started; request completed')
            trial.assert_no_permission_errors()

    def test_component_ports_reject_ephemeral_template_or_launcher_values(self):
        template = load()
        launcher = (ROOT / 'boot/run.sh').read_text()
        check_component_ports(template, launcher)
        changed = copy.deepcopy(template)
        changed['Resources']['PublicRelay']['Properties']['FromPort'] = 34780
        with self.assertRaisesRegex(AssertionError, 'unreserved Linux ephemeral range'):
            check_component_ports(changed, launcher)
        with self.assertRaisesRegex(AssertionError, 'unreserved Linux ephemeral range'):
            check_component_ports(template, launcher.replace(':-24780}', ':-34780}'))

    def test_systemd_failure_report_is_complete_and_redacted(self):
        with tempfile.TemporaryDirectory() as temporary:
            trial = boot.Trial.__new__(boot.Trial)
            trial.work = Path(temporary)
            trial.args = SimpleNamespace(systemd=True, diagnostics_dir=str(trial.work / 'redacted'))
            secret = 'test-generated-secret-value-should-never-be-printed'
            trial.passwords = {'relay': secret}
            trial.units = ['relais-test-relay.service', 'relais-prepare.service']
            trial.logs = {}
            commands = []
            def command(argv, **kwargs):
                commands.append(argv)
                return subprocess.CompletedProcess(argv, 1,
                    'unit output ' + secret + '\nRELAIS_SESSIONSTORE_KEY=another-secret\nAuthorization: Bearer auth-value\n', '')
            output = io.StringIO()
            with patch.object(boot.subprocess, 'run', side_effect=command), contextlib.redirect_stdout(output):
                trial.diagnostics('startup', RuntimeError('failure ' + secret))
            artifact = trial.work / 'redacted/boot-startup.log'
            for text in (output.getvalue(), artifact.read_text()):
                self.assertNotIn(secret, text)
                self.assertNotIn('another-secret', text)
                self.assertNotIn('auth-value', text)
                self.assertIn('<REDACTED>', text)
            units = {p.name for p in (ROOT / 'units').glob('*.service')}
            units |= {'relais-test-' + n + '.service' for n in ('relay', 'relay-standby', 'worker', 'worker-two', 'control', 'signal')}
            for unit in units:
                self.assertIn(['systemctl', 'status', '--no-pager', '--full', unit], commands)
                self.assertIn(['journalctl', '--no-pager', '-u', unit, '--output=cat'], commands)
                self.assertIn(['systemctl', 'cat', '--no-pager', unit], commands)
            self.assertIn(['ss', '-lntup'], commands)
            self.assertEqual(artifact.stat().st_mode & 0o777, 0o644)
            self.assertEqual(artifact.parent.stat().st_mode & 0o777, 0o755)
            self.assertFalse(any(argv[0] in ('cat', 'env', 'printenv') for argv in commands))

    def test_installed_unit_dump_does_not_read_environment_files(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            unit = root / 'relais-test-relay.service'
            environment = root / 'relay.env'
            environment.write_text('RELAIS_REDIS_PASSWORD=do-not-read-this-file\n')
            unit.write_text('[Service]\nEnvironmentFile=' + str(environment) + '\n')
            dropins = Path(str(unit) + '.d')
            dropins.mkdir()
            (dropins / 'fixture.conf').write_text('[Service]\nRestart=no\n')
            text = '\n'.join(boot.installed_unit_text(root))
            self.assertIn(unit.read_text(), text)
            self.assertIn('Restart=no', text)
            self.assertNotIn('do-not-read-this-file', text)

    def test_diagnostic_command_failure_does_not_abort_report(self):
        with tempfile.TemporaryDirectory() as temporary:
            trial = boot.Trial.__new__(boot.Trial)
            trial.work = Path(temporary)
            trial.args = SimpleNamespace(systemd=True, diagnostics_dir=None)
            trial.passwords, trial.logs, trial.units = {}, {}, []
            with patch.object(boot.subprocess, 'run', side_effect=OSError('not available')), contextlib.redirect_stdout(io.StringIO()):
                trial.diagnostics('startup', RuntimeError('original failure'))
            text = (trial.work / 'diagnostics/boot-startup.log').read_text()
            self.assertIn('original failure', text)
            self.assertIn('diagnostic command unavailable', text)


if __name__ == '__main__':
    unittest.main()
