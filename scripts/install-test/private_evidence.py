"""Bounded private control evidence for the Go-free install driver."""
import json

MAX_SAMPLE_BYTES = 1024 * 1024
SAMPLE_SLOTS = 32


def capture_private_status(request, logs, control, label):
    """Best effort: diagnostics must not replace the original test failure."""
    try:
        status = request(control + '/status')
        data = json.dumps(status, indent=2) + '\n'
        if len(data.encode()) > MAX_SAMPLE_BYTES:
            raise ValueError('private status exceeds evidence limit')
        events = json.dumps(status.get('takeovers', []), indent=2) + '\n'
    except Exception as exc:
        # Keep the failure type; arbitrary response bodies may contain secrets.
        data = json.dumps({'capture_error': type(exc).__name__}) + '\n'
        events = data
    try:
        (logs / f'control-private-{label}.json').write_text(data)
        (logs / f'takeovers-private-{label}.json').write_text(events)
    except OSError:
        pass


def wait_for_caller(caller, request, logs, control, timeout, clock, expired):
    deadline = clock() + timeout
    sample = 0
    while True:
        capture_private_status(request, logs, control, f'{sample % SAMPLE_SLOTS:02d}')
        sample += 1
        remaining = deadline - clock()
        if remaining <= 0:
            raise TimeoutError('caller completion timed out')
        try:
            return caller.wait(timeout=min(1, remaining))
        except expired:
            pass
