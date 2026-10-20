#!/usr/bin/env python3
"""Materialize reviewed public metadata; credential values are fetched separately."""
import json
from pathlib import Path
import sys

root = Path(sys.argv[1])
common = json.loads((root / 'etc/relais/common.json').read_text())['RelaisFiles']
host = json.loads((root / 'etc/relais/host.json').read_text())['RelaisFiles']
assert set(host) == {'/etc/relais/node.json', '/etc/relais/node.env'}
components = json.loads(host['/etc/relais/node.json']['content'])['components']
units = {'relais-prepare.service', *('relais-' + c + '.service' for c in components)}
if 'relay' in components:
    units.add('relais-relay-standby.service')
for name, entry in {**common, **host}.items():
    if name.startswith('/etc/systemd/system/') and Path(name).name not in units:
        continue
    path = root / name.lstrip('/')
    if not path.parent.exists():
        path.parent.mkdir(parents=True, mode=0o755)
    path.write_text(entry['content'])
    path.chmod(int(entry['mode'], 8))
