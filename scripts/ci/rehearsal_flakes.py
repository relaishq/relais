#!/usr/bin/env python3
"""Classify untrusted report JSON; emit only fixed signatures and fixed reasons.

The workflow checks out this script and its loader from the default branch.
Load that sibling by its trusted path so the CLI also works with Python -I.
"""
import importlib.util
import os
from pathlib import Path
import sys

SPEC = importlib.util.spec_from_file_location(
    'rehearsal_summary', Path(__file__).resolve().with_name('rehearsal_summary.py'))
SUMMARY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(SUMMARY)

GATES = ('rollout', 'control-restart', 'upgrade-rollback', 'relay-deploy')
GENERATOR = {'generator-event-exclusion', 'generator-interval-ns-max'}
NOT_KNOWN = 'not-known'


def count(value):
    return type(value) is int and value >= 0


def clean_cpu(row, allow_exclusion=False):
    if not isinstance(row, dict):
        return False
    causes = row.get('causes')
    if not isinstance(causes, list) or not all(isinstance(c, str) for c in causes):
        return False
    if not ((row.get('passed') is True and causes == []) or
            (allow_exclusion and row.get('passed') is False and set(causes) == GENERATOR)):
        return False
    if any(type(row.get(k)) is not int or row[k] != 0
           for k in ('decoderDropped', 'decoderErrors', 'duplicatePackets')):
        return False
    checks = row.get('checks')
    return isinstance(checks, list) and all(
        isinstance(c, dict) and c.get('rule') == 'generator-interval-ns-max' and
        count(c.get('observed')) and count(c.get('limit')) and c['observed'] > c['limit']
        for c in checks)


def clean_media(media, expected_gates):
    if not isinstance(media, dict) or media.get('status') not in ('passed', 'inconclusive'):
        return False
    if any(type(media.get(k)) is not int or media[k] != 0
           for k in ('failedMetrics', 'decryptionFailures', 'iceRestarts')):
        return False
    diagnostics = media.get('phaseDiagnostics')
    if (not isinstance(diagnostics, list) or len(diagnostics) != len(expected_gates) or
            any(not isinstance(row, dict) for row in diagnostics) or
            [row.get('gate') for row in diagnostics] != list(expected_gates)):
        return False
    for row in diagnostics:
        if (row.get('included') is not True or row.get('cause') != '' or
              row.get('issue') != {'path': '', 'reason': ''}):
            return False
    return True


def classify(report):
    if not isinstance(report, dict) or type(report.get('schemaVersion')) is not int or report['schemaVersion'] != 1:
        return NOT_KNOWN, 'unsupported-report'
    if (report.get('verdict') != 'failed' or report.get('latched') is not False or
            report.get('cleanup') is not True or report.get('errors') != [] or
            report.get('uncertainties') != []):
        return NOT_KNOWN, 'not-a-clean-failure'
    failures = report.get('failures')
    if not isinstance(failures, list) or not failures or not all(isinstance(f, str) for f in failures):
        return NOT_KNOWN, 'missing-failures'
    gates = report.get('gates')
    if (not isinstance(gates, list) or len(gates) != len(GATES) or
            any(not isinstance(g, dict) for g in gates) or
            [g.get('gate') for g in gates] != list(GATES)):
        return NOT_KNOWN, 'gate-count-or-order-differs'
    cpu = report.get('cpu')
    if not isinstance(cpu, dict) or not clean_cpu(cpu.get('calibration')):
        return NOT_KNOWN, 'calibration-not-clean'
    # Preparation refusal never creates a gate media phase. Later gates must
    # still complete; prior-gate-stopped is not explained by this signature.
    refused = [g for g in gates if g.get('reason') == 'clock-lease-expired' and
               g.get('category', '') == '' and g.get('procedureCompleted') is False]
    gate_name = refused[0]['gate'] if len(refused) == 1 else None
    population = tuple(g for g in GATES if g != gate_name)
    phases = cpu.get('phases')
    if (not isinstance(phases, list) or len(phases) != len(population) or
            not all(clean_cpu(p, gate_name is None) for p in phases)):
        return NOT_KNOWN, 'generator-evidence-not-clean'
    clock = report.get('clockSelection')
    coverage = report.get('clockCoverage')
    if (not isinstance(clock, dict) or clock.get('cause') != '' or
            not isinstance(coverage, dict) or coverage.get('falsified') is not False or
            coverage.get('drainComplete') is not True):
        return NOT_KNOWN, 'clock-evidence-not-clean'
    expected = {'pooled-media-not-passed'}
    if gate_name:
        # Evaluate also emits media-missing-or-failed for the absent phase.
        expected |= {f'{gate_name}:procedure-incomplete', f'{gate_name}:not-passed',
                     f'{gate_name}:procedure-error', f'{gate_name}:media-missing-or-failed'}
    if set(failures) != expected:
        return NOT_KNOWN, 'unexplained-failure-code'
    for i, gate in enumerate(gates):
        if gate.get('status') not in ('passed', 'inconclusive') or gate.get('callerStopped') is not True:
            return NOT_KNOWN, 'gate-evidence-not-clean'
        if gate['gate'] == gate_name:
            if gate.get('status') != 'inconclusive' or gate.get('media') is not None:
                return NOT_KNOWN, 'not-a-preparation-refusal'
        elif (gate.get('procedureCompleted') is not True or gate.get('reason', '') != '' or
              gate.get('category', '') != ''):
            return NOT_KNOWN, 'unexplained-gate-refusal'
        elif not clean_media(gate.get('media'), tuple(g for g in GATES[:i + 1] if g != gate_name)):
            return NOT_KNOWN, 'gate-evidence-not-clean'
    pooled = report.get('pooledMedia')
    if not clean_media(pooled, population):
        return NOT_KNOWN, 'pooled-media-not-clean'
    if any(type(pooled.get(k)) is not int or pooled[k] != 0 for k in ('lostCalls', 'duplicatePackets')):
        return NOT_KNOWN, 'media-integrity-failure'
    timing = report.get('timingUndecided')
    allowed = {'metric-undecided', 'clock-coverage-missing'}
    if gate_name:
        allowed = {'metric-undecided', 'trial-count-insufficient'}
    if (not isinstance(timing, list) or (not timing and not gate_name) or any(
            not isinstance(r, dict) or not isinstance(r.get('causes'), list) or not r['causes'] or
            any(not isinstance(c, str) or c not in allowed for c in r['causes']) for r in timing)):
        return NOT_KNOWN, 'unexplained-timing-cause'
    moves, takeovers = pooled.get('qualifiedMoves'), pooled.get('qualifiedTakeovers')
    if (not count(moves) or not count(takeovers) or any(
            not count(pooled.get(k)) for k in
            ('includedPhases', 'droppedPhases', 'expectedCalls', 'completedCalls', 'relayCalls'))):
        return NOT_KNOWN, 'missing-qualification-counts'
    if (pooled.get('completeMedia') is not True or pooled.get('includedPhases') != len(population) or
            pooled.get('droppedPhases') != 0 or pooled.get('expectedCalls') != len(population) or
            pooled.get('completedCalls') != len(population) or
            pooled.get('relayCalls') != int('relay-deploy' in population)):
        return NOT_KNOWN, 'incomplete-media-population'
    if gate_name:
        return 'lease-expired-flake', 'explicit-clock-lease-expired-preparation-refusal'
    if (pooled.get('status') != 'inconclusive' or not 0 < moves < 20 or takeovers < 10 or
            not any(p['passed'] is False and p['checks'] for p in phases)):
        return NOT_KNOWN, 'not-generator-move-shortfall'
    return 'generator-saturation-flake', 'generator-exclusion-interval-max-and-move-shortfall'


def classify_directory(directory):
    try:
        report, _ = SUMMARY.load(directory, 'report.json')
    except RecursionError:
        return NOT_KNOWN, 'report-missing-or-malformed'
    if report is None:
        return NOT_KNOWN, 'report-missing-or-malformed'
    return classify(report)


def main():
    signature, reason = classify_directory(Path(sys.argv[1]))
    print(f'signature={signature}\nreason={reason}')
    if os.environ.get('GITHUB_STEP_SUMMARY'):
        with open(os.environ['GITHUB_STEP_SUMMARY'], 'a', encoding='utf-8') as output:
            output.write(f'## Operator rehearsal automatic rerun\n\n{signature}: {reason}\n')


if __name__ == '__main__':
    main()
