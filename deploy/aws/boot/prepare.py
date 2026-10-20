#!/usr/bin/env python3
"""Fetch exact secret references; never print a secret or a CLI exception."""
import json
import os
from pathlib import Path
import re
import sys
import subprocess


def prepare(config_path, runtime):
    config = json.loads(Path(config_path).read_text())
    root = Path(runtime)
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(root, 0o700)
    if os.environ.get('RELAIS_TEST_SECRET_FILE'):
        values = json.loads(Path(os.environ['RELAIS_TEST_SECRET_FILE']).read_text())
        fetch = values.__getitem__
    else:
        def fetch(arn):
            result = subprocess.run(['aws', 'secretsmanager', 'get-secret-value', '--secret-id', arn,
                                     '--region', config['region'], '--query', 'SecretString', '--output', 'text'],
                                    capture_output=True, text=True, check=True)
            return result.stdout.rstrip('\n')

    def write(name, value):
        path = root / name
        # Refuse symlinks. Runtime directory is root-only in the real boot path.
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
        try:
            os.fchmod(fd, 0o600)
            with os.fdopen(fd, 'w') as stream:
                stream.write(value)
        except BaseException:
            path.unlink(missing_ok=True)
            raise

    def secret(arn, pattern):
        value = fetch(arn)
        if not re.fullmatch(pattern, value):
            raise ValueError('invalid generated secret')
        return value

    # Remove old values before a failed refresh can leave stale credentials usable.
    for role in config['components']:
        (root / (role + '.env')).unlink(missing_ok=True)
    (root / 'signal.token').unlink(missing_ok=True)
    for role in config['components']:
        if role == 'signal':
            write('signal.token', secret(config['secrets']['token'], r'[A-Za-z0-9]{64}'))
            continue
        entries = {
            'RELAIS_REDIS_USERNAME': role,
            'RELAIS_REDIS_PASSWORD': secret(config['secrets'][role], r'[A-Za-z0-9]{64}'),
        }
        if role in ('worker', 'control'):
            entries['RELAIS_SESSIONSTORE_KEY'] = secret(config['secrets']['snapshot'], r'[0-9a-f]{64}')
        write(role + '.env', ''.join(f'{key}={value}\n' for key, value in entries.items()))


if __name__ == '__main__':
    try:
        prepare(sys.argv[1], sys.argv[2])
    except Exception:
        print('secret preparation failed; start refused', file=sys.stderr)
        sys.exit(1)
    print('role secrets prepared')
