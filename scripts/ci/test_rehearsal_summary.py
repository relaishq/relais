"""The run-page summary shows each listed report field and nothing else."""
import copy
import json
from pathlib import Path
import tempfile
import unittest

from rehearsal_summary import summary, report_lines

# Values the summary must never print, planted where real reports keep them.
SECRETS = ('10.0.3.7', 'ip-10-0-3-7', 'relay-host-a', 'dial tcp refused', 'caller-stop-unconfirmed',
           'a' * 64, 'b' * 64, 'host-diagnostics')


def gate(name, status, media):
    return {'gate': name, 'status': status, 'procedureCompleted': True, 'callerStopped': True,
            'process': {'completedSteps': 3}, 'media': {'status': media, 'qualifiedTakeovers': 9}}


def caller(gate_name, *evidence):
    return {'gate': gate_name, 'probes': 4,
            'events': [{'kind': 'move', 'startUnixMs': 1, 'timingEvidence': e} for e in evidence]}


PASSED = {
    'schemaVersion': 1, 'verdict': 'passed', 'commit': 'c' * 40,
    'clockSelection': {'cause': '', 'receiptSha256': 'a' * 64,
                       'updates': {'relay-host-a': 3}},
    'gates': [gate('rollout', 'passed', 'passed'), gate('control-restart', 'passed', 'inconclusive'),
              gate('upgrade-rollback', 'passed', 'passed'), gate('relay-deploy', 'inconclusive', 'passed')],
    'pooledMedia': {'status': 'passed', 'qualifiedTakeovers': 10, 'qualifiedMoves': 20,
                    'undecidedMetrics': []},
    'timingUndecided': [],
    'errors': ['dial tcp refused 10.0.3.7'], 'failures': ['rollout:caller-stop-unconfirmed'],
    'limitations': ['host-diagnostics'],
    'callerTiming': [caller('rollout', False, False, True), caller('relay-deploy')],
    'headCaller': {'commit': 'd' * 40, 'callerTiming': [caller('rollout', True, False)],
                   'errors': ['ip-10-0-3-7']},
}
ADMITTED = {'cause': '', 'selectedBounds': {'frontendControlBoundNs': 120000, 'frontendWorkerBoundNs': 170000,
                                            'driftPpm': 100},
            'updates': {'relay-host-a': 92}, 'receiptSha256': 'a' * 64, 'evidenceSha256': 'b' * 64}


def inconclusive():
    report = copy.deepcopy(PASSED)
    report['verdict'] = 'inconclusive'
    report['pooledMedia'].update(status='inconclusive', qualifiedTakeovers=7, qualifiedMoves=15)
    report['timingUndecided'] = [
        {'gate': 'rollout', 'phase': 0, 'event': 2, 'metric': 'distributed_window',
         'causes': ['metric-undecided', 'trial-count-insufficient']},
        {'gate': 'control-restart', 'phase': 1, 'event': 0, 'metric': 'first_content',
         'causes': ['trusted-coverage-below-80-percent']}]
    return report


class Summary(unittest.TestCase):
    def render(self, report=None, status=None, raw=None):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            if report is not None:
                (path / 'report.json').write_text(json.dumps(report))
            if raw is not None:
                (path / 'report.json').write_bytes(raw)
            if status is not None:
                (path / 'clock-selection-status.json').write_text(json.dumps(status))
            return summary(path)

    def assertPrivate(self, text):
        for secret in SECRETS:
            self.assertNotIn(secret, text)

    def test_abm_summary_bytes(self):
        for name, report in (("passed", PASSED), ("inconclusive", inconclusive())):
            expected = (Path(__file__).parent / "testdata" / f"abm-summary-{name}.md").read_text()
            self.assertEqual(self.render(report, ADMITTED), expected)

    def test_passed_report_shows_each_listed_value(self):
        text = self.render(PASSED, ADMITTED)
        lines = text.splitlines()
        self.assertIn('Verdict: **passed**', lines)
        for row in ('| rollout | passed | passed |', '| control-restart | passed | inconclusive |',
                    '| upgrade-rollback | passed | passed |', '| relay-deploy | inconclusive | passed |'):
            self.assertIn(row, lines)
        self.assertIn('Pooled media: status **passed**, qualified takeovers 10, qualified moves 20', lines)
        self.assertEqual(lines[lines.index('### Timing undecided') + 2], 'none')
        self.assertIn('- Caller events with `timingEvidence: false`, release callers (`callerTiming`): '
                      '2 of 3 events', lines)
        self.assertIn('- Caller events with `timingEvidence: false`, head caller (`headCaller.callerTiming`): '
                      '1 of 2 events', lines)
        self.assertIn('Cause: **admitted**', lines)
        self.assertIn('Selected bounds: frontendControlBoundNs 120000, frontendWorkerBoundNs 170000, driftPpm 100',
                      lines)
        self.assertPrivate(text)

    def test_inconclusive_report_lists_timing_undecided_rows(self):
        text = self.render(inconclusive(), ADMITTED)
        lines = text.splitlines()
        self.assertIn('Verdict: **inconclusive**', lines)
        self.assertIn('Pooled media: status **inconclusive**, qualified takeovers 7, qualified moves 15', lines)
        self.assertIn('| rollout | 0 | 2 | distributed_window | metric-undecided, trial-count-insufficient |', lines)
        self.assertIn('| control-restart | 1 | 0 | first_content | trusted-coverage-below-80-percent |', lines)
        self.assertPrivate(text)

    def test_failed_report_without_head_caller(self):
        report = copy.deepcopy(PASSED)
        report['verdict'] = 'failed'
        report['gates'][0].update(status='failed', media=None)
        del report['headCaller']
        lines = self.render(report, ADMITTED).splitlines()
        self.assertIn('Verdict: **failed**', lines)
        self.assertIn('| rollout | failed | missing |', lines)
        self.assertIn('- Caller events with `timingEvidence: false`, head caller (`headCaller.callerTiming`): '
                      'missing', lines)

    def test_clock_refusal_shows_cause_without_bounds(self):
        refusal = {'cause': 'clock-lease-expired', 'leaseRefusal': {'slot': 'relay-host-a'}}
        text = self.render(inconclusive(), refusal)
        lines = text.splitlines()
        self.assertIn('Cause: **clock-lease-expired**', lines)
        self.assertIn('Selected bounds: none', lines)
        self.assertPrivate(text)

    def test_absent_cause_is_not_admitted(self):
        lines = self.render(PASSED, {'selectedBounds': ADMITTED['selectedBounds']}).splitlines()
        self.assertIn('Cause: **missing**', lines)

    def test_failed_windows_survive_admission_without_private_fields(self):
        status = dict(ADMITTED, failedWindows=[
            {'cause': 'clock-select-updates', 'updates': {'relay-host-a': 9}},
            {'cause': 'clock-select-probes', 'diagnostics': 'host-diagnostics'}])
        text = self.render(PASSED, status)
        self.assertIn('Failed windows: 2', text.splitlines())
        self.assertIn('| 1 | clock-select-updates |', text.splitlines())
        self.assertIn('| 2 | clock-select-probes |', text.splitlines())
        self.assertPrivate(text)

    def test_failed_windows_absent_empty_or_invalid(self):
        for value, expected in (([], '0'), (None, 'unreadable'), ({}, 'unreadable')):
            text = self.render(PASSED, dict(ADMITTED, failedWindows=value))
            self.assertIn('Failed windows: ' + expected, text.splitlines())
        self.assertIn('Failed windows: 0', self.render(PASSED, ADMITTED).splitlines())
        text = self.render(PASSED, dict(ADMITTED, failedWindows=[None, {'cause': '10.0.3.7'}]))
        self.assertIn('| 1 | missing |', text.splitlines())
        self.assertIn('| 2 | unreadable |', text.splitlines())
        self.assertPrivate(text)

    def test_missing_report_still_shows_clock_status(self):
        lines = self.render(status=ADMITTED).splitlines()
        self.assertIn('`report.json` missing.', lines)
        self.assertIn('Cause: **admitted**', lines)
        self.assertNotIn('Verdict', '\n'.join(lines))

    def test_missing_clock_status_still_shows_report(self):
        lines = self.render(PASSED).splitlines()
        self.assertIn('Verdict: **passed**', lines)
        self.assertIn('`clock-selection-status.json` missing.', lines)

    def test_unparsable_report_is_named_without_error_text(self):
        for raw in (b'{"verdict": "passed", 10.0.3.7', b'["10.0.3.7"]', b'\xff\xfe'):
            with self.subTest(raw=raw):
                text = self.render(status=ADMITTED, raw=raw)
                self.assertIn('`report.json` cannot be parsed.', text.splitlines())
                self.assertIn('Cause: **admitted**', text.splitlines())
                self.assertPrivate(text)

    def test_unknown_schema_version_shows_no_report_fields(self):
        for version in (2, True, '1', None):
            with self.subTest(version=version):
                report = dict(PASSED, schemaVersion=version)
                text = self.render(report, ADMITTED)
                self.assertIn('`report.json` schema version is not 1; its fields are not shown.', text.splitlines())
                self.assertNotIn('Verdict', text)

    def test_free_text_and_wrong_types_are_unreadable(self):
        report = copy.deepcopy(inconclusive())
        report['verdict'] = '10.0.3.7'
        report['gates'][0]['gate'] = 'ip-10-0-3-7 | x'
        report['pooledMedia'].update(qualifiedTakeovers=True, qualifiedMoves=-1)
        report['timingUndecided'][0].update(phase='0', causes='dial tcp refused')
        report['headCaller']['callerTiming'][0]['events'][0]['timingEvidence'] = 'false'
        report['callerTiming'] = '10.0.3.7'
        status = {'cause': 'dial tcp refused', 'selectedBounds': {'frontendControlBoundNs': '120000',
                                                                  'driftPpm': float('nan')}}
        text = self.render(report, status)
        lines = text.splitlines()
        self.assertIn('Verdict: **unreadable**', lines)
        self.assertIn('| unreadable | passed | passed |', lines)
        self.assertIn('Pooled media: status **inconclusive**, qualified takeovers unreadable, '
                      'qualified moves unreadable', lines)
        self.assertIn('| rollout | unreadable | 2 | distributed_window | unreadable |', lines)
        self.assertIn('- Caller events with `timingEvidence: false`, release callers (`callerTiming`): '
                      'unreadable', lines)
        self.assertIn('- Caller events with `timingEvidence: false`, head caller (`headCaller.callerTiming`): '
                      'unreadable', lines)
        self.assertIn('Cause: **unreadable**', lines)
        self.assertIn('Selected bounds: frontendControlBoundNs unreadable, frontendWorkerBoundNs missing, '
                      'driftPpm unreadable', lines)
        self.assertPrivate(text)




class RelayHostLossSummary(unittest.TestCase):
    def test_per_call_gaps_and_failed_boundary(self):
        report = {'schemaVersion': 1, 'relayLoss': {'completed': True, 'calls': [
            {'call': 1, 'gapNs': 5_000_000_000, 'recoveryGapNs': 4_000_000_000, 'recovered': True},
            {'call': 2, 'gapNs': 5_000_000_001, 'recovered': True},
            {'call': 3, 'gapNs': 0, 'recovered': False}]}}
        text = '\n'.join(report_lines(report))
        self.assertIn('| 1 | 5000000000 | 4000000000 | observed | pass |', text)
        self.assertIn('| 2 | 5000000001 | missing | observed | fail |', text)
        self.assertIn('| 3 | 0 | missing | unobserved | fail |', text)


class PartRSummary(unittest.TestCase):
    def test_episode_counts_and_specific_restart_cause(self):
        call = {'ordinal': 0, 'role': 'affected', 'gapNs': 2233456140,
                'outcome': 'inconclusive', 'causes': ['relay-loss-ice-restart-missing'],
                'episodeCount': 2, 'iceRestarts': 0}
        report = {'schemaVersion': 1, 'partR': True, 'gates': [{'status': 'inconclusive',
                  'process': {'relayLoss': {'calls': [call]}}}]}
        text = '\n'.join(report_lines(report))
        self.assertIn('| 0 | affected | 2 | 0 | 2233456140 | inconclusive | relay-loss-ice-restart-missing |', text)
        for bad in (-1, 0.5, float('nan'), float('inf'), True, 2**53, 'private-host'):
            call.update(episodeCount=bad, iceRestarts=bad)
            text = '\n'.join(report_lines(report))
            self.assertIn('| 0 | affected | unreadable | unreadable |', text)
            self.assertNotIn('private-host', text)

    def test_verdict_gaps_and_causes(self):
        report = {'schemaVersion': 1, 'partR': True, 'gates': [
            {'status': 'inconclusive', 'procedureCompleted': False, 'process': {'relayLoss': {
                'outcome': 'inconclusive', 'causes': ['relay-loss-missing'], 'calls': [
                    {'ordinal': 0, 'role': 'affected', 'gapNs': 5000000001, 'outcome': 'fail', 'causes': ['relay-loss-slow']},
                    {'ordinal': 1, 'role': 'unaffected', 'gapNs': 1000, 'outcome': 'pass', 'causes': []},
                    {'ordinal': 2, 'role': 'unknown', 'gapNs': 0, 'outcome': 'inconclusive', 'causes': ['relay-loss-missing']},
                    {'ordinal': 3, 'role': 'affected', 'gapNs': 10000000000, 'outcome': 'fail', 'causes': ['relay-loss-unrecovered']}
                ]}}}]}
        text = '\n'.join(report_lines(report))
        self.assertIn('### Part R: relay host loss (5 s budget)', text)
        self.assertIn('Verdict: **inconclusive**', text)
        self.assertIn('| 0 | affected | missing | missing | 5000000001 | fail | relay-loss-slow |', text)
        self.assertIn('| 1 | unaffected | missing | missing | 1000 | pass | none |', text)
        self.assertIn('| 2 | unknown | missing | missing | 0 | inconclusive | relay-loss-missing |', text)
        self.assertIn('| 3 | affected | missing | missing | 10000000000 | fail | relay-loss-unrecovered |', text)
        result = report['gates'][0]['process']['relayLoss']
        result['causes'] = ['relay-host-secret', '10.0.0.1', 'token_abc']
        result['calls'][0].update(role='private-host', outcome='secret', causes=['private-name'])
        text = '\n'.join(report_lines(report))
        for private in ('relay-host-secret', '10.0.0.1', 'token_abc', 'private-host', 'private-name'):
            self.assertNotIn(private, text)
        self.assertIn('unreadable', text)


if __name__ == '__main__':
    unittest.main()
