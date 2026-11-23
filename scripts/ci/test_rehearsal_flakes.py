"""Fail closed before granting the one rehearsal retry."""
import copy
import json
import os
from pathlib import Path
import re
import tempfile
import subprocess
import sys
import unittest

from rehearsal_summary import load
from rehearsal_flakes import classify, classify_directory

ROOT = Path(__file__).resolve().parents[2]
FIXTURES = Path(__file__).with_name('fixtures') / 'rehearsal-flakes'


def fixture(run):
    return json.loads((FIXTURES / f'operator-rehearsal-{run}.json').read_text())


def lease_fixture():
    return json.loads((FIXTURES / 'lease-preparation-refusal.json').read_text())


def clock_select_fixture():
    # Reduced from operator-rehearsal-44984258994-1.json to the fields the classifier reads.
    return {
        'schemaVersion': 1, 'verdict': 'failed', 'latched': False, 'cleanup': True,
        'errors': ['clock-binding-failed', 'clock-binding-missing'],
        'failures': ['clock-binding-failed', 'clock-binding-missing', 'calibration-failed',
                     'gate-count-differs', 'pooled-media-not-passed'],
        'uncertainties': [], 'gates': [], 'pooledMedia': None, 'clocks': [],
        'callerTiming': [], 'timingUndecided': [], 'exemptions': [],
        'clockSelection': {'cause': 'clock-select-ceiling',
                           'failedWindows': [{'cause': 'clock-select-ceiling'}]},
        'cpu': {
            'callerAffinities': [], 'phases': [],
            'calibration': {
                'passed': False, 'checks': [], 'causes': ['incomplete-calibration-evidence'],
                'samples': 0, 'mediaNs': 0, 'flaggedNs': 0, 'flaggedPercent': 0,
                'peakP99Ns': 0, 'peakMaxNs': 0, 'peakCpuPerGomaxprocs': 0,
                'decoderDropped': 0, 'decoderErrors': 0, 'completedCalls': 0,
                'duplicatePackets': 0,
                'mediaWindow': {'firstVideoSendOffsetMs': 0, 'hangUpOffsetMs': 0,
                                'flaggedIntervals': [], 'clippedFlaggedMs': 0,
                                'mediaWindowMs': 0},
            },
        },
    }


class SummaryLoaderCharacterization(unittest.TestCase):
    def test_object_and_fixed_failure_notes(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            self.assertEqual(load(path, 'report.json'), (None, '`report.json` missing.'))
            for raw in (b'{', b'[]', b'null', b'"untrusted"', b'\xff'):
                with self.subTest(raw=raw):
                    (path / 'report.json').write_bytes(raw)
                    self.assertEqual(load(path, 'report.json'),
                                     (None, '`report.json` cannot be parsed.'))
            (path / 'report.json').write_text('{"schemaVersion": 1}')
            self.assertEqual(load(path, 'report.json'), ({'schemaVersion': 1}, None))

    def test_unreadable_report_uses_fixed_note(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            (path / 'report.json').mkdir()
            self.assertEqual(load(path, 'report.json'), (None, '`report.json` cannot be read.'))


class FlakeClassification(unittest.TestCase):
    def test_real_clock_selection_report(self):
        self.assertEqual(classify(fixture('44984258994-1')),
                         ('clock-select-flake', 'clock-select-ceiling-before-any-gate'))

    def test_clock_selection_ceiling_before_any_gate(self):
        self.assertEqual(classify(clock_select_fixture()),
                         ('clock-select-flake', 'clock-select-ceiling-before-any-gate'))

    def test_clock_selection_requires_only_ceiling_failure_before_any_work(self):
        changes = [
            (('schemaVersion',), True), (('verdict',), 'passed'), (('latched',), True),
            (('cleanup',), False), (('gates',), [{'gate': 'rollout'}]),
            (('errors',), ['clock-binding-failed', 'clock-binding-missing', 'unclassified']),
            (('errors',), ['clock-binding-failed']),
            (('failures',), clock_select_fixture()['failures'] + ['unclassified']),
            (('failures',), clock_select_fixture()['failures'][:-1]),
            (('failures',), clock_select_fixture()['failures'][:-1] + ['clock-binding-failed']),
            (('failures',), [{}]),
            (('clockSelection', 'cause'), 'clock-select-probes'),
            (('clockSelection', 'failedWindows'), []),
            (('clockSelection', 'failedWindows'), [None]),
            (('clockSelection', 'failedWindows'), [{}]),
            (('clockSelection', 'failedWindows'), [{'cause': 'clock-select-probes'}]),
            (('clockSelection', 'failedWindows'), [{'cause': 'clock-select-ceiling'},
                                                  {'cause': 'clock-select-probes'}]),
            (('uncertainties',), ['unknown']), (('pooledMedia',), {}),
            (('clocks',), [{}]), (('callerTiming',), [{}]),
            (('timingUndecided',), [{}]), (('exemptions',), ['unknown']),
            (('cpu', 'callerAffinities'), ['0,1']), (('cpu', 'phases'), [{}]),
            (('cpu', 'calibration', 'passed'), True),
            (('cpu', 'calibration', 'checks'), [{}]),
            (('cpu', 'calibration', 'causes'), ['unclassified']),
            (('cpu', 'calibration', 'mediaWindow', 'flaggedIntervals'), [{}]),
        ]
        calibration = clock_select_fixture()['cpu']['calibration']
        for key, value in calibration.items():
            if type(value) is int:
                changes.extend((('cpu', 'calibration', key), changed) for changed in (1, False, 0.0))
        for key, value in calibration['mediaWindow'].items():
            if type(value) is int:
                changes.extend((('cpu', 'calibration', 'mediaWindow', key), changed)
                               for changed in (1, False, 0.0))
        for path, value in changes:
            with self.subTest(path=path, value=value):
                report = clock_select_fixture()
                owner = report
                for key in path[:-1]:
                    owner = owner[key]
                owner[path[-1]] = value
                self.assertEqual(classify(report)[0], 'not-known')

    def test_clock_selection_requires_all_evidence_fields(self):
        def paths(row, prefix=()):
            for key, value in row.items():
                yield prefix + (key,)
                if isinstance(value, dict):
                    yield from paths(value, prefix + (key,))
        for path in paths(clock_select_fixture()):
            with self.subTest(missing=path):
                report = clock_select_fixture()
                owner = report
                for key in path[:-1]:
                    owner = owner[key]
                del owner[path[-1]]
                self.assertEqual(classify(report)[0], 'not-known')

    def test_historical_generator_reports(self):
        # The two 4371 reports have the same JSON generator evidence as the
        # 4368 reports. Neither has a JSON precondition refusal.
        for run in ('43668182387-1', '43683756709-1', '43713479010-1', '43713957392-1'):
            with self.subTest(run=run):
                report = fixture(run)
                self.assertEqual(report['pooledMedia']['excludedEvents'], 0)
                self.assertEqual(report['pooledMedia']['saturationOverlaps'], 0)
                self.assertEqual(classify(report)[0], 'generator-saturation-flake')

    def test_generator_does_not_require_pooled_counters(self):
        # operator-rehearsal-43683756709-1.json has both counters at zero:
        # its pooled failure comes from a clock-coverage-missing timing row, so
        # these pooled event counters are not evidence required by this rule.
        report = fixture('43683756709-1')
        for field in ('excludedEvents', 'saturationOverlaps'):
            del report['pooledMedia'][field]
        self.assertEqual(classify(report)[0], 'generator-saturation-flake')

    def test_real_non_flakes_and_insufficient_lease_evidence(self):
        for run in ('43779279943-1', '43776714400-1', '43743583263-1', '43713957392-2',
                    '43774355478-1'):
            with self.subTest(run=run):
                self.assertEqual(classify(fixture(run))[0], 'not-known')

    def test_explicit_json_lease_refusal_is_required(self):
        # Generated and pinned by TestLeasePreparationFlakeFixture in Go.
        report = lease_fixture()
        self.assertEqual(classify(report)[0], 'lease-expired-flake')
        for reason in ('clock-lease-missing', 'unclassified', 'precondition-failed'):
            with self.subTest(reason=reason):
                report['gates'][1]['reason'] = reason
                self.assertEqual(classify(report)[0], 'not-known')

    def test_lease_requires_absent_phase_and_completed_later_gates(self):
        base = lease_fixture()
        changes = [
            (('gates', 1, 'category'), 'clock-lease-expired'),
            (('gates', 1, 'media'), base['gates'][0]['media']),
            (('gates', 2, 'reason'), 'prior-gate-stopped'),
            (('gates', 3, 'procedureCompleted'), False),
            (('cpu', 'phases', 0, 'passed'), False),
            (('pooledMedia', 'includedPhases'), 4),
        ]
        for path, value in changes:
            with self.subTest(path=path):
                report = copy.deepcopy(base)
                owner = report
                for key in path[:-1]:
                    owner = owner[key]
                owner[path[-1]] = value
                self.assertEqual(classify(report)[0], 'not-known')
        for code in base['failures']:
            with self.subTest(missing_failure=code):
                report = copy.deepcopy(base)
                report['failures'].remove(code)
                self.assertEqual(classify(report)[0], 'not-known')

    def test_any_extra_failure_prevents_both_signatures(self):
        generator = fixture('43683756709-1')
        lease = lease_fixture()
        for report in (generator, lease):
            for code in ('unclassified', 'calibration-failed', 'gate-count-differs',
                         'relay-deploy:procedure-error', 'new-failure'):
                with self.subTest(signature=classify(report)[0], code=code):
                    changed = copy.deepcopy(report)
                    changed['failures'].append(code)
                    self.assertEqual(classify(changed)[0], 'not-known')

    def test_missing_or_malformed_report(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            self.assertEqual(classify_directory(path), ('not-known', 'report-missing-or-malformed'))
            for raw in ('{', '[]', 'null', 'true', '"unknown"', '[' * 2000 + ']' * 2000):
                with self.subTest(raw=raw):
                    (path / 'report.json').write_text(raw)
                    self.assertEqual(classify_directory(path)[0], 'not-known')

    def test_missing_fields_wrong_types_latches_and_other_evidence(self):
        base = fixture('43683756709-1')
        changes = [
            (('schemaVersion',), True), (('verdict',), 'passed'), (('latched',), True),
            (('cleanup',), False), (('errors',), ['unclassified']), (('uncertainties',), ['unknown']),
            (('failures',), []), (('failures',), [{}]), (('gates',), []),
            (('cpu', 'calibration', 'passed'), False),
            (('cpu', 'phases', 0, 'causes'), ['generator-event-exclusion']),
            (('cpu', 'phases', 0, 'checks'), [{'rule': 'unknown'}]),
            (('cpu', 'phases', 0, 'decoderErrors'), 1),
            (('gates', 0, 'callerStopped'), False), (('gates', 0, 'procedureCompleted'), False),
            (('gates', 0, 'reason'), 'precondition-failed'),
            (('gates', 0, 'media', 'failedMetrics'), 1),
            (('gates', 0, 'media', 'phaseDiagnostics', 0, 'cause'), 'unclassified'),
            (('clockSelection', 'cause'), 'clock-select-probes'),
            (('clockCoverage', 'falsified'), True), (('clockCoverage', 'drainComplete'), False),
            (('pooledMedia', 'qualifiedMoves'), 20), (('pooledMedia', 'qualifiedMoves'), True),
            (('pooledMedia', 'qualifiedTakeovers'), 9), (('pooledMedia', 'lostCalls'), 1),
            (('pooledMedia', 'relayCalls'), True), (('pooledMedia', 'droppedPhases'), False),
            (('pooledMedia', 'completeMedia'), False),
            (('timingUndecided',), [{'causes': ['trial-count-insufficient']}]),
            (('timingUndecided',), [{'causes': ['unclassified']}]),
        ]
        for path, value in changes:
            with self.subTest(path=path, value=value):
                report = copy.deepcopy(base)
                owner = report
                for key in path[:-1]:
                    owner = owner[key]
                owner[path[-1]] = value
                self.assertEqual(classify(report)[0], 'not-known')
        for key in base:
            with self.subTest(missing=key):
                report = copy.deepcopy(base)
                del report[key]
                self.assertEqual(classify(report)[0], 'not-known')

    def test_generator_requires_positive_exclusion_evidence(self):
        report = fixture('43683756709-1')
        for phase in report['cpu']['phases']:
            phase.update(passed=True, causes=[])
        self.assertEqual(classify(report)[0], 'not-known')

    def test_phase_diagnostics_match_the_exact_population(self):
        for base in (fixture('43683756709-1'), lease_fixture()):
            media_paths = [('pooledMedia',)] + [('gates', i, 'media') for i, g in enumerate(base['gates'])
                                               if g.get('media') is not None]
            for path in media_paths:
                for change in ('missing', 'extra', 'duplicate'):
                    with self.subTest(path=path, change=change):
                        report = copy.deepcopy(base)
                        media = report
                        for key in path:
                            media = media[key]
                        rows = media['phaseDiagnostics']
                        if change == 'missing':
                            rows.pop()
                        elif change == 'extra':
                            rows.append(copy.deepcopy(rows[0]))
                        else:
                            rows[0]['gate'] = 'unknown-gate'
                        self.assertEqual(classify(report)[0], 'not-known')

    def test_isolated_cli_emits_no_untrusted_text(self):
        report = fixture('43683756709-1')
        report['failures'].append('untrusted\nsignature=lease-expired-flake\n$(false)')
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            (path / 'report.json').write_text(json.dumps(report))
            result = subprocess.run([sys.executable, '-I', str(ROOT / 'scripts/ci/rehearsal_flakes.py'),
                                     directory], env={**os.environ, 'GITHUB_STEP_SUMMARY': str(path / 'summary')},
                                    capture_output=True, text=True, check=True)
            self.assertEqual(result.stdout, 'signature=not-known\nreason=unexplained-failure-code\n')
            self.assertNotIn('untrusted', (path / 'summary').read_text())


def tracking_issue(source):
    values = re.findall(r'^  RERUN_TRACKING_ISSUE: (.*)$', source, re.MULTILINE)
    assert len(values) == 1, values
    return values[0]


class WorkflowGuard(unittest.TestCase):
    def test_rerun_records_go_to_one_tracking_issue(self):
        source = (ROOT / '.github/workflows/operator-rehearsal-flake-rerun.yml').read_text()
        issue = tracking_issue(source)
        self.assertRegex(issue, r'^[1-9][0-9]*$')
        self.assertNotEqual(issue, '481')
        self.assertNotIn('481', source)
        # The number is defined once; the step reads it from the workflow env.
        self.assertEqual(len(re.findall(rf'\b{issue}\b', source)), 1)
        self.assertIn('gh api "repos/$GH_REPO/issues/$RERUN_TRACKING_ISSUE/comments"', source)
        docs = (ROOT / 'docs/dev/ci.md').read_text()
        section = docs.split('## One automatic rehearsal rerun', 1)[1].split('\n## ', 1)[0]
        self.assertIn(f'[#{issue}](https://github.com/relaishq/relais/issues/{issue})', section)
        self.assertNotIn('481', section)

    def test_attempt_and_event_guards_precede_privileged_steps(self):
        source = (ROOT / '.github/workflows/operator-rehearsal-flake-rerun.yml').read_text()
        guard = source.split('    if: >-', 1)[1].split('    runs-on:', 1)[0]
        self.assertIn("github.event.workflow_run.conclusion == 'failure'", guard)
        self.assertIn('github.event.workflow_run.run_attempt == 1', guard)
        self.assertIn("github.event.workflow_run.path == '.github/workflows/operator-rehearsal.yml'", guard)
        self.assertIn("github.event.workflow_run.event == 'pull_request'", guard)
        self.assertIn("github.event.workflow_run.event == 'push'", guard)
        # Attempt 2 (including a failed rerun) cannot satisfy the job guard.
        self.assertNotIn('||', guard.split('run_attempt == 1', 1)[0])
        self.assertIn('ref: ${{ github.event.repository.default_branch }}', source)
        self.assertNotIn('head_sha', source)
        self.assertIn('python3 -I trusted/scripts/ci/rehearsal_flakes.py', source)
        self.assertIn('gh run rerun "$RUN_ID" --repo "$GH_REPO" --failed', source)

    def test_privileged_step_with_offline_api_stub(self):
        source = (ROOT / '.github/workflows/operator-rehearsal-flake-rerun.yml').read_text()
        script = source.rsplit('        run: |\n', 1)[1]
        script = '\n'.join(line[10:] for line in script.splitlines())
        issue = tracking_issue(source)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            stub = path / 'gh'
            stub.write_text('''#!/usr/bin/env python3
import json, os, sys
with open(os.environ['CALL_LOG'], 'a') as log:
    log.write(json.dumps(sys.argv[1:]) + '\\n')
if sys.argv[1:2] == ['api'] and 'comments' not in sys.argv[2]:
    print(os.environ['LIVE_ELIGIBLE'])
if sys.argv[1:3] == ['run', 'rerun']:
    sys.exit(int(os.environ['RERUN_EXIT']))
''')
            stub.chmod(0o755)
            for signature, eligible, rerun_exit, expected_count in (
                    ('generator-saturation-flake', 'true', '0', 3),
                    ('lease-expired-flake', 'true', '0', 3),
                    ('clock-select-flake', 'true', '0', 3),
                    ('generator-saturation-flake', 'false', '0', 1),
                    ('generator-saturation-flake', 'true', '1', 2),
                    ('not-known', 'true', '0', 0)):
                with self.subTest(signature=signature, eligible=eligible, rerun_exit=rerun_exit):
                    log = path / 'calls'
                    log.write_text('')
                    result = subprocess.run(['bash', '-c', script], capture_output=True, text=True,
                                            env={**os.environ, 'PATH': str(path) + os.pathsep + os.environ['PATH'],
                                                 'CALL_LOG': str(log), 'LIVE_ELIGIBLE': eligible,
                                                 'RERUN_EXIT': rerun_exit, 'SIGNATURE': signature,
                                                 'RUN_ID': '123', 'GH_REPO': 'relaishq/relais',
                                                 'RERUN_TRACKING_ISSUE': issue,
                                                 'GITHUB_SERVER_URL': 'https://github.com',
                                                 'GITHUB_STEP_SUMMARY': str(path / 'summary')})
                    calls = [json.loads(line) for line in log.read_text().splitlines()]
                    self.assertEqual(len(calls), expected_count, result.stderr)
                    self.assertEqual(result.returncode, int(rerun_exit) if signature != 'not-known' else 1)
                    if expected_count == 3:
                        self.assertEqual(calls[1], ['run', 'rerun', '123', '--repo', 'relaishq/relais', '--failed'])
                        self.assertEqual(calls[2], ['api', f'repos/relaishq/relais/issues/{issue}/comments', '-f',
                                                   f'body=Automatic rerun: {signature}; '
                                                   'https://github.com/relaishq/relais/actions/runs/123 '
                                                   '(attempt 1 -> 2).', '--silent'])


if __name__ == '__main__':
    unittest.main()
