"""Standalone clock helper contract with recorded chrony data."""
import types
import unittest
from unittest.mock import patch
import clock


class ClockTests(unittest.TestCase):
    def test_chrony_error_envelope_and_unsynchronised_refusal(self):
        tracking = "Leap status : Normal\nSystem time : -0.000001\nRoot dispersion : 0.000002\nRoot delay : 0.000004\nResidual freq : -0.2\nSkew : 0.3\n"
        with patch.object(clock.subprocess, "run", return_value=types.SimpleNamespace(stdout=tracking)), patch.object(clock.time, "time_ns", return_value=1792886400000000000):
            row = clock.clock_evidence()
            self.assertEqual(row["errorBoundNs"], 5000)
            self.assertEqual(row["rateErrorPpm"], 0.5)
            self.assertEqual(row["measuredUnixNs"], "1792886400000000000")
        for bad in (tracking.replace("Normal", "Not synchronised"), tracking.replace("0.3", "nan"), tracking.replace("Root dispersion", "Missing")):
            with patch.object(clock.subprocess, "run", return_value=types.SimpleNamespace(stdout=bad)), self.assertRaises(ValueError):
                clock.clock_evidence()


class TrackingTests(unittest.TestCase):
    def setUp(self):
        import pathlib
        import tempfile
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.log = pathlib.Path(self.temp.name) / 'tracking.log'
        self.log.write_text(self.row(0))
        self.wall, self.mono = 1792886400000000000, 1000000000
        self.daemon = 'daemon-one'

    def row(self, second, reference='203.0.113.15', leap='N', offset='1e-6', maximum='9e-6'):
        at = clock.datetime.datetime.fromtimestamp(1792886400 + second, clock.datetime.timezone.utc).strftime('%Y-%m-%d %H:%M:%S')
        return f'{at} {reference:<15} 2 -3.541 0.075 {offset} {leap} 2 2.940e-07 -2.084e-06 1.534e-06 3.472e-06 {maximum}\n'

    def read(self, cursor=None, start=0):
        with patch.object(clock, 'TRACKING_LOG', self.log), patch.object(clock, 'daemon_identity', return_value=self.daemon), patch.object(clock.time, 'time_ns', return_value=self.wall), patch.object(clock.time, 'monotonic_ns', return_value=self.mono):
            return clock.tracking_evidence(cursor, start)

    def append(self, line, seconds=1):
        with self.log.open('a') as f:
            f.write(line)
        self.wall += seconds * 1000000000
        self.mono += seconds * 1000000000

    def test_clean_real_layout_with_repeated_banners(self):
        from pathlib import Path
        self.log.write_bytes((Path(__file__).parent / 'testdata/tracking.log').read_bytes())
        result = self.read()
        self.assertEqual(result['breaks'], [])
        self.assertEqual(len(result['rows']), 2)
        self.assertEqual(result['cursor']['position'], self.log.stat().st_size)
        self.assertEqual(result['rows'][0], dict(unixSeconds=1503408156, reference='203.0.113.15', stratum=2,
                         frequencyPpm=-3.541, skewPpm=0.075, offsetSeconds=-8.621e-6, leapStatus='N',
                         combinedSources=2, offsetSdSeconds=2.940e-7, remainingCorrectionSeconds=-2.084e-4,
                         rootDelaySeconds=1.534e-2, rootDispersionSeconds=3.472e-3, maxErrorSeconds=4.244e-2, beforeRun=False))
        self.assertEqual(self.read(result['cursor'])['rows'], [])
        for reference in ('203.0.113.15', '2001:db8::15', 'GPS0'):
            self.assertEqual(clock.tracking_row(self.row(0, reference=reference))['reference'], reference)

    def test_recorded_break_fixtures(self):
        for kind in ('cursor-gap', 'log-rotation', 'log-loss', 'clock-step', 'daemon-restart', 'source-change', 'leap-status', 'self-consistency'):
            with self.subTest(kind=kind):
                self.log.write_text(self.row(0))
                first = self.read()['cursor']
                if kind == 'log-rotation':
                    self.log.rename(self.log.with_suffix('.old'))
                    self.log.write_text('')
                if kind == 'daemon-restart':
                    self.daemon += '-new'
                second = 121 if kind == 'cursor-gap' else 1
                self.append(self.row(second, reference='GPS0' if kind == 'source-change' else '203.0.113.15',
                                     leap='+' if kind == 'leap-status' else 'N', offset='1e-5' if kind == 'self-consistency' else '1e-6'), seconds=second)
                if kind == 'log-loss':
                    self.log.write_text('')
                if kind == 'clock-step':
                    self.wall += 1000000000
                self.assertEqual([b['kind'] for b in self.read(first)['breaks']], [kind])

    def test_self_consistency_uses_current_column_14_without_aging(self):
        for offset, expected in (('9e-6', []), ('9.0001e-6', ['self-consistency']), ('-1e-5', ['self-consistency'])):
            self.log.write_text(self.row(0, offset=offset))
            self.assertEqual([b['kind'] for b in self.read()['breaks']], expected)

    def test_slow_reads_keep_complete_rows_without_backlog(self):
        first = self.read()['cursor']
        for n in range(3):
            self.append(''.join(self.row(s) for s in range(n * 180 + 1, (n + 1) * 180 + 1)), seconds=180)
            result = self.read(first)
            self.assertEqual(result['breaks'], [])
            self.assertEqual(len(result['rows']), 180)
            self.assertGreater(result['cursor']['position'] - first['position'], 16000)
            self.assertEqual(result['cursor']['position'], self.log.stat().st_size)
            first = result['cursor']

    def test_capped_reads_resume_without_breaks_and_bound_encoded_output(self):
        import json
        first = self.read()['cursor']
        self.append(''.join(self.row(s) for s in range(1, 1200)), seconds=1200)
        result = self.read(first)
        self.assertEqual(result['breaks'], [])
        self.assertLess(result['cursor']['position'], self.log.stat().st_size)
        rest = self.read(result['cursor'])
        self.assertEqual(rest['breaks'], [])
        self.assertEqual(len(result['rows']) + len(rest['rows']), 1199)
        self.assertEqual(rest['rows'][0]['unixSeconds'], result['rows'][-1]['unixSeconds'] + 1)
        # Dense hostile rows can expand much more than a real chrony line.
        self.log.write_text('2026-10-25 00:00:00 GPS 2 0 0 1 + 0 0 0 0 0 0\n' * 1400)
        result = self.read()
        self.assertLess(len(json.dumps(result).encode()), 240000)
        self.assertLess(result['cursor']['position'], self.log.stat().st_size)

    def test_oversized_line_without_newline_advances_once(self):
        first = self.read()['cursor']
        self.append(self.row(1) + 'x' * (clock.READ_LIMIT + 100))
        result = self.read(first)
        self.assertEqual([b['kind'] for b in result['breaks']], ['log-loss'])
        self.assertEqual(result['cursor']['position'], self.log.stat().st_size)
        self.assertEqual(self.read(result['cursor'])['breaks'], [])
        self.append('more partial text')
        again = self.read(result['cursor'])
        self.assertEqual(again['breaks'], [])
        self.append('\n' + self.row(2))
        final = self.read(again['cursor'])
        self.assertEqual(final['breaks'], [])
        self.assertEqual(len(final['rows']), 1)
        self.log.write_text('x' * (clock.READ_LIMIT + 100))
        self.assertEqual([b['kind'] for b in self.read()['breaks']], ['log-loss'])

    def test_history_rows_and_breaks_are_marked_from_run_start(self):
        self.log.write_text(self.row(0, leap='+') + self.row(1, offset='1e-5') + self.row(2, reference='GPS0'))
        result = self.read(start=1792886402000)
        self.assertEqual(result['runStartUnixMs'], 1792886402000)
        self.assertEqual([r['beforeRun'] for r in result['rows']], [True, True, False])
        self.assertEqual([b['beforeRun'] for b in result['breaks']], [True, True, False])
        result = self.read(start=1792886400500)
        self.assertFalse(result['rows'][0]['beforeRun'])  # Straddling second is in-run.

    def test_missing_malformed_partial_and_rewritten_log(self):
        first = self.read()['cursor']
        self.log.write_text(self.row(1))
        self.assertEqual(self.read(first)['breaks'][0]['kind'], 'cursor-gap')
        for bad in ('bad row\n', self.row(0).replace(' 2 ', ' 999999999999999999999999 ', 1), '\xff\n'):
            self.log.write_bytes(bad.encode('latin1'))
            self.assertEqual(self.read()['breaks'][0]['kind'], 'log-loss')
        self.log.unlink()
        self.assertEqual(self.read()['breaks'][0]['kind'], 'log-loss')
        self.log.write_text(self.row(0)[:-1])
        result = self.read()
        self.assertEqual((result['rows'], result['breaks'], result['cursor']['position']), ([], [], 0))
        for cursor in ({'position': 1}, 'bad-cursor', {'lastRow': {}}):
            self.assertEqual(self.read(cursor)['breaks'][0]['kind'], 'log-loss')

    def test_row_gap_backward_step_leaps_and_loss_interval(self):
        self.log.write_text(self.row(2) + self.row(1))
        self.assertEqual([b['kind'] for b in self.read()['breaks']], ['clock-step'])
        for leap in ('+', '-', '?'):
            self.log.write_text(self.row(0, leap=leap))
            self.assertEqual([b['kind'] for b in self.read()['breaks']], ['leap-status'])
        self.log.write_text(self.row(0) + 'bad row\n' + self.row(1))
        self.assertEqual(self.read()['breaks'][0]['fromUnixSeconds'], 1792886400)
        first = self.read()['cursor']
        self.wall += 30000000000
        self.mono += 30000000000
        self.log.unlink()
        self.assertEqual(self.read(first)['breaks'][0]['fromUnixSeconds'], 1792886401)

    def test_read_clock_keeps_existing_measurement(self):
        with patch.object(clock, 'clock_evidence', return_value={'errorBoundNs': 12}), patch.object(clock, 'tracking_evidence', return_value={'breaks': [{'kind': 'log-loss'}]}) as read:
            result = clock.read_clock({'position': 3}, 1234)
        read.assert_called_once_with({'position': 3}, 1234)
        self.assertEqual(result['errorBoundNs'], 12)
        self.assertEqual(result['trackingLog']['breaks'][0]['kind'], 'log-loss')

    def test_wire_limit_records_gap_without_changing_clock_measurement(self):
        import random
        import string
        rng = random.Random(7)
        first = self.read()['cursor']
        self.append(''.join(self.row(s, reference=''.join(rng.choices(string.ascii_letters, k=220))) for s in range(1, 161)), seconds=160)
        timeline = self.read(first)
        sample = dict(errorBoundNs=12, rateErrorPpm=0.5, measuredUnixNs=str(self.wall))
        self.assertGreater(len(clock.base64.b64encode(clock.gzip.compress(clock.json.dumps(dict(sample, trackingLog=timeline)).encode()))), 23998)
        with patch.object(clock, 'clock_evidence', return_value=sample), patch.object(clock, 'tracking_evidence', return_value=timeline):
            result = clock.read_clock(first)
        self.assertLessEqual(len(clock.base64.b64encode(clock.gzip.compress(clock.json.dumps(result).encode()))), 23998)
        self.assertEqual(result['errorBoundNs'], 12)
        self.assertEqual(result['trackingLog']['rows'], [])
        self.assertEqual(result['trackingLog']['breaks'][0]['fromUnixSeconds'], 1792886400)
