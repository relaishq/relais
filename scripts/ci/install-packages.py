#!/usr/bin/env python3
"""Bound Ubuntu setup separately from tests; cache only authenticated APT inputs."""
import hashlib
import os
from pathlib import Path
import subprocess
import sys
import time

CACHE = Path(os.environ.get('RUNNER_TEMP', '/tmp')) / 'relais-apt'
POLICY = Path('/usr/sbin/policy-rc.d')


def run(command, seconds):
    # Run timeout as root too: an ordinary runner cannot kill root-owned apt children.
    subprocess.run(['sudo', 'timeout', '--signal=TERM', '--kill-after=5s',
                    f'{seconds}s', *command], check=True)


def ubuntu():
    release = dict(line.split('=', 1) for line in Path('/etc/os-release').read_text().splitlines()
                   if '=' in line)
    if release.get('ID', '').strip('"') != 'ubuntu':
        raise RuntimeError('package setup supports Ubuntu runners only')
    return release['VERSION_CODENAME'].strip('"')


def cache_key(packages):
    # A weekly snapshot avoids retaining old security packages indefinitely. Never
    # share lists/binaries across runner image revisions, Ubuntu releases or CPUs.
    identity = [ubuntu(), os.environ.get('ImageVersion', 'unknown'),
                subprocess.check_output(['dpkg', '--print-architecture'], text=True).strip(),
                time.strftime('%G-%V', time.gmtime()), *sorted(packages)]
    digest = hashlib.sha256(Path(__file__).read_bytes() + '\n'.join(identity).encode()).hexdigest()
    return 'ubuntu-packages-' + digest


def apt_options(cache):
    return ['-o', f'Dir::State::lists={cache}/lists',
            '-o', f'Dir::Cache::archives={cache}/archives',
            '-o', f'Dir::Etc::sourcelist={cache}/sources.list',
            '-o', 'Dir::Etc::sourceparts=-',
            '-o', 'Acquire::Retries=0', '-o', 'Acquire::http::Timeout=10',
            '-o', 'Acquire::https::Timeout=10', '-o', 'Acquire::Languages=none',
            '-o', 'APT::Update::Error-Mode=any',
            '-o', 'APT::Keep-Downloaded-Packages=true',
            '-o', 'Binary::apt-get::APT::Keep-Downloaded-Packages=true',
            '-o', 'DPkg::Lock::Timeout=10']


def download(packages, codename, arch, cache):
    options = apt_options(cache)
    if (cache / 'sources.list').exists():
        try:
            # Check completeness without network before touching dpkg. A partial
            # or missing cache falls through to the two bounded mirror attempts.
            run(['apt-get', *options, 'install', '-y', '--no-install-recommends',
                 '--no-upgrade', '--download-only', '--no-download', *packages], 30)
            return 'cache'
        except subprocess.CalledProcessError:
            print('Cached packages incomplete; trying authenticated Ubuntu mirrors', flush=True)
    mirrors = (['https://azure.archive.ubuntu.com/ubuntu', 'https://archive.ubuntu.com/ubuntu']
               if arch == 'amd64' else
               ['https://ports.ubuntu.com/ubuntu-ports', 'http://ports.ubuntu.com/ubuntu-ports'])
    for mirror in mirrors:
        # Do not mix a previous mirror's lists with the next attempt.
        run(['rm', '-rf', str(cache / 'lists')], 10)
        (cache / 'lists' / 'partial').mkdir(parents=True)
        (cache / 'sources.list').write_text('\n'.join(
            f'deb [signed-by=/usr/share/keyrings/ubuntu-archive-keyring.gpg] {mirror} '
            f'{codename}{suite} main restricted universe multiverse'
            for suite in ('', '-updates', '-security')) + '\n')
        try:
            print(f'Package download attempt: {mirror} (update <=45s, download <=75s)', flush=True)
            run(['apt-get', *options, 'update'], 45)
            run(['apt-get', *options, 'install', '-y', '--no-install-recommends',
                 '--no-upgrade', '--download-only', *packages], 75)
            return mirror
        except subprocess.CalledProcessError as error:
            print(f'Package download failed or timed out on {mirror}: {error.returncode}', flush=True)
    raise RuntimeError('Ubuntu package download failed on both bounded mirror attempts')


def install(packages, cache=CACHE):
    codename = ubuntu()
    arch = subprocess.check_output(['dpkg', '--print-architecture'], text=True).strip()
    installed = subprocess.run(['dpkg-query', '-W', '-f=${db:Status-Status}\n', *packages],
                               capture_output=True, text=True, check=False)
    if installed.returncode == 0 and installed.stdout.splitlines() == ['installed'] * len(packages):
        return 'already installed on runner image'
    cache.mkdir(parents=True, exist_ok=True)
    (cache / 'archives' / 'partial').mkdir(parents=True, exist_ok=True)
    source = download(packages, codename, arch, cache)
    # Never start the distribution's Redis (or any other service). Tests own
    # their processes and dedicated ports. Preserve even a symlink policy.
    backup = cache.parent / 'relais-original-policy-rc.d'
    existed = POLICY.exists() or POLICY.is_symlink()
    if existed:
        run(['cp', '-a', str(POLICY), str(backup)], 10)
    policy = cache.parent / 'relais-policy-rc.d'
    policy.write_text('#!/bin/sh\nexit 101\n')
    try:
        run(['rm', '-f', str(POLICY)], 10)
        run(['install', '-m', '755', str(policy), str(POLICY)], 10)
        # Network is forbidden in the dpkg phase, which has its own bound.
        run(['env', 'DEBIAN_FRONTEND=noninteractive', 'NEEDRESTART_MODE=a',
             'apt-get', *apt_options(cache), 'install', '-y', '--no-install-recommends',
             '--no-upgrade', '--no-download', *packages], 90)
    finally:
        run(['rm', '-f', str(POLICY)], 10)
        if existed:
            run(['cp', '-a', str(backup), str(POLICY)], 10)
        # actions/cache saves as the runner user, not root.
        run(['chown', '-R', f'{os.getuid()}:{os.getgid()}', str(cache)], 10)
    return source


def main():
    mode, *packages = sys.argv[1:]
    if mode not in ('key', 'install') or not packages or any(p.startswith('-') for p in packages):
        raise SystemExit('usage: install-packages.py {key|install} PACKAGE...')
    if mode == 'key':
        with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
            print('key=' + cache_key(packages), file=output)
        return
    start = time.monotonic()
    result = 'failed'
    try:
        result = install(packages)
    except (RuntimeError, subprocess.CalledProcessError) as error:
        print(f'::error title=Ubuntu package install failed::{error}; packages: {" ".join(packages)}',
              file=sys.stderr)
        raise SystemExit(1) from error
    finally:
        message = f'Ubuntu package setup: {result}; {time.monotonic() - start:.1f}s; {" ".join(packages)}'
        print(message, flush=True)
        if os.environ.get('GITHUB_STEP_SUMMARY'):
            with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as summary:
                print(message + '\n', file=summary)


if __name__ == '__main__':
    main()
