"""Exercise mirror stalls and offline reuse without root, APT or network access."""
import importlib.util
from pathlib import Path
import re
import subprocess
import tempfile
import unittest
from unittest import mock

SCRIPT = Path(__file__).with_name('install-packages.py')
spec = importlib.util.spec_from_file_location('install_packages', SCRIPT)
packages = importlib.util.module_from_spec(spec)
spec.loader.exec_module(packages)


class PackageSetup(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.cache = Path(self.temp.name) / 'cache'
        self.cache.mkdir()
        self.commands = []
        self.fail_downloads = 0
        self.fail_updates = 0
        self.fail_cache = False
        self.fail_install = False

    def fake_run(self, command, seconds):
        self.commands.append((command, seconds))
        if command[:2] == ['rm', '-rf']:
            import shutil
            shutil.rmtree(command[2], ignore_errors=True)
        if '--download-only' in command:
            if '--no-download' in command and self.fail_cache:
                raise subprocess.CalledProcessError(100, command)
            if '--no-download' not in command and self.fail_downloads:
                self.fail_downloads -= 1
                raise subprocess.CalledProcessError(124, command)
        if 'update' in command and self.fail_updates:
            self.fail_updates -= 1
            raise subprocess.CalledProcessError(124, command)
        if command[0] == 'env' and self.fail_install:
            raise subprocess.CalledProcessError(100, command)

    def download(self, arch='amd64'):
        with mock.patch.object(packages, 'run', self.fake_run), \
             mock.patch.object(packages, 'verify_cached_inputs', create=True):
            return packages.download(['ffmpeg'], 'noble', arch, self.cache)

    def test_stalled_download_retries_another_mirror_with_a_short_bound(self):
        self.fail_downloads = 1
        self.assertEqual('https://archive.ubuntu.com/ubuntu', self.download())
        self.assertEqual(2, sum('update' in command for command, _ in self.commands))
        self.assertEqual([75, 75], [seconds for command, seconds in self.commands if '--download-only' in command])
        self.assertIn('signed-by=/usr/share/keyrings/ubuntu-archive-keyring.gpg',
                      (self.cache / 'sources.list').read_text())
        self.assertNotIn('azure', (self.cache / 'sources.list').read_text())

    def test_both_stalled_mirrors_fail_before_any_install(self):
        self.fail_downloads = 2
        with self.assertRaisesRegex(RuntimeError, 'both bounded mirror attempts'):
            self.download()
        self.assertFalse(any(command[0] == 'env' for command, _ in self.commands))
        self.assertEqual(2, sum('--download-only' in command for command, _ in self.commands))

    def test_failed_update_retries_without_downloading_from_partial_lists(self):
        self.fail_updates = 1
        self.download()
        self.assertEqual(1, sum('--download-only' in command for command, _ in self.commands))

    def test_cache_is_reused_without_any_network_operation(self):
        (self.cache / 'sources.list').write_text('cached authenticated Ubuntu sources')
        self.assertRegex(self.download(), r'^cache \(verification \d+\.\d{2}s\)$')
        self.assertEqual(1, len(self.commands))
        self.assertIn('--no-download', self.commands[0][0])

    def test_incomplete_cache_falls_back_to_a_bounded_download(self):
        (self.cache / 'sources.list').write_text('cached')
        self.fail_cache = True
        self.assertEqual('http://azure.archive.ubuntu.com/ubuntu', self.download())
        self.assertEqual(1, sum('update' in command for command, _ in self.commands))

    def test_arm_uses_ports_and_retains_signature_verification_on_fallback(self):
        self.fail_downloads = 1
        self.assertEqual('http://ports.ubuntu.com/ubuntu-ports', self.download('arm64'))
        self.assertNotIn('trusted=yes', (self.cache / 'sources.list').read_text())

    def install(self, installed=False):
        status = subprocess.CompletedProcess([], 0 if installed else 1,
                                             'installed\n' if installed else '', '')
        with mock.patch.object(packages, 'ubuntu', return_value='noble'), \
             mock.patch.object(packages.subprocess, 'check_output', return_value='amd64\n'), \
             mock.patch.object(packages.subprocess, 'run', return_value=status), \
             mock.patch.object(packages, 'run', self.fake_run), \
             mock.patch.object(packages, 'POLICY', Path(self.temp.name) / 'policy-rc.d'):
            return packages.install(['redis-server'], self.cache)

    def test_image_packages_need_neither_network_nor_root(self):
        self.assertEqual('already installed on runner image', self.install(installed=True))
        self.assertEqual([], self.commands)

    def test_failed_offline_install_restores_symlink_service_policy(self):
        policy = Path(self.temp.name) / 'policy-rc.d'
        policy.symlink_to('missing-target')
        self.fail_install = True
        with self.assertRaises(subprocess.CalledProcessError):
            self.install()
        commands = [command for command, _ in self.commands]
        backup = str(self.cache.parent / 'relais-original-policy-rc.d')
        self.assertIn(['cp', '-a', str(policy), backup], commands)
        self.assertIn(['cp', '-a', backup, str(policy)], commands)
        installation = next(command for command in commands if command[0] == 'env')
        self.assertIn('--no-download', installation)
        self.assertNotIn('--download-only', installation)
        self.assertLess(commands.index(['rm', '-f', str(policy)]),
                        next(i for i, command in enumerate(commands) if command[0] == 'install'))

    def test_timeout_owns_root_processes_and_escalates_to_kill(self):
        with mock.patch.object(packages.subprocess, 'run') as run:
            packages.run(['apt-get', 'update'], 45)
        self.assertEqual(['sudo', 'timeout', '--signal=TERM', '--kill-after=5s',
                          '45s', 'apt-get', 'update'], run.call_args.args[0])

    def test_cache_key_changes_with_image_architecture_and_package_set(self):
        with mock.patch.object(packages, 'ubuntu', return_value='noble'), \
             mock.patch.object(packages.subprocess, 'check_output', return_value='amd64\n'), \
             mock.patch.dict(packages.os.environ, {'ImageVersion': 'first'}):
            initial = packages.cache_key(['redis-server', 'ffmpeg'])
            self.assertEqual(initial, packages.cache_key(['ffmpeg', 'redis-server']))
            self.assertNotEqual(initial, packages.cache_key(['redis-server']))
            with mock.patch.dict(packages.os.environ, {'ImageVersion': 'second'}):
                self.assertNotEqual(initial, packages.cache_key(['ffmpeg', 'redis-server']))
            with mock.patch.object(packages.subprocess, 'check_output', return_value='arm64\n'):
                self.assertNotEqual(initial, packages.cache_key(['ffmpeg', 'redis-server']))


    def test_cleanup_failures_do_not_skip_later_steps_or_mask_install_error(self):
        policy = Path(self.temp.name) / 'policy-rc.d'
        policy.symlink_to('missing-target')
        self.fail_install = True
        original_run = self.fake_run
        def fail_cleanup(command, seconds):
            original_run(command, seconds)
            if command[0] == 'env':
                return
            if any(prior[0][0] == 'env' for prior in self.commands):
                if command[0] in ('rm', 'cp'):
                    raise subprocess.CalledProcessError(23, command)
        self.fake_run = fail_cleanup
        with self.assertRaises(subprocess.CalledProcessError) as caught:
            self.install()
        self.assertEqual(100, caught.exception.returncode)
        self.assertEqual('env', caught.exception.cmd[0])
        cleanup = [command[0] for command, _ in self.commands[self.commands.index(
            next(item for item in self.commands if item[0][0] == 'env')) + 1:]]
        self.assertEqual(['rm', 'cp', 'chown'], cleanup)


    def test_cleanup_failure_after_success_is_reported_and_later_steps_still_run(self):
        original_run = self.fake_run
        def fail_cleanup(command, seconds):
            original_run(command, seconds)
            if command[0] == 'rm' and command[1] == '-f':
                if any(prior[0][0] == 'env' for prior in self.commands):
                    raise subprocess.CalledProcessError(23, command)
        self.fake_run = fail_cleanup
        with self.assertRaisesRegex(RuntimeError, 'Ubuntu package cleanup failed'):
            self.install()
        self.assertEqual('chown', self.commands[-1][0][0])


    def test_cache_hit_step_summary_includes_verification_duration(self):
        (self.cache / 'sources.list').write_text('cached authenticated Ubuntu sources')
        summary = Path(self.temp.name) / 'summary'
        with mock.patch.object(packages.sys, 'argv', [str(SCRIPT), 'install', 'ffmpeg']), \
             mock.patch.object(packages, 'install', side_effect=lambda _: self.download()), \
             mock.patch.object(packages.time, 'monotonic', side_effect=[100.0, 101.0, 102.25, 103.0]), \
             mock.patch.dict(packages.os.environ, {'GITHUB_STEP_SUMMARY': str(summary)}):
            packages.main()
        self.assertIn('Ubuntu package setup: cache (verification 1.25s); 3.0s; ffmpeg',
                      summary.read_text())


class SetupIsolation(unittest.TestCase):
    def test_crash_driver_requires_successful_package_setup_even_with_status_override(self):
        # An explicit !cancelled() overrides Actions' implicit success() check.
        # Keep collecting crash evidence after other test failures, but never
        # start the driver when package setup failed.
        workflow = (SCRIPT.parents[2] / '.github/workflows/ci.yml').read_text()
        job = workflow.split('  crash-run:\n', 1)[1].split('\n  relay-continuity:', 1)[0]
        setup = job.split('      - name: Install Redis and ffmpeg\n', 1)[1].split('      - name:', 1)[0]
        self.assertIn('id: install-packages', setup)
        driver = job.split('      - name: Run twenty interleaved SIGKILL trials (ten per mode)\n', 1)[1].split('      - name:', 1)[0]
        self.assertIn("!cancelled() && steps.install-packages.outcome == 'success'", driver)


class PackageAction(unittest.TestCase):
    def test_every_package_action_caller_bounds_the_whole_setup_step(self):
        root = SCRIPT.parents[2]
        callers = 0
        for path in (root / '.github/workflows').glob('*.yml'):
            workflow = path.read_text()
            self.assertNotIn('run: python3 scripts/ci/install-packages.py', workflow)
            for step in re.split(r'(?m)^      - ', workflow):
                if 'uses: ./.github/actions/ubuntu-packages' not in step:
                    continue
                callers += 1
                self.assertIn('timeout-minutes: 10', step, str(path))
                self.assertRegex(step, r'with:\n          packages: \S')
        self.assertGreater(callers, 0)

    def test_composite_keeps_cache_segment_bound_and_exports_key_for_valkey(self):
        action = (SCRIPT.parents[2] / '.github/actions/ubuntu-packages/action.yml').read_text()
        self.assertNotIn('timeout-minutes:', action)
        self.assertIn("SEGMENT_DOWNLOAD_TIMEOUT_MINS: '1'", action)
        self.assertIn('value: ${{ steps.package-cache-key.outputs.key }}', action)
        self.assertRegex(action, r'uses: actions/cache@[0-9a-f]{40} ')
