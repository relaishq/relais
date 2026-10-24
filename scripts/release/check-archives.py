#!/usr/bin/env python3
"""Check every target archive against the generated manifest before publication."""
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tarfile


def check(directory, version, commit):
    directory = Path(directory)
    checksums = {}
    for line in (directory / 'checksums.txt').read_text().splitlines():
        digest, name = line.split()
        assert name not in checksums, 'duplicate checksum entry'
        checksums[name] = digest
    expected_archives = {f'relais_{version}_{system}_{arch}.tar.gz'
                         for system in ('linux', 'darwin') for arch in ('amd64', 'arm64')}
    assert set(checksums) == expected_archives, 'incomplete archive checksum set'
    files = {'relais-' + name for name in ('relay', 'worker', 'control', 'signal', 'demo', 'load', 'compat')}
    files |= {'LICENSE', 'NOTICE', 'README.md', 'start.sh', 'stop.sh', 'compatibility.json'}
    for system in ('linux', 'darwin'):
        for arch in ('amd64', 'arm64'):
            base = f'relais_{version}_{system}_{arch}'
            archive = directory / (base + '.tar.gz')
            with archive.open('rb') as stream:
                digest = hashlib.sha256()
                for chunk in iter(lambda: stream.read(1024 * 1024), b''):
                    digest.update(chunk)
                assert digest.hexdigest() == checksums[archive.name], 'archive checksum mismatch'
            with tarfile.open(archive, 'r:gz') as tar:
                names = []
                for member in tar.getmembers():
                    if member.isdir() and member.name.rstrip('/') == base:
                        continue
                    assert member.isfile() and member.name in {base + '/' + name for name in files}, 'unsafe archive member'
                    names.append(member.name)
                assert len(names) == len(set(names)) and set(names) == {base + '/' + name for name in files}, 'incomplete archive layout'
                stream = tar.extractfile(base + '/compatibility.json')
                assert stream is not None
                manifest = json.load(stream)
                expected = json.loads(subprocess.check_output(
                    ['go', 'run', './scripts/release/compat', 'generate', '--version', version,
                     '--commit', commit, '--os', system, '--arch', arch], text=True))
                assert manifest == expected, 'archive manifest differs from generated contract'
    print('PASS: all four archive layouts, checksums and generated manifests')


if __name__ == '__main__':
    if len(sys.argv) != 4:
        raise SystemExit('usage: check-archives.py DIRECTORY VERSION FULL_COMMIT')
    check(*sys.argv[1:])
