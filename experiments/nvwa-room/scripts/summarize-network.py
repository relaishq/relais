#!/usr/bin/env python3
"""Merge uploaded stage-4 artifacts; retain raw tails, losses and failed cells."""
import argparse
import collections
import gzip
import json
import math
import pathlib


def percentiles(values):
    values = sorted(values)
    if not values:
        return {'n': 0, 'p50': None, 'p95': None, 'p99': None, 'max': None}
    return {'n': len(values), **{name: values[max(0, math.ceil(q * len(values)) - 1)]
                               for name, q in [('p50', .5), ('p95', .95), ('p99', .99), ('max', 1)]}}


def summarize(root):
    groups = collections.defaultdict(list)
    ledgers = [row for path in root.rglob('ledger.json') for row in json.loads(path.read_text())]
    for path in sorted(root.rglob('result.json.gz')):
        with gzip.open(path, 'rt') as source:
            run = json.load(source)
        groups[(run['profile'], run['variant'])].append((path, run))
    plans = {plan['profile']: plan for path in root.rglob('matrix-plan.json') for plan in [json.loads(path.read_text())]}
    summary = []
    for (profile, variant), runs in sorted(groups.items()):
        rtts, ages, lags, gaps, recoveries, wire_sizes = [], [], [], [], collections.defaultdict(list), collections.defaultdict(list)
        counts = collections.Counter({key: 0 for key in ['runs', 'commands_expected', 'commands_delivered', 'states_expected', 'states_unique', 'state_deliveries', 'state_duplicates', 'command_duplicate_receipts', 'state_reordered', 'stale_states_at_room', 'dedup_checks_passed']})
        errors = []
        for path, run in runs:
            errors.extend(f'{path.parent.name}: {error}' for error in (run['errors'] or []))
            counts['runs'] += 1
            if run['proof']['recovery_ms'] is not None:
                recoveries[run['fault']].append(run['proof']['recovery_ms'])
            counts['dedup_checks_passed'] += int(run['room_metrics']['deduplicated'] == 1)
            for player in run['players']:
                counts['commands_expected'] += (run['messages_per_client'] + 1) // 2
                counts['states_expected'] += run['messages_per_client'] // 2
                seen = set()
                receipts = set()
                previous = None
                high = 0
                for message in (player['messages'] or []):
                    wire_sizes[message['type']].append(message['bytes'])
                    lags.append(message['schedule_lag_ms'])
                    if message['type'] != 'state' and not message.get('error'):
                        rtts.append(message['rtt_ms'])
                        counts['commands_delivered'] += 1
                for observation in (player['observations'] or []):
                    reply = observation['reply']
                    if not run['StartNS'] <= observation['at_ns'] <= run['EndNS']:
                        continue
                    if reply['type'] == 'result' and reply.get('request_id'):
                        request_id = reply['request_id']
                        counts['command_duplicate_receipts'] += int(request_id in receipts)
                        receipts.add(request_id)
                    if reply['type'] != 'state-update':
                        continue
                    key = (reply['player_id'], reply['seq'])
                    counts['state_deliveries'] += 1
                    if key in seen:
                        counts['state_duplicates'] += 1
                        continue
                    seen.add(key)
                    counts['states_unique'] += 1
                    counts['state_reordered'] += int(reply['seq'] < high)
                    high = max(high, reply['seq'])
                    ages.append((observation['at_ns'] - reply['sent_ns']) / 1e6)
                    if previous is not None:
                        gaps.append((observation['at_ns'] - previous) / 1e6)
                    previous = observation['at_ns']
            counts['stale_states_at_room'] += run['room_metrics']['stale_state']
        cells = [row for row in ledgers if row['profile'] == profile and row['variant'] == variant]
        faults = ['normal', 'disconnect', 'worker-kill', 'lost-receipt']
        trials = plans.get(profile, {}).get('trials', max([row['trial'] for row in cells], default=2))
        expected = {(fault, trial) for fault in faults for trial in range(1, trials + 1)}
        observed = {(row['fault'], row['trial']) for row in cells}
        failed = [row['name'] for row in cells if row['exit_code']]
        summary.append({'profile': profile, 'variant': variant, 'counts': dict(counts),
                        'command_rtt_ms': percentiles(rtts), 'state_age_ms': percentiles(ages),
                        'state_gap_ms': percentiles(gaps), 'send_schedule_lag_ms': percentiles(lags),
                        'message_bytes': {kind: percentiles(sizes) for kind, sizes in wire_sizes.items()},
                        'recovery_ms': {kind: percentiles(values) for kind, values in recoveries.items()},
                        'errors': errors, 'failed_cells': failed, 'ledger_cells': len(cells),
                        'complete': not errors and not failed and observed == expected and len(cells) == len(runs)})
    # A startup failure may produce no result at all; ledger failure is still visible.
    return {'schema': 1, 'groups': summary, 'failed_cells': [row for row in ledgers if row['exit_code']]}



def merge_summaries(root):
    """Fill the report using only the uploaded per-profile summaries."""
    groups, failures = {}, {}
    for path in sorted(root.rglob('summary.json')):
        document = json.loads(path.read_text())
        if document.get('schema') != 1:
            raise ValueError(f'unsupported summary schema: {path}')
        for group in document['groups']:
            key = (group['profile'], group['variant'])
            if key in groups and groups[key] != group:
                raise ValueError(f'conflicting summaries for {key}: {path}')
            groups[key] = group
        for failure in document['failed_cells']:
            failures[failure['name']] = failure
    return {'schema': 1, 'groups': [groups[key] for key in sorted(groups)],
            'failed_cells': [failures[key] for key in sorted(failures)]}


def fmt(value):
    return 'NOT-RUN' if value is None else f'{value:.2f}'


def table(summary):
    lines = ['| Profile | Transport | Cmd RTT p50/p95/p99/max ms | State age p50/p95/p99 ms | Longest state gap ms | Commands delivered/expected | States unique/expected (duplicates) | Disconnect p50/max ms | Worker kill p50/max ms | Status |',
             '|---|---|---|---|---|---|---|---|---|---|']
    for group in summary['groups']:
        count = group['counts']
        rtt = '/'.join(fmt(group['command_rtt_ms'][q]) for q in ['p50', 'p95', 'p99', 'max'])
        age = '/'.join(fmt(group['state_age_ms'][q]) for q in ['p50', 'p95', 'p99'])
        recovery = lambda fault: '/'.join(fmt(group['recovery_ms'].get(fault, {}).get(q)) for q in ['p50', 'max'])
        lines.append(f"| {group['profile']} | {group['variant']} | {rtt} | {age} | {fmt(group['state_gap_ms']['max'])} | "
                     f"{count['commands_delivered']}/{count['commands_expected']} | {count['states_unique']}/{count['states_expected']} ({count['state_duplicates']}) | "
                     f"{recovery('disconnect')} | {recovery('worker-kill')} | {'PASS' if group['complete'] else 'PARTIAL/FAIL'} |")
    if not summary['groups']:
        lines.append('| Pending Linux CI | ws / dc-reliable / dc-unreliable | NOT-RUN | NOT-RUN | NOT-RUN | NOT-RUN | NOT-RUN | NOT-RUN | NOT-RUN | NOT-RUN |')
    if summary['failed_cells']:
        lines.extend(['', 'Failed cells (including startup failures with no raw result): ' + ', '.join(row['name'] for row in summary['failed_cells'])])
    return '\n'.join(lines) + '\n'


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('artifact_root', type=pathlib.Path)
    parser.add_argument('--readme', type=pathlib.Path)
    parser.add_argument('--from-summaries', action='store_true', help='merge uploaded summary.json files without parsing raw results')
    args = parser.parse_args()
    result = merge_summaries(args.artifact_root) if args.from_summaries else summarize(args.artifact_root)
    rendered = table(result)
    (args.artifact_root / 'summary.json').write_text(json.dumps(result, indent=2) + '\n')
    (args.artifact_root / 'summary.md').write_text(rendered)
    if args.readme:
        text = args.readme.read_text()
        before, rest = text.split('<!-- RESULTS:START -->', 1)
        _, after = rest.split('<!-- RESULTS:END -->', 1)
        args.readme.write_text(before + '<!-- RESULTS:START -->\n\n' + rendered + '\n<!-- RESULTS:END -->' + after)
    print(rendered)
