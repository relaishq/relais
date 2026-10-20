#!/usr/bin/env python3
"""Bound boot success on local component readiness, without credential requests."""
import json
from pathlib import Path
import ssl
import sys
import time
import urllib.error
import urllib.request


def ready(config_path, env_path, timeout=180):
    components = json.loads(Path(config_path).read_text())['components']
    settings = dict(line.split('=', 1) for line in Path(env_path).read_text().splitlines() if line and not line.startswith('#'))
    ports = {'relay': ('RELAY_HTTP_PORT', 24700), 'worker': ('WORKER_HTTP_PORT', 24701),
             'control': ('CONTROL_HTTP_PORT', 24710), 'signal': ('SIGNAL_PORT', 24743)}
    deadline = time.monotonic() + timeout
    for role in components:
        address = settings['RELAY_PRIVATE' if role == 'relay' else 'CONTROL_PRIVATE' if role == 'control' else 'NODE_PRIVATE']
        key, default = ports[role]
        url = ('https' if role == 'signal' else 'http') + '://' + address + ':' + settings.get(key, str(default)) + '/status'
        while time.monotonic() < deadline:
            try:
                with urllib.request.urlopen(url, timeout=2, context=ssl._create_unverified_context()) as response:
                    data = json.load(response)
                    if role != 'control' or data.get('ready') is True:
                        break
            except urllib.error.HTTPError as exc:
                if role == 'signal' and exc.code == 401:
                    break
            except (OSError, ValueError):
                pass
            time.sleep(0.2)
        else:
            raise RuntimeError('component readiness failed: ' + role)


if __name__ == '__main__':
    try:
        ready(sys.argv[1], sys.argv[2])
    except Exception:
        print('component readiness failed; boot refused', file=sys.stderr)
        sys.exit(1)
    print('local components ready')
