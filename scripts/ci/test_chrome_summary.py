import unittest

from chrome_summary import summary


class ChromeSummaryTests(unittest.TestCase):
    def test_shows_both_phases_and_tracks_without_inventing_a_verdict(self):
        log = '\n'.join(
            f'chrome_test.go:549: CHROME_GATE row={phase} track={track} '
            f'n={count} p50_ms=20.0 p95_ms=30.5 max_ms=40.0'
            for phase, count in [('1-planned', 10), ('2-SIGKILL-local', 8)]
            for track in ['audio', 'video'])
        result = summary(log, 'Chrome 123\nRunner image: sample')
        self.assertIn('| 1-planned | audio | 10 | 20.0 | 30.5 | 40.0 |', result)
        self.assertIn('| 2-SIGKILL-local | video | 8 | 20.0 | 30.5 | 40.0 |', result)
        self.assertIn('Chrome 123', result)
        self.assertNotIn('PARTIAL:', result)
        self.assertIn('rows alone do not prove a pass', result)

    def test_partial_or_malformed_logs_remain_explicit(self):
        for log in ['', 'FAIL', 'CHROME_GATE row=1-planned track=audio n=10 p50_ms=NaN']:
            self.assertIn('PARTIAL:', summary(log, 'NOT-RUN: log missing'))

    def test_does_not_claim_unexposed_decryption_counters(self):
        result = summary('NOT-RUN row=7 Chrome inbound-rtp decrypt counter not exposed', '')
        self.assertIn('NOT-RUN: Chrome does not expose caller decryption counters.', result)
