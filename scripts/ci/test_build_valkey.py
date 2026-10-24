"""Check the actual setup script's cache and checksum failure paths offline."""
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('build-valkey.sh').resolve()


class ValkeySetup(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.tools = self.root / 'tools'
        self.tools.mkdir()
        # No source download or build is allowed in the cache path. A cold
        # download writes corrupt bytes to exercise the real sha256sum check.
        self.tool('curl', '#!/bin/sh\nfor last; do :; done\nprintf corrupt > "$last"\n')
        self.tool('make', '#!/bin/sh\necho unexpected-build >&2\nexit 99\n')
        self.env = dict(os.environ, RUNNER_TEMP=str(self.root),
                        GITHUB_PATH=str(self.root / 'path'),
                        GITHUB_STEP_SUMMARY=str(self.root / 'summary'),
                        VALKEY_VERSION='9.1.2', VALKEY_SHA256='0' * 64,
                        PATH=str(self.tools) + os.pathsep + os.environ['PATH'])

    def tool(self, name, content):
        path = self.tools / name
        path.write_text(content)
        path.chmod(0o755)

    def cached_binaries(self):
        binaries = self.root / 'valkey-bin'
        binaries.mkdir()
        checksums = []
        for name in ('redis-server', 'redis-cli'):
            path = binaries / name
            path.write_text('#!/bin/sh\necho Valkey-9.1.2\n')
            path.chmod(0o755)
            checksums.append(f'{hashlib.sha256(path.read_bytes()).hexdigest()}  {name}\n')
        (binaries / 'SHA256SUMS').write_text(''.join(checksums))
        return binaries

    def run_setup(self):
        return subprocess.run(['bash', str(SCRIPT)], env=self.env,
                              capture_output=True, text=True, check=False)

    def test_cache_hit_checks_real_binaries_and_exports_their_directory(self):
        binaries = self.cached_binaries()
        self.tool('curl', '#!/bin/sh\necho unexpected-download >&2\nexit 98\n')
        result = self.run_setup()
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertIn('Using cached pinned Valkey binaries', result.stdout)
        self.assertEqual(str(binaries) + '\n', (self.root / 'path').read_text())
        self.assertIn('exit 0', (self.root / 'summary').read_text())

    def test_corrupt_source_fails_checksum_before_extraction_or_build(self):
        result = self.run_setup()
        self.assertNotEqual(0, result.returncode)
        self.assertIn('Valkey setup failed', result.stdout)
        self.assertIn('checksum', result.stderr)
        self.assertNotIn('unexpected-build', result.stderr)
        self.assertFalse((self.root / 'path').exists())

    def test_corrupt_cache_fails_checksum_before_exporting_path(self):
        binaries = self.cached_binaries()
        (binaries / 'redis-cli').write_text('#!/bin/sh\necho changed\n')
        result = self.run_setup()
        self.assertNotEqual(0, result.returncode)
        self.assertIn('Valkey setup failed', result.stdout)
        self.assertFalse((self.root / 'path').exists())
