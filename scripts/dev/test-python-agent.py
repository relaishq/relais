#!/usr/bin/env python3
"""Test the installed, pinned agent package in an owned throwaway environment."""
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import venv


def main():
    repository = Path(__file__).resolve().parents[2]
    package = repository / 'python/relais-agent'
    with tempfile.TemporaryDirectory(prefix='relais-agent-tests-') as directory:
        temporary = Path(directory)
        source = temporary / 'package'
        shutil.copytree(package, source,
                        ignore=shutil.ignore_patterns('__pycache__', '*.egg-info', 'build'))
        environment = temporary / 'venv'
        venv.EnvBuilder(with_pip=True).create(environment)
        python = environment / 'bin/python'
        subprocess.run([str(python), '-m', 'pip', 'install', '--disable-pip-version-check',
                        str(source)], check=True)
        return subprocess.run([str(python), '-m', 'unittest', 'discover', '-v',
                               '-s', str(package / 'tests')], cwd=repository).returncode


if __name__ == '__main__':
    sys.exit(main())
