#!/usr/bin/env python3
"""Paired, alternating, sequential stage-4 runs. No workflow dispatch here."""
import argparse
import gzip
import json
import pathlib
import subprocess
import sys
import time

p = argparse.ArgumentParser()
p.add_argument('--scratch', required=True)
p.add_argument('--out', required=True)
p.add_argument('--topology', choices=['loopback', 'netns'], default='loopback')
p.add_argument('--profile', default='lan')
p.add_argument('--trials', type=int, default=2)
p.add_argument('--count', type=int, default=1000)
p.add_argument('--rate', type=int, default=30)
a = p.parse_args()
if not 1 <= a.trials <= 4 or not 4 <= a.count <= 2000 or not 1 <= a.rate <= 50:
    p.error('bounded trial/count/rate exceeded')
scratch = pathlib.Path(a.scratch).resolve()
out = pathlib.Path(a.out).resolve()
out.mkdir(parents=True, exist_ok=False)
variants = ['ws', 'dc-reliable', 'dc-unreliable']
ledger = []
(out / 'matrix-plan.json').write_text(json.dumps({'profile': a.profile, 'topology': a.topology,
    'trials': a.trials, 'count': a.count, 'rate': a.rate, 'variants': variants,
    'faults': ['normal', 'disconnect', 'worker-kill', 'lost-receipt']}, indent=2) + '\n')
for fault in ['normal', 'disconnect', 'worker-kill', 'lost-receipt']:
    for trial in range(a.trials):
        # Exact reversal on the paired repeat; middle position alternates by fault.
        base = variants if fault in ['normal', 'worker-kill'] else variants[1:] + variants[:1]
        order = base if trial % 2 == 0 else list(reversed(base))
        for variant in order:
            name = f'{a.profile}-{fault}-{trial + 1}-{variant}'
            run_dir = out / name
            cmd = [str(scratch / 'bin/network-driver'), '-bin', str(scratch / 'bin'),
                   '-out', str(run_dir), '-tone-a', str(scratch / 'tone-440.ogg'),
                   '-tone-b', str(scratch / 'tone-880.ogg'), '-topology', a.topology,
                   '-profile', a.profile, '-variant', variant, '-fault', fault,
                   '-count', str(a.count), '-rate', str(a.rate)]
            print(json.dumps({'starting': name}), flush=True)
            start = time.monotonic()
            with (out / (name + '.log')).open('w') as log:
                try:
                    run = subprocess.run(cmd, stdout=log, stderr=subprocess.STDOUT, timeout=190)
                    code = run.returncode
                except subprocess.TimeoutExpired:
                    code = 124
            entry = {'name': name, 'command': cmd, 'exit_code': code,
                     'wall_seconds': time.monotonic() - start, 'trial': trial + 1,
                     'variant': variant, 'fault': fault, 'profile': a.profile}
            ledger.append(entry)
            (out / 'ledger.json').write_text(json.dumps(ledger, indent=2) + '\n')
            raw = run_dir / 'result.json'
            if raw.exists():
                with raw.open('rb') as source, gzip.open(run_dir / 'result.json.gz', 'wb') as target:
                    target.write(source.read())
                raw.unlink()
            print(json.dumps(entry), flush=True)
            # A failed comparison is retained and does not suppress its paired cell.
            # A timeout stops this matrix: the CI cleanup assertion must inspect it.
            if code == 124:
                break
        if ledger[-1]['exit_code'] == 124:
            break
    if ledger[-1]['exit_code'] == 124:
        break
subprocess.run([sys.executable, 'experiments/nvwa-room/scripts/summarize-network.py',
                str(out)], check=True)
raise SystemExit(any(row['exit_code'] for row in ledger))
