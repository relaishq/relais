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

    def test_cursorless_tail_records_one_in_run_gap(self):
        self.log.write_text(''.join(self.row(s) for s in range(1200)))
        self.assertGreater(self.log.stat().st_size, clock.READ_LIMIT)
        result = self.read(start=1792886400500)
        self.assertGreater(result['rows'][0]['unixSeconds'], 1792886400)
        self.assertEqual(result['breaks'], [dict(kind='cursor-gap', fromUnixSeconds=1792886400,
                         unixSeconds=result['rows'][0]['unixSeconds'], beforeRun=False)])

    def test_cursorless_tail_before_run_records_no_gap(self):
        self.log.write_text(''.join(self.row(s) for s in range(1200)))
        self.assertGreater(self.log.stat().st_size, clock.READ_LIMIT)
        result = self.read(start=1792887400000)
        self.assertTrue(result['rows'][0]['beforeRun'])
        self.assertFalse(result['rows'][-1]['beforeRun'])
        self.assertEqual(result['breaks'], [])

    def test_cursorless_small_log_records_gap_only_for_in_run_first_row(self):
        self.assertLess(self.log.stat().st_size, clock.READ_LIMIT)
        result = self.read(start=1792886401000)
        self.assertTrue(result['rows'][0]['beforeRun'])
        self.assertEqual(result['breaks'], [])
        result = self.read(start=1792886400500)
        self.assertFalse(result['rows'][0]['beforeRun'])
        self.assertEqual(result['breaks'], [dict(kind='cursor-gap', fromUnixSeconds=1792886400,
                         unixSeconds=1792886400, beforeRun=False)])

    def test_cursorless_empty_read_records_gap_only_after_run_start(self):
        for text, losses in (('', []), ('# banner\n' * clock.READ_LIMIT, []), ('x' * (clock.READ_LIMIT + 100), ['log-loss'])):
            self.log.write_text(text)
            for start in (0, self.wall // 10**6, self.wall // 10**6 + 1):
                self.assertEqual([b['kind'] for b in self.read(start=start)['breaks']], losses)
            start = self.wall // 10**6 - 1500
            result = self.read(start=start)
            self.assertEqual(result['rows'], [])
            self.assertEqual([b['kind'] for b in result['breaks']], ['cursor-gap'] + losses)
            self.assertEqual(result['breaks'][0], dict(kind='cursor-gap', fromUnixSeconds=start // 1000,
                             unixSeconds=self.wall // 10**9, beforeRun=False))

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
        timeline = self.read(first, start=1792886396500)
        sample = dict(errorBoundNs=12, rateErrorPpm=0.5, measuredUnixNs=str(self.wall))
        self.assertGreater(len(clock.base64.b64encode(clock.gzip.compress(clock.json.dumps(dict(sample, trackingLog=timeline)).encode()))), 23998)
        for expected in (1792886400, 1792886397):
            if expected < 1792886400:
                timeline['breaks'].extend(dict(kind='clock-step', fromUnixSeconds=at, unixSeconds=at + 1, beforeRun=history)
                                          for at, history in ((1792886390, True), (1792886398, False), (1792886397, False)))
            with patch.object(clock, 'clock_evidence', return_value=dict(sample)), patch.object(clock, 'tracking_evidence', return_value=clock.json.loads(clock.json.dumps(timeline))):
                result = clock.read_clock(first, 1792886396500)
            self.assertLessEqual(len(clock.base64.b64encode(clock.gzip.compress(clock.json.dumps(result).encode()))), 23998)
            self.assertEqual(result['errorBoundNs'], 12)
            self.assertEqual(result['trackingLog']['rows'], [])
            self.assertEqual(result['trackingLog']['breaks'][0]['fromUnixSeconds'], expected)

    def test_cursorless_wire_limit_preserves_run_start_gap(self):
        import random
        rng = random.Random(1)
        lines = []
        for s in range(1200):
            columns = self.row(s * 16, maximum='9.9e-01').split()
            columns[4:7] = [f'{rng.uniform(-5, 5):.3f}', f'{rng.random():.3f}', f'{rng.uniform(-1e-5, 1e-5):.3e}']
            columns[9:13] = [f'{rng.uniform(1e-7, 1e-3):.3e}' for _ in range(4)]
            lines.append(' '.join(columns) + '\n')
        self.log.write_text(''.join(lines))
        self.wall += 1200 * 16 * 10**9
        start = (1792886400 + 100 * 16) * 1000
        timeline = self.read(start=start)
        self.assertGreater(self.log.stat().st_size, clock.READ_LIMIT)
        self.assertGreater(timeline['rows'][0]['unixSeconds'], start // 1000)
        sample = dict(errorBoundNs=12, rateErrorPpm=0.5, measuredUnixNs=str(self.wall))
        self.assertGreater(len(clock.base64.b64encode(clock.gzip.compress(clock.json.dumps(dict(sample, trackingLog=timeline)).encode()))), 23998)
        with patch.object(clock, 'TRACKING_LOG', self.log), patch.object(clock, 'daemon_identity', return_value=self.daemon), patch.object(clock.time, 'time_ns', return_value=self.wall), patch.object(clock, 'clock_evidence', return_value=sample):
            result = clock.read_clock(run_start_unix_ms=start)
        self.assertEqual(result['trackingLog']['rows'], [])
        self.assertEqual(result['trackingLog']['breaks'], [dict(kind='cursor-gap', fromUnixSeconds=start // 1000,
                         unixSeconds=self.wall // 10**9, beforeRun=False)])
        self.assertLessEqual(len(clock.base64.b64encode(clock.gzip.compress(clock.json.dumps(result).encode()))), 23998)


class ProbeTests(unittest.TestCase):
    def setUp(self):
        import pathlib
        import tempfile
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = pathlib.Path(self.temp.name) / 'probes'
        self.path.write_bytes(b'')
        self.sample = dict(errorBoundNs=12, rateErrorPpm=0.5, measuredUnixNs='1793930000000000000')
        self.timeline = dict(rows=[], breaks=[], cursor=None, runStartUnixMs=0)

    def line(self, n=0, target=0, result=0, stamps=None):
        if stamps is None:
            base = 1793930000000000000 + n * 10**9
            stamps = [base, base+1000, base+2000, base+3000]
        return (str(target) + ' ' + ' '.join(f'{v:019d}' for v in stamps) + ' ' + str(result) + '\n').encode()

    def read(self, cursor=None, path=None):
        import copy
        with patch.object(clock, 'clock_evidence', return_value=copy.deepcopy(self.sample)), patch.object(clock, 'tracking_evidence', return_value=copy.deepcopy(self.timeline)):
            return clock.read_clock(None, 0, str(self.path) if path is None else path, cursor)

    def baseline(self):
        return dict(self.sample, trackingLog=self.timeline)

    def test_probe_line_mirrors_fixed_format_and_partial_tail(self):
        for target in range(3):
            for result in range(3):
                raw = self.line(target=target, result=result)
                self.assertEqual(len(raw), 84)
                self.assertEqual(clock.probe_line(raw), (target, [1793930000000000000, 1793930000000001000, 1793930000000002000, 1793930000000003000], result))
                for n in range(84):
                    self.assertIsNone(clock.probe_line(raw[:n]))
        self.assertEqual(clock.probe_line(self.line(target=3, result=3, stamps=[0]*4)), (3, [0]*4, 3))
        for raw in (self.line(target=3), self.line(result=3), self.line(target=3, result=3), self.line(stamps=[10**19-1]*4), b'short\n', self.line().replace(b' ', b'\t', 1), self.line() + b'x'):
            with self.assertRaises(ValueError):
                clock.probe_line(raw)

    def test_partial_last_line_waits_and_cursor_resumes(self):
        first, second = self.line(0), self.line(1)
        self.path.write_bytes(first + second[:20])
        result = self.read()['probeLog']
        self.assertEqual(result['lines'].encode(), first)
        self.assertEqual(result['cursor']['position'], 84)
        waiting = self.read(result['cursor'])['probeLog']
        self.assertEqual(waiting['lines'], '')
        self.assertEqual(waiting['cursor']['position'], 84)
        with self.path.open('ab') as f:
            f.write(second[20:])
        rest = self.read(waiting['cursor'])['probeLog']
        self.assertEqual(rest['lines'].encode(), second)
        self.assertEqual(rest['cursor']['position'], 168)
        self.assertEqual(sum(rest['lossCounts'].values()), 0)

    def test_capacity_marker_and_malformed_line_are_counted_once(self):
        self.path.write_bytes(b'bad\n' + self.line(target=3, result=3, stamps=[0]*4))
        result = self.read()['probeLog']
        self.assertEqual(result['lines'], '')
        self.assertEqual(result['lossCounts']['capacityEvents'], 1)
        self.assertEqual(result['lossCounts']['malformedLines'], 1)
        self.assertEqual(sum(self.read(result['cursor'])['probeLog']['lossCounts'].values()), 0)

    def test_unreadable_and_bad_cursor_never_change_baseline(self):
        missing = str(self.path.with_suffix('.missing'))
        result = self.read(path=missing)
        self.assertEqual(result['probeLog']['lossCounts']['readFailures'], 1)
        del result['probeLog']
        self.assertEqual(result, self.baseline())
        self.path.write_bytes(self.line())
        first = self.read()['probeLog']
        self.path.write_bytes(self.line(1))
        gap = self.read(first['cursor'])['probeLog']
        self.assertEqual(gap['lossCounts']['cursorGaps'], 1)
        self.assertEqual(gap['lines'], '')
        with self.path.open('ab') as f:
            f.write(self.line(2))
        self.assertEqual(self.read(gap['cursor'])['probeLog']['lines'].encode(), self.line(2))
        self.assertNotIn('probeLog', self.read(path=''))

    def test_combined_wire_pressure_defers_a_prefix_without_loss(self):
        import random
        import string
        rng = random.Random(31)
        self.sample['tracking'] = ''.join(rng.choices(string.ascii_letters + string.digits, k=16000))
        lines = []
        for i in range(900):
            base = 1793930000000000000 + i * 10**9
            a, b, processing = rng.randrange(10**8), rng.randrange(10**8), rng.randrange(10**7)
            lines.append(self.line(i, target=i % 3, stamps=[base, base+a, base+a+processing, base+a+processing+b]))
        self.path.write_bytes(b''.join(lines))
        first = self.read()['probeLog']
        count = len(first['lines']) // 84
        self.assertGreater(count, 0)
        self.assertLess(count, len(lines))
        self.assertEqual(first['cursor']['position'], count * 84)
        self.assertEqual(first['lines'].encode(), b''.join(lines[:count]))
        seen, cursor = first['lines'], first['cursor']
        for _ in range(20):
            result = self.read(cursor)
            encoded = clock.json.dumps(result).encode()
            self.assertLessEqual(len(encoded), 256000)
            self.assertLessEqual(len(clock.base64.b64encode(clock.gzip.compress(encoded))), 23998)
            batch = result.pop('probeLog')
            self.assertEqual(result, self.baseline())
            self.assertEqual(sum(batch['lossCounts'].values()), 0)
            seen += batch['lines']
            cursor = batch['cursor']
            if cursor['position'] == self.path.stat().st_size:
                break
        self.assertEqual(seen.encode(), b''.join(lines))

    def test_full_window_has_bounded_compression_calls(self):
        # Wire pressure forces deferral. Include baseline compression, normal
        # records, and the maximum number of malformed lines in one window.
        import random
        import string
        self.sample['tracking'] = ''.join(random.Random(31).choices(string.ascii_letters + string.digits, k=22000))
        for data in (b''.join(self.line(i) for i in range(781))[:clock.READ_LIMIT], b'\n' * clock.READ_LIMIT):
            self.path.write_bytes(data)
            with patch.object(clock.gzip, 'compress', wraps=clock.gzip.compress) as compress:
                result = self.read()
            self.assertLessEqual(compress.call_count, 15)
            self.assertGreater(result['probeLog']['cursor']['position'], 0)
            result.pop('probeLog')
            self.assertEqual(result, self.baseline())

    def test_fifo_and_symlink_leave_baseline_intact(self):
        import os
        fifo = self.path.with_suffix('.fifo')
        os.mkfifo(fifo)
        link = self.path.with_suffix('.link')
        link.symlink_to(self.path)
        # A subprocess timeout makes a blocking-open regression fail promptly.
        import subprocess
        import sys
        for path in (fifo, link):
            completed = subprocess.run([sys.executable, '-B', '-c',
                "import clock,json,sys; print(json.dumps(clock.add_probe_evidence(json.load(sys.stdin),sys.argv[1],None,0)))", str(path)],
                input=clock.json.dumps(self.baseline()), text=True, capture_output=True, check=True, timeout=2)
            result = clock.json.loads(completed.stdout)
            self.assertEqual(result.pop('probeLog')['lossCounts']['readFailures'], 1)
            self.assertEqual(result, self.baseline())

    def test_unexpected_probe_exception_leaves_baseline_intact(self):
        self.path.write_bytes(self.line())
        with patch.object(clock, 'probe_line', side_effect=RuntimeError('injected')):
            result = self.read()
        batch = result.pop('probeLog')
        self.assertEqual(batch['lossCounts']['readFailures'], 1)
        self.assertIsNone(batch['cursor'])
        self.assertEqual(result, self.baseline())
        # Even an exception in diagnostic packing cannot escape this boundary.
        with patch.object(clock, 'probe_fits', side_effect=RuntimeError('injected')):
            self.assertEqual(self.read(), self.baseline())

    def test_unpack_budget_and_tracking_fallback_are_unchanged(self):
        self.path.write_bytes(self.line())
        # Low-entropy text fits the compressed limit but leaves no unpack room.
        self.sample['tracking'] = 'x' * 255900
        result = self.read()
        self.assertEqual(result, self.baseline())
        # #404's baseline fallback must run before extension budgeting.
        import random
        import string
        rng = random.Random(7)
        self.sample['tracking'] = ''
        self.timeline['rows'] = [dict(reference=''.join(rng.choices(string.ascii_letters, k=500)), unixSeconds=1793930000, beforeRun=False) for _ in range(100)]
        self.timeline['breaks'] = [dict(kind='clock-step', fromUnixSeconds=1793929990, unixSeconds=1793929991, beforeRun=False)]
        with patch.object(clock, 'clock_evidence', return_value=dict(self.sample)), patch.object(clock, 'tracking_evidence', return_value=clock.json.loads(clock.json.dumps(self.timeline))):
            baseline = clock.read_clock()
        result = self.read()
        result.pop('probeLog', None)
        self.assertEqual(result, baseline)
        self.assertEqual(result['trackingLog']['breaks'][0]['fromUnixSeconds'], 1793929990)
