#!/usr/bin/env python3
"""Print the operator rehearsal run-page summary as Markdown.

Reads only report.json and clock-selection-status.json from the sanitized
report directory. Prints only fixed codes and counts from the listed fields:
never errors, failures, diagnostics, addresses, host labels or digests. Any
other text value is shown as "unreadable".
"""
import json
import math
from pathlib import Path
import re
import sys

CODE = re.compile(r'[a-z][a-z0-9_-]{0,63}')
MISSING = object()
BOUNDS = ('frontendControlBoundNs', 'frontendWorkerBoundNs', 'driftPpm')


def load(directory, name):
    """Return the JSON object, or None and a fixed note naming the problem."""
    try:
        raw = (directory / name).read_bytes()
    except FileNotFoundError:
        return None, f'`{name}` missing.'
    except OSError:
        return None, f'`{name}` cannot be read.'
    try:
        document = json.loads(raw)
    except ValueError:
        return None, f'`{name}` cannot be parsed.'
    if not isinstance(document, dict):
        return None, f'`{name}` cannot be parsed.'
    return document, None


def field(value, *path):
    for key in path:
        if not isinstance(value, dict) or key not in value:
            return MISSING
        value = value[key]
    return value


def code(value):
    if value is MISSING:
        return 'missing'
    return value if isinstance(value, str) and CODE.fullmatch(value) else 'unreadable'


def count(value):
    if value is MISSING:
        return 'missing'
    return str(value) if type(value) is int and value >= 0 else 'unreadable'


def number(value):
    if value is MISSING:
        return 'missing'
    finite = type(value) in (int, float) and math.isfinite(value)
    return str(value) if finite else 'unreadable'


def codes(value):
    if value is MISSING:
        return 'missing'
    if not isinstance(value, list):
        return 'unreadable'
    return ', '.join(code(v) for v in value) or 'none'


def untimed(callers):
    """Count caller events whose timingEvidence is false."""
    if callers is MISSING:
        return 'missing'
    total = untimed_events = 0
    for caller in callers if isinstance(callers, list) else [None]:
        events = field(caller, 'events')
        if not isinstance(events, list):
            return 'unreadable'
        for event in events:
            evidence = field(event, 'timingEvidence')
            if not isinstance(evidence, bool):
                return 'unreadable'
            total += 1
            untimed_events += not evidence
    return f'{untimed_events} of {total} events'


def report_lines(report):
    version = report.get('schemaVersion')
    if type(version) is not int or version != 1:
        return ['`report.json` schema version is not 1; its fields are not shown.']
    lines = [f'Verdict: **{code(report.get("verdict", MISSING))}**', '']
    if report.get("partR") is True:
        return lines + part_r_lines(report)
    gates = report.get('gates', MISSING)
    if isinstance(gates, list):
        lines += ['| Gate | Status | Media status |', '| --- | --- | --- |']
        lines += [f'| {code(field(g, "gate"))} | {code(field(g, "status"))} | {code(field(g, "media", "status"))} |'
                  for g in gates]
    else:
        lines.append(f'Gates: {code(gates)}')
    lines += ['',
              f'Pooled media: status **{code(field(report, "pooledMedia", "status"))}**, '
              f'qualified takeovers {count(field(report, "pooledMedia", "qualifiedTakeovers"))}, '
              f'qualified moves {count(field(report, "pooledMedia", "qualifiedMoves"))}',
              '', '### Timing undecided', '']
    rows = report.get('timingUndecided', MISSING)
    if isinstance(rows, list) and rows:
        lines += ['| Gate | Phase | Event | Metric | Causes |', '| --- | --- | --- | --- | --- |']
        lines += [f'| {code(field(r, "gate"))} | {count(field(r, "phase"))} | {count(field(r, "event"))} '
                  f'| {code(field(r, "metric"))} | {codes(field(r, "causes"))} |' for r in rows]
    else:
        lines.append('none' if isinstance(rows, list) else code(rows))
    lines += ['', '### Untrusted caller events', '',
              '- Caller events with `timingEvidence: false`, release callers (`callerTiming`): '
              + untimed(report.get('callerTiming', MISSING)),
              '- Caller events with `timingEvidence: false`, head caller (`headCaller.callerTiming`): '
              + untimed(field(report, 'headCaller', 'callerTiming'))]
    loss = report.get('relayLoss', MISSING)
    lines += ['', '### Relay host loss (5 s budget)', '']
    completed = field(loss, 'completed')
    lines += ['Procedure completed: ' + ('yes' if completed is True else 'no' if completed is False else code(completed))]
    phase = field(loss, 'failedPhase')
    lines += ['Failed phase: ' + ('none' if phase == '' or completed is True and phase is MISSING else code(phase))]
    calls = field(loss, 'calls')
    if isinstance(calls, list):
        lines += ['', '| Call | Graded gap (ns) | Recovery gap (ns) | Recovery | Within 5 s |', '| --- | ---: | ---: | --- | --- |']
        for call in calls:
            gap, recovered = field(call, 'gapNs'), field(call, 'recovered')
            valid = type(gap) is int and gap > 0
            within = 'pass' if recovered is True and valid and gap <= 5_000_000_000 else 'fail'
            recovery = 'observed' if recovered is True else 'unobserved'
            lines.append(f'| {count(field(call, "call"))} | {count(gap)} | {count(field(call, "recoveryGapNs"))} | {recovery} | {within} |')
    else:
        lines.append('Call gaps: ' + code(calls))
    return lines


# R uses the separate pinned judge contract. These are finite vocabularies,
# not the generic code-shaped filter: host labels must never become causes.
R_CAUSES = frozenset((
    'relay-loss-missing', 'relay-loss-generator', 'relay-loss-target-insufficient',
    'relay-loss-restore-failed', 'relay-loss-call-lost', 'relay-loss-decryption',
    'relay-loss-duplicate', 'relay-loss-unaffected-restart', 'relay-loss-episode-count',
    'relay-loss-generation-mismatch', 'relay-loss-candidate-mismatch',
    'relay-loss-anchor-order', 'relay-loss-media-order-violated',
    'relay-loss-unrecovered', 'relay-loss-slow', 'unclassified'))


def r_value(value, allowed):
    return 'missing' if value is MISSING else value if isinstance(value, str) and value in allowed else 'unreadable'


def r_causes(value):
    if value is MISSING:
        return 'missing'
    if not isinstance(value, list):
        return 'unreadable'
    return ', '.join(r_value(v, R_CAUSES) for v in value) or 'none'


def part_r_lines(report):
    gates = report.get('gates')
    gate = gates[0] if isinstance(gates, list) and len(gates) == 1 else {}
    result = field(gate, 'process', 'relayLoss')
    completed = field(gate, 'procedureCompleted')
    lines = ['', '### Part R: relay host loss (5 s budget)', '',
             'Verdict: **' + r_value(field(gate, 'status'), ('passed', 'failed', 'inconclusive', 'not-run')) + '**',
             'Procedure completed: ' + ('yes' if completed is True else 'no' if completed is False else 'missing'),
             'Causes: ' + r_causes(field(result, 'causes'))]
    calls = field(result, 'calls')
    if not isinstance(calls, list):
        return lines + ['Call gaps: missing']
    lines += ['', '| Call | Role | Graded gap (ns) | Verdict | Causes |',
              '| --- | --- | ---: | --- | --- |']
    for call in calls:
        lines.append(f'| {count(field(call, "ordinal"))} | '
                     f'{r_value(field(call, "role"), ("affected", "unaffected", "unknown"))} | '
                     f'{count(field(call, "gapNs"))} | '
                     f'{r_value(field(call, "outcome"), ("pass", "fail", "inconclusive"))} | '
                     f'{r_causes(field(call, "causes"))} |')
    return lines


def clock_lines(status):
    cause = status.get('cause', MISSING)
    bounds = status.get('selectedBounds', MISSING)
    if bounds is MISSING:
        selected = 'none'
    else:
        selected = ', '.join(f'{name} {number(field(bounds, name))}' for name in BOUNDS)
    lines = [f'Cause: **{"admitted" if cause == "" else code(cause)}**', '', f'Selected bounds: {selected}']
    failed = status.get('failedWindows', [])
    lines += ['', f'Failed windows: {len(failed) if isinstance(failed, list) else "unreadable"}']
    if isinstance(failed, list) and failed:
        lines += ['', '| Window | Cause |', '| --- | --- |']
        lines += [f'| {i} | {code(field(window, "cause"))} |' for i, window in enumerate(failed, 1)]
    return lines


def summary(directory):
    report, report_note = load(directory, 'report.json')
    status, status_note = load(directory, 'clock-selection-status.json')
    lines = ['## Operator rehearsal summary', '']
    lines += [report_note] if report is None else report_lines(report)
    lines += ['', '### Clock selection', '']
    lines += [status_note] if status is None else clock_lines(status)
    return '\n'.join(lines) + '\n'


if __name__ == '__main__':
    sys.stdout.write(summary(Path(sys.argv[1])))
