#!/usr/bin/env python3
"""Test the installed, pinned agent package in an owned throwaway environment."""
from pathlib import Path
import os
import shutil
import subprocess
import sys
import tempfile
import venv


def python312():
    candidates = [os.environ.get('PYTHON_AGENT', 'python3.12'), 'python3.12',
                  '/opt/homebrew/bin/python3.12', '/usr/local/bin/python3.12',
                  *map(str, sorted((Path.home() / '.pyenv/versions').glob('3.12*/bin/python3.12'))),
                  sys.executable]
    for candidate in dict.fromkeys(candidates):
        binary = shutil.which(candidate)
        if binary is None:
            continue
        try:
            result = subprocess.run([binary, '-c', 'import sys; sys.exit(sys.version_info[:2] != (3, 12))'],
                                    capture_output=True, timeout=5)
        except (OSError, subprocess.TimeoutExpired):
            continue
        if result.returncode == 0:
            return binary
    return None


def main():
    interpreter = python312()
    if interpreter is None:
        required = 'CI' in os.environ
        print(('FAIL' if required else 'SKIP') +
              ' Python bridge tests: no usable Python 3.12 found; nothing installed.')
        return 1 if required else 0
    if Path(interpreter).resolve() != Path(sys.executable).resolve():
        return subprocess.run([interpreter, str(Path(__file__).resolve())]).returncode
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
