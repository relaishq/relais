#!/usr/bin/env python3
"""Prototype measurement: isolate output load factors, preserving the 150 ms gate."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--repeat', type=int, choices=(1, 5), default=5)
    parser.add_argument('--factor', choices=('all', 'cpu1', 'cpu2', 'stalls', 'renewal'), default='all')
    args = parser.parse_args()
    files = [
        'pkg/mediaworker/accepted_event.go', 'pkg/mediaworker/agent_tasks.go',
        'pkg/mediaworker/agent_v2.go', 'pkg/mediaworker/bridge_stream_output_prototype.go',
        'pkg/mediaworker/bridge_task_throughput_test.go',
        'pkg/mediaworker/bridge_output_admission_test.go',
        'pkg/mediaworker/bridge_output_verdict_test.go',
        'pkg/mediaworker/bridge_output_diagnosis_test.go',
        'pkg/mediaworker/bridge_output_factors_test.go',
        'scripts/dev/measure-bridge-output.py',
        'scripts/dev/measure-bridge-output-factors.py',
    ]
    digest = hashlib.sha256()
    for name in files:
        digest.update(name.encode())
        digest.update(Path(name).read_bytes())
    print('BRIDGE_SOURCE ' + json.dumps({'sha256': digest.hexdigest(), 'files': files}), flush=True)
    factors = ('cpu1', 'cpu2', 'stalls', 'renewal') if args.factor == 'all' else (args.factor,)
    candidates = [('catchup', 10), ('catchup', 20), ('stream', 10)]
    failures = 0
    started = time.monotonic()
    for factor in factors:
        print('BRIDGE_PHASE_START factor-' + factor, flush=True)
        for trial in range(1, args.repeat + 1):
            # Rotate the order to limit time-of-run bias on the shared Mac.
            offset = (trial - 1) % len(candidates)
            for candidate, bound in candidates[offset:] + candidates[:offset]:
                print(f'BRIDGE_FACTOR_TRIAL factor={factor} candidate={candidate} batch={bound} repetition={trial}', flush=True)
                code = subprocess.call([
                    sys.executable, 'scripts/dev/measure-bridge-output.py',
                    '--candidate', candidate, '--batch', str(bound), '--factor', factor,
                    '--race', '--redis',
                ])
                failures += code != 0
                print(f'BRIDGE_FACTOR_END factor={factor} candidate={candidate} batch={bound} repetition={trial} exit={code}', flush=True)
    print(f'BRIDGE_FACTOR_SUITE_END failures={failures} wall_s={time.monotonic()-started:.3f}', flush=True)
    return int(failures != 0)


if __name__ == '__main__':
    raise SystemExit(main())
