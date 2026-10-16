#!/usr/bin/env python3
"""Bounded sequential trials; paired mode alternates which transport runs first."""
import argparse
import json
import pathlib
import subprocess
import sys
import time

p = argparse.ArgumentParser()
p.add_argument('--scratch', required=True)
p.add_argument('--count', type=int, default=1000)
p.add_argument('--faults', nargs='+', choices=['normal','disconnect','worker-kill','lost-receipt'], default=['normal','disconnect','worker-kill','lost-receipt'])
p.add_argument('--after-regression', help='wait for the owned gate result before starting measurement')
p.add_argument('--transport', choices=['ws', 'dc', 'paired'], default='ws')
a = p.parse_args()
s = pathlib.Path(a.scratch).resolve()
out = s / ('paired-matrix' if a.transport == 'paired' else a.transport + '-matrix')
out.mkdir(exist_ok=True)
start = time.monotonic()
if a.after_regression:
    gate = pathlib.Path(a.after_regression)
    deadline = start + 900
    while True:
        try:
            result = json.loads(gate.read_text())
            print(json.dumps({'measurement_waited_for': str(gate), 'gate_result': result}), flush=True)
            break
        except (FileNotFoundError, json.JSONDecodeError):
            if time.monotonic() > deadline:
                raise RuntimeError('owned regression did not finish within wait bound')
            time.sleep(5)
    # The gate's failure is retained; this only separates its CPU work from measurements.
    start = time.monotonic()
failed = []
for mode in a.faults:
    for trial in range(1, 4):
        order = (['ws', 'dc'] if trial % 2 else ['dc', 'ws']) if a.transport == 'paired' else [a.transport]
        for transport in order:
            name = f'{mode}-{trial}' + (f'-{transport}' if a.transport == 'paired' else '')
            path = out / name
            if path.exists():
                raise RuntimeError(f'refusing to overwrite trial: {path}')
            command = [str(s / 'bin/driver'), '-bin', str(s / 'bin'), '-out', str(path),
                       '-tone-a', str(s / 'tone-440.ogg'), '-tone-b', str(s / 'tone-880.ogg'),
                       '-fault', mode, '-count', str(a.count), '-rate', '30']
            if transport == 'dc':
                command.append('-datachannel')
            run = subprocess.run(command, text=True, capture_output=True, timeout=100)
            (out / f'{name}.stdout').write_text(run.stdout)
            (out / f'{name}.stderr').write_text(run.stderr)
            print(json.dumps({'run': name, 'transport': transport, 'exit_code': run.returncode,
                              'elapsed_s': round(time.monotonic() - start, 3),
                              'summary': json.loads(run.stdout) if run.stdout.strip().startswith('{') else run.stdout,
                              'stderr': run.stderr}), flush=True)
            if run.returncode:
                failed.append(name)
print(json.dumps({'matrix_duration_s': time.monotonic() - start, 'failed': failed}), flush=True)
sys.exit(bool(failed))
