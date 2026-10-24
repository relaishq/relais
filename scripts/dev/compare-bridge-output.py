#!/usr/bin/env python3
"""Prototype, not for merge: run the complete bounded #254 output comparison."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--repetitions', type=int, choices=(1, 10), default=10)
    args = parser.parse_args()
    driver = str(Path(__file__).with_name('measure-bridge-output.py'))
    source_files = [
        'pkg/mediaworker/accepted_event.go', 'pkg/mediaworker/agent_tasks.go',
        'pkg/mediaworker/agent_v2.go', 'pkg/mediaworker/bridge_stream_output_prototype.go',
        'pkg/mediaworker/bridge_task_throughput_test.go', 'pkg/mediaworker/bridge_output_admission_test.go',
        'pkg/mediaworker/bridge_output_verdict_test.go',
        'scripts/dev/measure-bridge-output.py',
    ]
    source_hash = hashlib.sha256()
    for name in source_files:
        source_hash.update(name.encode())
        source_hash.update(Path(name).read_bytes())
    print('BRIDGE_SOURCE ' + json.dumps({'sha256': source_hash.hexdigest(), 'files': source_files}), flush=True)
    phases = (
        ('memory-baseline', []),
        ('race-redis-baseline', ['--race', '--redis']),

        ('race-redis-rotation-pressure', ['--race', '--redis', '--stress']),
        ('race-redis-cpu-jitter', ['--race', '--redis', '--loaded']),
        ('race-redis-loaded-interruptions', ['--race', '--redis', '--stress', '--loaded']),
        ('race-redis-repetitions', ['--race', '--redis', '--repeat', str(args.repetitions)]),
    )
    failures = 0
    started = time.monotonic()
    for name, flags in phases:
        print(f'BRIDGE_PHASE_START {name}', flush=True)
        code = subprocess.call([sys.executable, driver] + flags)
        print(f'BRIDGE_PHASE_END {name} exit={code}', flush=True)
        failures += code != 0
    print(f'BRIDGE_SUITE_END failed_phases={failures} wall_s={time.monotonic()-started:.3f}', flush=True)
    return 1 if failures else 0


if __name__ == '__main__':
    raise SystemExit(main())
