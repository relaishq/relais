#!/usr/bin/env python3
"""Exercise fail-closed host installation without cloud, packages or services."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class InstallBootContracts(unittest.TestCase):
    def install_boot(self, components, ssm, fail='', fail_code=42):
        """Real user-data/loader/install and exit trap, with local host commands."""
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            image, path = work / 'image', work / 'path'
            image.mkdir()
            path.mkdir()
            for tool in ('bash', 'cat', 'chmod', 'install'):
                (path / tool).symlink_to(shutil.which(tool))
            (path / 'python3').symlink_to(Path(sys.executable).resolve())
            command = work / 'command.py'
            command.write_text('#!' + str(Path(sys.executable).resolve()) + '\n' + '''
import json, os, sys
from pathlib import Path
name = Path(sys.argv[0]).name
args = sys.argv[1:]
if name == 'aws':
    resource = args[args.index('--logical-resource-id') + 1]
    print(json.loads(Path(os.environ['CLOUD']).read_text())[resource])
    sys.exit(0)
if name == 'cfn-signal':
    step = 'signal ' + args[args.index('-e') + 1]
elif name == 'systemctl':
    step = ' '.join(args)
elif name in ('dnf', 'nft', 'timeout'):
    step = name + ' ' + ' '.join(args)
else:
    step = name.removesuffix('.sh').removesuffix('.py')
with open(os.environ['TRACE'], 'a') as stream:
    stream.write(step + '\\n')
if step == os.environ['FAIL_STEP']:
    sys.exit(int(os.environ['FAIL_CODE']))
if name == 'timeout':
    os.execv(str(Path(os.environ['FIXTURE_PATH']) / args[2]), args[2:])
''')
            command.chmod(0o755)
            for tool in ('aws', 'systemctl', 'sysctl', 'readiness', 'dnf', 'nft', 'timeout'):
                (path / tool).symlink_to(command)
            signaler = image / 'opt/aws/bin/cfn-signal'
            signaler.parent.mkdir(parents=True)
            signaler.symlink_to(command)
            files = {}
            for name in ('load.py', 'install.sh'):
                files['/opt/relais/boot/' + name] = {
                    'content': (ROOT / 'boot' / name).read_text(), 'mode': '000755'}
            for name in ('download.sh', 'prepare.py'):
                files['/opt/relais/boot/' + name] = {'content': command.read_text(), 'mode': '000755'}
            files['/etc/sysctl.d/60-relais-udp.conf'] = {'content': '# fixture\n', 'mode': '000644'}
            node = {'version': '0.4.0', 'architecture': 'arm64', 'sha256': 'a' * 64,
                    'components': components, 'ssm': ssm}
            cloud = work / 'cloud.json'
            cloud.write_text(json.dumps({
                'BootFiles': json.dumps({'RelaisFiles': files}),
                'NODE_RESOURCE': json.dumps({'RelaisFiles': {
                    '/etc/relais/node.json': {'content': json.dumps(node), 'mode': '000600'},
                    '/etc/relais/node.env': {'content': '# fixture\n', 'mode': '000600'}}})}))
            trace = work / 'trace'
            env = {**os.environ, 'PATH': str(path), 'FIXTURE_PATH': str(path),
                   'RELAIS_INSTALL_ROOT': str(image), 'RELAIS_TEST_READY': str(path / 'readiness'),
                   'CLOUD': str(cloud), 'TRACE': str(trace), 'FAIL_STEP': fail, 'FAIL_CODE': str(fail_code)}
            result = subprocess.run(['bash', str(ROOT / 'boot/user-data.sh')],
                                    env=env, capture_output=True, text=True, timeout=10)
            return result, trace.read_text().splitlines()

    @staticmethod
    def non_relay_steps(components, ssm):
        steps = ['download', 'prepare', 'sysctl', 'enable chronyd.service', 'restart chronyd.service']
        steps += (['disable --now amazon-ssm-agent.service', 'mask amazon-ssm-agent.service']
                  if ssm == 'false' else ['enable --now amazon-ssm-agent.service'])
        return [*steps, 'daemon-reload', *(f'enable --now relais-{c}.service' for c in components),
                *(f'is-active --quiet relais-{c}.service' for c in components), 'readiness', 'signal 0']

    def test_non_relay_boot_characterization(self):
        for components in (['worker'], ['control'], ['signal'], ['control', 'signal']):
            for ssm in ('false', 'true'):
                with self.subTest(components=components, ssm=ssm):
                    result, trace = self.install_boot(components, ssm)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(trace, self.non_relay_steps(components, ssm))

    def test_non_relay_boot_fails_closed_at_each_step(self):
        for ssm in ('false', 'true'):
            steps = self.non_relay_steps(['worker'], ssm)
            for i, step in enumerate(steps[:-1]):
                with self.subTest(ssm=ssm, failure=step):
                    result, trace = self.install_boot(['worker'], ssm, fail=step)
                    self.assertEqual(result.returncode, 42, result.stderr)
                    self.assertEqual(trace, [*steps[:i+1], 'signal 42'])
                    self.assertNotIn('boot services started and ready', result.stdout)

    def test_ssm_relay_installs_and_checks_nft_before_starting_services(self):
        result, trace = self.install_boot(['relay'], 'true')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(trace, [
            'download', 'timeout --kill-after=10s 120s dnf install -y nftables',
            'dnf install -y nftables', 'nft --version', 'prepare', 'sysctl',
            'enable chronyd.service', 'restart chronyd.service',
            'enable --now amazon-ssm-agent.service', 'daemon-reload',
            'enable --now relais-relay.service', 'enable --now relais-relay-standby.service',
            'is-active --quiet relais-relay.service', 'is-active --quiet relais-relay-standby.service',
            'readiness', 'signal 0'])

    def test_relay_without_ssm_installs_nothing(self):
        result, trace = self.install_boot(['relay'], 'false')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(any(step.startswith(('timeout ', 'dnf ', 'nft ')) for step in trace))
        self.assertEqual(trace[-1], 'signal 0')

    def test_nft_install_timeout_and_version_failure_signal_boot_failure(self):
        steps = ['download', 'timeout --kill-after=10s 120s dnf install -y nftables',
                 'dnf install -y nftables', 'nft --version']
        for i, step in enumerate(steps[1:], 1):
            failure_code = 124 if step.startswith('timeout ') else 42
            with self.subTest(failure=step):
                result, trace = self.install_boot(['relay'], 'true', fail=step, fail_code=failure_code)
                self.assertEqual(result.returncode, failure_code, result.stderr)
                self.assertEqual(trace, [*steps[:i+1], f'signal {failure_code}'])
                self.assertNotIn('boot services started and ready', result.stdout)


if __name__ == '__main__':
    unittest.main()
