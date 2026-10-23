"""Authenticate a cached Ubuntu APT snapshot before privileged APT reads it."""
from collections import defaultdict
from datetime import datetime, timedelta, timezone
from email.utils import parsedate_to_datetime
import hashlib
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from urllib.parse import unquote

KEYRING = '/usr/share/keyrings/ubuntu-archive-keyring.gpg'
COMPONENTS = 'main restricted universe multiverse'


def mirrors(arch):
    return (['https://azure.archive.ubuntu.com/ubuntu', 'https://archive.ubuntu.com/ubuntu']
            if arch == 'amd64' else
            ['https://ports.ubuntu.com/ubuntu-ports', 'http://ports.ubuntu.com/ubuntu-ports'])


def sources(mirror, codename, arch):
    # Cache only package indices, uncompressed locally, so their signed SHA256
    # can be checked directly without trusting a cached decompressor or manifest.
    return '\n'.join(
        f'deb [arch={arch} target=Packages signed-by={KEYRING}] {mirror} '
        f'{codename}{suite} {COMPONENTS}' for suite in ('', '-updates', '-security')) + '\n'


def records(lines):
    """Read Debian control records without loading an entire Packages index."""
    record, field = {}, None
    for line in lines:
        line = line.rstrip('\r\n')
        if not line:
            if record:
                yield record
                record, field = {}, None
        elif line[0].isspace():
            if field is None:
                raise ValueError('continuation without a field')
            record[field] += '\n' + line[1:]
        else:
            field, value = line.split(':', 1)
            if field in record:
                raise ValueError('duplicate control field')
            record[field] = value.strip()
    if record:
        yield record


def regular(path):
    if path.is_symlink() or not path.is_file():
        raise ValueError(f'not a regular cache file: {path.name}')
    return path


def digest(path):
    checksum = hashlib.sha256()
    with regular(path).open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            checksum.update(chunk)
    return checksum.hexdigest(), path.stat().st_size


def verify_cache(cache, codename, arch, keyring=KEYRING):
    """Verify Ubuntu signatures, every used index, then every cached .deb."""
    for directory in (cache, cache / 'lists', cache / 'archives'):
        if directory.is_symlink() or not directory.is_dir():
            raise ValueError('missing or symlinked cache directory')
    source = regular(cache / 'sources.list').read_text()
    origin = next((mirror for mirror in mirrors(arch)
                   if source == sources(mirror, codename, arch)), None)
    if origin is None:
        raise ValueError('cached sources do not match the allowed Ubuntu sources')
    prefix = origin.split('://', 1)[1].replace('/', '_') + '_dists_'
    authenticated = {}
    releases = set()
    now = datetime.now(timezone.utc)
    with tempfile.TemporaryDirectory(prefix='relais-gpgv-') as homedir:
        for suffix in ('', '-updates', '-security'):
            suite = codename + suffix
            name = prefix + suite + '_InRelease'
            release = regular(cache / 'lists' / name)
            if release.stat().st_size > 5 * 1024 * 1024:
                raise ValueError('oversized InRelease')
            # --output emits the cleartext only after gpgv verifies it against
            # the runner image's Ubuntu keyring, never keys from the cache.
            verified = subprocess.run(
                ['gpgv', '--homedir', homedir, '--keyring', str(keyring),
                 '--output', '-', str(release)], check=True, capture_output=True, timeout=15)
            fields, = records(verified.stdout.decode('utf-8').splitlines())
            if fields.get('Suite') != suite or fields.get('Codename') != codename:
                raise ValueError('signed release does not match the Ubuntu suite')
            signed_date = parsedate_to_datetime(fields.get('Date', ''))
            if signed_date.tzinfo is None:
                raise ValueError('signed release Date lacks a timezone')
            if signed_date > now + timedelta(minutes=5):
                raise ValueError(f'cached {suite} release Date is in the future')
            # The base suite keeps its original release date; only moving suites age out.
            if suffix and now - signed_date > timedelta(days=9):
                raise ValueError(f'cached {suite} release is older than 9 days')
            if 'Valid-Until' in fields:
                if parsedate_to_datetime(fields['Valid-Until']) < now:
                    raise ValueError('cached release has expired')
            for row in fields['SHA256'].splitlines():
                if not row.strip():
                    continue
                checksum, size, relative = row.split()
                # Only uncompressed Packages are enabled by our source options.
                match = re.fullmatch(r'(main|restricted|universe|multiverse)/binary-([^/]+)/Packages', relative)
                if not match or match[2] not in (arch, 'all'):
                    continue
                if not re.fullmatch(r'[0-9a-f]{64}', checksum) or int(size) < 0:
                    raise ValueError('invalid signed index checksum')
                filename = prefix + suite + '_' + relative.replace('/', '_')
                authenticated[filename] = checksum, int(size)
            releases.add(name)
    indices = []
    for path in (cache / 'lists').iterdir():
        if path.name in ('lock', 'partial', 'auxfiles'):
            continue
        regular(path)
        if path.name in releases:
            continue
        if path.name not in authenticated or digest(path) != authenticated[path.name]:
            raise ValueError(f'index is not authenticated by InRelease: {path.name}')
        indices.append(path)
    if not indices:
        raise ValueError('no authenticated Packages indices')
    archives = list((cache / 'archives').glob('*.deb'))
    wanted = {unquote(path.name) for path in archives}
    expected = defaultdict(set)
    for index in indices:
        with index.open(encoding='utf-8') as stream:
            for record in records(stream):
                name = f"{record['Package']}_{record['Version']}_{record['Architecture']}.deb"
                if name not in wanted:
                    continue
                checksum, size = record['SHA256'], int(record['Size'])
                if not re.fullmatch(r'[0-9a-f]{64}', checksum) or size < 0:
                    raise ValueError('invalid package checksum')
                expected[name].add((checksum, size))
    for archive in archives:
        if digest(archive) not in expected[unquote(archive.name)]:
            raise ValueError(f'archive is not authenticated by Packages: {archive.name}')


if __name__ == '__main__':
    try:
        verify_cache(Path(sys.argv[1]), sys.argv[2], sys.argv[3])
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        print(f'Cached Ubuntu inputs failed authentication: {error}', file=sys.stderr)
        raise SystemExit(1) from error
