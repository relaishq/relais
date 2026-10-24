#!/usr/bin/env python3
"""Run bounded, isolated bridge output measurements; owned local stores."""
import argparse
import os
from pathlib import Path
import subprocess
import sys
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--candidate', choices=('catchup', 'stream', 'both'), default='both')
    parser.add_argument('--repeat', type=int, default=1)
    parser.add_argument('--race', action='store_true')
    parser.add_argument('--redis', action='store_true')
    parser.add_argument('--loaded', action='store_true')
    parser.add_argument('--stress', action='store_true')
    parser.add_argument('--factor', choices=('baseline', 'combined', 'cpu1', 'cpu2', 'stalls', 'renewal'))
    parser.add_argument('--batch', type=int, choices=(10, 20), default=10)
    args = parser.parse_args()
    if not 1 <= args.repeat <= 10:
        parser.error('--repeat must be between 1 and 10')
    candidates = ('catchup', 'stream') if args.candidate == 'both' else (args.candidate,)
    failures = 0
    for candidate in candidates:
        for trial in range(1, args.repeat + 1):
            env = dict(os.environ)
            env.update(RELAIS_BRIDGE_THROUGHPUT='1', RELAIS_BRIDGE_OUTPUT=candidate,
                       RELAIS_BRIDGE_LOAD='1' if args.loaded else '0',
                       RELAIS_BRIDGE_STRESS='1' if args.stress else '0',
                       RELAIS_BRIDGE_BATCH=str(args.batch))
            env['RELAIS_BRIDGE_FACTOR'] = args.factor or ('combined' if args.loaded else 'baseline')
            env.setdefault('PION_LOG_WARN', 'session,bridge-throughput')
            # Sustained evidence always uses the full 60-second source.
            env.pop('RELAIS_BRIDGE_PACKETS', None)
            cmd = ['go', 'test']
            if args.race:
                cmd.append('-race')
            cmd += ['-count=1', '-timeout=3m', '-v', '-run', '^TestBridgeTaskOutputThroughput$', './pkg/mediaworker']
            if args.redis:
                cmd = [sys.executable, str(Path(__file__).with_name('with-redis.py'))] + cmd
            else:
                env.update(RELAIS_TEST_REDIS_ADDR='', RELAIS_TEST_REDIS_REQUIRE='0')
            print(f'BRIDGE_TRIAL candidate={candidate} trial={trial} race={args.race} redis={args.redis} loaded={args.loaded} stress={args.stress} factor={env["RELAIS_BRIDGE_FACTOR"]} batch={args.batch}', flush=True)
            start = time.monotonic()
            try:
                code = subprocess.call(cmd, env=env)
            except KeyboardInterrupt:
                return 130
            failures += code != 0
            print(f'BRIDGE_TRIAL_END candidate={candidate} trial={trial} exit={code} wall_s={time.monotonic()-start:.3f}', flush=True)
    return 1 if failures else 0


if __name__ == '__main__':
    raise SystemExit(main())
