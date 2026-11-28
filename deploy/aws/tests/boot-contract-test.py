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

spec = importlib.util.spec_from_file_location('install_test', Path(__file__).with_name('install-test.py'))
install = importlib.util.module_from_spec(spec)
spec.loader.exec_module(install)
InstallBootContracts = install.InstallBootContracts


class BootContracts(unittest.TestCase):
    def launcher(self, component, extra=None):
        # Capture the exact launcher contract without running a media process.
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / ('relais-' + ('relay' if component == 'relay-standby' else component))
            binary.write_text('#!/bin/bash\nprintf "%s\\n" "$@"\n')
            binary.chmod(0o755)
            env = {'PATH': '/usr/bin:/bin', 'RELAIS_BIN_DIR': directory,
                   'RELAY_PRIVATE': '10.42.0.10', 'RELAY_PUBLIC': '192.0.2.10',
                   'CONTROL_PRIVATE': '10.42.10.20', 'WORKER_ONE': '10.42.10.11',
                   'WORKER_TWO': '10.42.11.12', 'WORKER_CALL_CAP': '2'}
            env.update(extra or {})
            return subprocess.check_output(['bash', str(ROOT / 'boot/run.sh'), component], env=env, text=True).splitlines()

    def test_single_relay_launcher_contract(self):
        self.assertEqual(self.launcher('control'), ['-http', '10.42.10.20:24710', '-relay',
            'http://10.42.0.10:24700', '-worker-call-cap', '2', '-worker',
            'w1=http://10.42.10.11:24701', '-worker', 'w2=http://10.42.11.12:24701'])
        args = self.launcher('relay-standby')
        self.assertEqual(args[-3:], ['-relay-lease', 'preview', '-standby'])

    def test_relay_set_launcher_uses_private_urls_and_distinct_leases(self):
        args = self.launcher('control', {'RELAY_TWO_PRIVATE': '10.42.1.10'})
        self.assertEqual([args[i+1] for i, x in enumerate(args) if x == '-relay'],
                         ['preview=http://10.42.0.10:24700', 'preview-two=http://10.42.1.10:24700'])
        for component in ('relay', 'relay-standby'):
            args = self.launcher(component, {'RELAY_PRIVATE': '10.42.1.10', 'RELAY_SLOT': 'preview-two', 'RELAY_TWO_PRIVATE': '10.42.1.10'})
            self.assertEqual(args[args.index('-relay-lease')+1], 'preview-two')
            self.assertEqual(args[args.index('-leg')+1], '10.42.1.10:24781')
            self.assertEqual(args[args.index('-advertise-media')+1], '192.0.2.10:24780')
            self.assertEqual(args[args.index('-relay-slot')+1], 'preview-two')

    def test_unknown_component_is_a_permanent_launcher_refusal(self):
        result = subprocess.run(['bash', str(ROOT / 'boot/run.sh'), 'unknown-component'], capture_output=True, text=True)
        self.assertEqual(result.returncode, 78)
        self.assertIn('unknown component; start refused', result.stderr)

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

    def test_acl_selectors_refuse_old_or_unknown_redis_runtime(self):
        trial = boot.Trial.__new__(boot.Trial)
        for version in ('Redis server v=6.2.16 sha=00000000', 'unrecognized server version'):
            with patch.object(boot.subprocess, 'check_output', return_value=version):
                with self.assertRaisesRegex(AssertionError, r'Redis 7\.0\+ is required'):
                    trial.cluster()

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
