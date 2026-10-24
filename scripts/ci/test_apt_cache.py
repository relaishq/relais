"""Real signed fixtures exercise the cache-authentication chain without root/APT."""
from datetime import datetime, timedelta, timezone
from email.utils import format_datetime
import hashlib
import re
import importlib.util
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock

import apt_cache

SCRIPT = Path(__file__).with_name('install-packages.py')
spec = importlib.util.spec_from_file_location('cache_installer', SCRIPT)
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)


class AuthenticatedCache(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not shutil.which('gpg') or not shutil.which('gpgv'):
            raise RuntimeError('cache authentication tests require gpg and gpgv')
        cls.temp = tempfile.TemporaryDirectory(dir='/tmp', prefix='apt-cache-')
        cls.addClassCleanup(cls.temp.cleanup)
        cls.root = Path(cls.temp.name)
        cls.home = cls.root / 'gpg'
        cls.home.mkdir(mode=0o700)
        def gpg(*args):
            return subprocess.run(['gpg', '--homedir', str(cls.home), '--batch',
                                   '--pinentry-mode', 'loopback', '--passphrase', '', *args],
                                  check=True, capture_output=True, timeout=30)
        cls.gpg = staticmethod(gpg)
        cls.addClassCleanup(lambda: subprocess.run(
            ['gpgconf', '--homedir', str(cls.home), '--kill', 'gpg-agent'],
            check=False, capture_output=True, timeout=10))
        gpg('--quick-generate-key', 'APT cache fixture', 'ed25519', 'sign', '0')
        cls.keyring = cls.root / 'trusted.gpg'
        cls.keyring.write_bytes(gpg('--export').stdout)
        cls.fixture = cls.root / 'fixture'
        (cls.fixture / 'lists').mkdir(parents=True)
        (cls.fixture / 'archives').mkdir()
        cls.archive_name = 'fixture_1%3a1.0_amd64.deb'
        payload = b'authenticated package bytes'
        (cls.fixture / 'archives' / cls.archive_name).write_bytes(payload)
        index = ('Package: fixture\nVersion: 1:1.0\nArchitecture: amd64\n'
                 'Filename: pool/main/f/fixture/fixture_1.0_amd64.deb\n'
                 f'Size: {len(payload)}\nSHA256: {hashlib.sha256(payload).hexdigest()}\n\n').encode()
        cls.prefix = 'azure.archive.ubuntu.com_ubuntu_dists_'
        cls.release_bodies = {}
        for suffix in ('', '-updates', '-security'):
            suite = 'noble' + suffix
            (cls.fixture / 'lists' / (cls.prefix + suite + '_main_binary-amd64_Packages')).write_bytes(index)
            release = cls.root / 'Release'
            body = (f'Suite: {suite}\nCodename: noble\n'
                    f'Date: {format_datetime(datetime.now(timezone.utc), usegmt=True)}\nSHA256:\n '
                    f'{hashlib.sha256(index).hexdigest()} {len(index)} main/binary-amd64/Packages\n')
            cls.release_bodies[suite] = body
            release.write_text(body)
            gpg('--armor', '--output', str(cls.fixture / 'lists' / (cls.prefix + suite + '_InRelease')),
                '--clearsign', str(release))
        (cls.fixture / 'sources.list').write_text(apt_cache.sources(
            'http://azure.archive.ubuntu.com/ubuntu', 'noble', 'amd64'))

    def setUp(self):
        self.case = tempfile.TemporaryDirectory(dir=self.root)
        self.addCleanup(self.case.cleanup)
        self.cache = Path(self.case.name) / 'cache'
        shutil.copytree(self.fixture, self.cache)
        self.commands = []

    def verify(self, cache=None, *_args):
        apt_cache.verify_cache(cache or self.cache, 'noble', 'amd64', keyring=self.keyring)

    def fake_privileged_command(self, command, seconds):
        self.commands.append(command)
        if command[:2] == ['rm', '-rf']:
            shutil.rmtree(command[2], ignore_errors=True)

    def download(self):
        with mock.patch.object(installer, 'verify_cached_inputs', self.verify, create=True), \
             mock.patch.object(installer, 'run', self.fake_privileged_command):
            return installer.download(['fixture'], 'noble', 'amd64', self.cache)

    def assert_mirror_fallback(self):
        self.assertEqual('http://azure.archive.ubuntu.com/ubuntu', self.download())
        self.assertTrue(any('update' in command for command in self.commands))
        self.assertFalse(any('--download-only' in command and '--no-download' in command
                             for command in self.commands))
        # The mirror cannot reuse same-sized, unauthenticated cached archives.
        self.assertFalse((self.cache / 'archives' / self.archive_name).exists())

    def test_valid_signature_index_and_archive_reuse_cache_without_network(self):
        self.assertRegex(self.download(), r'^cache \(verification \d+\.\d{2}s\)$')
        self.assertEqual(1, len(self.commands))
        self.assertIn('--no-download', self.commands[0])

    def test_same_size_tampered_deb_falls_back_to_mirrors(self):
        path = self.cache / 'archives' / self.archive_name
        path.write_bytes(b'x' * path.stat().st_size)
        self.assert_mirror_fallback()

    def test_same_size_tampered_packages_list_falls_back_to_mirrors(self):
        path = self.cache / 'lists' / (self.prefix + 'noble_main_binary-amd64_Packages')
        path.write_bytes(path.read_bytes().replace(b'fixture', b'changed'))
        self.assert_mirror_fallback()

    def test_bad_inrelease_signature_falls_back_to_mirrors(self):
        path = self.cache / 'lists' / (self.prefix + 'noble_InRelease')
        path.write_bytes(path.read_bytes().replace(b'Codename: noble', b'Codename: other'))
        self.assert_mirror_fallback()

    def test_self_consistent_list_and_deb_tampering_still_needs_ubuntu_signature(self):
        archive = self.cache / 'archives' / self.archive_name
        original = archive.read_bytes()
        archive.write_bytes(b'x' * len(original))
        index = self.cache / 'lists' / (self.prefix + 'noble_main_binary-amd64_Packages')
        index.write_text(index.read_text().replace(hashlib.sha256(original).hexdigest(),
                                                 hashlib.sha256(archive.read_bytes()).hexdigest()))
        self.assert_mirror_fallback()

    def test_unrecognized_list_and_untrusted_sources_are_rejected(self):
        (self.cache / 'lists' / 'unknown_Packages').write_text('untrusted')
        with self.assertRaisesRegex(ValueError, 'not authenticated'):
            self.verify()
        (self.cache / 'lists' / 'unknown_Packages').unlink()
        source = self.cache / 'sources.list'
        source.write_text(source.read_text().replace('target=Packages', 'trusted=yes'))
        with self.assertRaisesRegex(ValueError, 'allowed Ubuntu sources'):
            self.verify()

    def test_symlinked_index_is_rejected(self):
        path = self.cache / 'lists' / (self.prefix + 'noble_main_binary-amd64_Packages')
        target = Path(self.case.name) / 'original-index'
        path.rename(target)
        path.symlink_to(target)
        with self.assertRaisesRegex(ValueError, 'not a regular cache file'):
            self.verify()


    def sign_date(self, suite, date):
        release = Path(self.case.name) / 'Release'
        release.write_text(re.sub(r'(?m)^Date: .*$',
                                  'Date: ' + format_datetime(date, usegmt=True),
                                  self.release_bodies[suite]))
        self.gpg('--yes', '--armor', '--output',
                 str(self.cache / 'lists' / (self.prefix + suite + '_InRelease')),
                 '--clearsign', str(release))

    def test_stale_security_snapshot_falls_back_to_mirrors(self):
        self.sign_date('noble-security', datetime.now(timezone.utc) - timedelta(days=10))
        self.assert_mirror_fallback()

    def test_stale_updates_snapshot_falls_back_to_mirrors(self):
        self.sign_date('noble-updates', datetime.now(timezone.utc) - timedelta(days=10))
        self.assert_mirror_fallback()

    def test_future_signed_date_falls_back_to_mirrors(self):
        self.sign_date('noble', datetime.now(timezone.utc) + timedelta(minutes=10))
        self.assert_mirror_fallback()

    def test_old_base_release_date_is_accepted(self):
        self.sign_date('noble', datetime(2024, 4, 25, tzinfo=timezone.utc))
        self.assertRegex(self.download(), r'^cache \(verification \d+\.\d{2}s\)$')
        self.assertFalse(any('update' in command for command in self.commands))

    def test_small_future_clock_skew_is_accepted(self):
        self.sign_date('noble-security', datetime.now(timezone.utc) + timedelta(minutes=3))
        self.assertRegex(self.download(), r'^cache \(verification \d+\.\d{2}s\)$')
