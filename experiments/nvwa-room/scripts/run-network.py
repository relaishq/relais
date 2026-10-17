#!/usr/bin/env python3
"""Paired sequential runs with bounded startup failures and graceful cancellation."""
import argparse
import gzip
import json
import pathlib
import signal
import subprocess
import sys
import time


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--scratch', required=True)
    p.add_argument('--out', required=True)
    p.add_argument('--topology', choices=['loopback', 'netns'], default='loopback')
    p.add_argument('--profile', default='lan')
    p.add_argument('--trials', type=int, default=2)
    p.add_argument('--count', type=int, default=1000)
    p.add_argument('--rate', type=int, default=30)
    p.add_argument('--max-startup-failures', type=int, default=2)
    p.add_argument('--budget-seconds', type=float, default=1440)
    p.add_argument('--cell-timeout', type=float, default=190)
    p.add_argument('--shutdown-grace', type=float, default=20)
    a = p.parse_args()
    if not 1 <= a.trials <= 4 or not 4 <= a.count <= 2000 or not 1 <= a.rate <= 50:
        p.error('bounded trial/count/rate exceeded')
    if not 1 <= a.max_startup_failures <= 10 or min(a.budget_seconds, a.cell_timeout, a.shutdown_grace) <= 0:
        p.error('invalid failure/budget limits')
    scratch, out = pathlib.Path(a.scratch).resolve(), pathlib.Path(a.out).resolve()
    out.mkdir(parents=True, exist_ok=False)
    variants = ['ws', 'dc-reliable', 'dc-unreliable']
    faults = ['normal', 'disconnect', 'worker-kill', 'lost-receipt']
    ledger = []
    (out / 'matrix-plan.json').write_text(json.dumps({
        'profile': a.profile, 'topology': a.topology, 'trials': a.trials,
        'count': a.count, 'rate': a.rate, 'variants': variants, 'faults': faults,
        'budget_seconds': a.budget_seconds, 'max_startup_failures': a.max_startup_failures}, indent=2) + '\n')
    current = None
    stop_signal = 0
    stop_at = None

    def interrupted(number, _frame):
        nonlocal stop_signal, stop_at
        if stop_signal:
            return
        stop_signal, stop_at = number, time.monotonic()
        if current is not None and current.poll() is None:
            try:
                current.send_signal(number)
            except ProcessLookupError:
                pass

    previous = {number: signal.signal(number, interrupted) for number in (signal.SIGTERM, signal.SIGINT)}
    started = time.monotonic()
    deadline = started + a.budget_seconds
    consecutive_startup_failures = 0
    reason = None
    try:
        for fault in faults:
            for trial in range(a.trials):
                base = variants if fault in ['normal', 'worker-kill'] else variants[1:] + variants[:1]
                order = base if trial % 2 == 0 else list(reversed(base))
                for variant in order:
                    if stop_signal or time.monotonic() >= deadline:
                        reason = 'interrupted' if stop_signal else 'profile budget exhausted'
                        break
                    name = f'{a.profile}-{fault}-{trial + 1}-{variant}'
                    run_dir = out / name
                    cmd = [str(scratch / 'bin/network-driver'), '-bin', str(scratch / 'bin'),
                           '-out', str(run_dir), '-tone-a', str(scratch / 'tone-440.ogg'),
                           '-tone-b', str(scratch / 'tone-880.ogg'), '-topology', a.topology,
                           '-profile', a.profile, '-variant', variant, '-fault', fault,
                           '-count', str(a.count), '-rate', str(a.rate)]
                    print(json.dumps({'starting': name}), flush=True)
                    cell_start = time.monotonic()
                    cell_deadline = min(cell_start + a.cell_timeout, deadline)
                    timed_out = False
                    term_at = None
                    with (out / (name + '.log')).open('w') as log:
                        try:
                            # The driver owns its children. Isolate it from the runner's
                            # process group so it can handle TERM and finish its defers.
                            current = subprocess.Popen(cmd, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
                            while True:
                                now = time.monotonic()
                                if stop_signal and term_at is None and current.poll() is None:
                                    current.send_signal(stop_signal)
                                    term_at = stop_at
                                if not stop_signal and now >= cell_deadline and term_at is None:
                                    timed_out, term_at = True, now
                                    current.terminate()
                                shutdown_at = stop_at if stop_signal else term_at
                                if shutdown_at is not None and now - shutdown_at >= a.shutdown_grace:
                                    current.kill()
                                try:
                                    code = current.wait(timeout=.1)
                                    break
                                except subprocess.TimeoutExpired:
                                    continue
                            if timed_out:
                                code = 124
                        except OSError as error:
                            log.write(f'cannot start owned driver: {error}\n')
                            code = 127
                        finally:
                            current = None
                    phase = 'startup'
                    status = run_dir / 'cell-status.json'
                    if status.exists():
                        phase = json.loads(status.read_text())['phase']
                    startup_failure = bool(code and phase == 'startup')
                    consecutive_startup_failures = consecutive_startup_failures + 1 if startup_failure else 0
                    entry = {'name': name, 'command': cmd, 'exit_code': code,
                             'wall_seconds': time.monotonic() - cell_start, 'trial': trial + 1,
                             'variant': variant, 'fault': fault, 'profile': a.profile,
                             'phase': phase, 'startup_failure': startup_failure}
                    ledger.append(entry)
                    (out / 'ledger.json').write_text(json.dumps(ledger, indent=2) + '\n')
                    raw = run_dir / 'result.json'
                    if raw.exists():
                        with raw.open('rb') as source, gzip.open(run_dir / 'result.json.gz', 'wb') as target:
                            target.write(source.read())
                        raw.unlink()
                    print(json.dumps(entry), flush=True)
                    if stop_signal:
                        reason = 'interrupted'
                    elif timed_out:
                        reason = 'cell/profile deadline reached'
                    elif consecutive_startup_failures >= a.max_startup_failures:
                        reason = f'{consecutive_startup_failures} consecutive startup failures'
                    if reason:
                        break
                if reason:
                    break
            if reason:
                break
    finally:
        for number, handler in previous.items():
            signal.signal(number, handler)
    (out / 'runner-status.json').write_text(json.dumps({
        'stopped_reason': reason, 'signal': stop_signal, 'completed_cells': len(ledger),
        'expected_cells': len(faults) * a.trials * len(variants),
        'wall_seconds': time.monotonic() - started}, indent=2) + '\n')
    subprocess.run([sys.executable, 'experiments/nvwa-room/scripts/summarize-network.py', str(out)], check=True)
    if stop_signal:
        return 128 + stop_signal
    return int(bool(reason) or any(row['exit_code'] for row in ledger))


if __name__ == '__main__':
    raise SystemExit(main())
