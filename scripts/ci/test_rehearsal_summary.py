"""The run-page summary shows each listed report field and nothing else."""
import copy
import json
from pathlib import Path
import tempfile
import unittest

from rehearsal_summary import summary

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


if __name__ == '__main__':
    unittest.main()
