import json
import pathlib
import tempfile
import types
import unittest


class HistoryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = pathlib.Path(self.temp.name) / 'tracking.log'
        ns = {}
        root = pathlib.Path(__file__).parent
        exec((root/'clock.py').read_text() + '\n' + (root/'clock_history.py').read_text(), ns)
        self.h = types.SimpleNamespace(**ns)
        ns['TRACKING_LOG'], ns['daemon_identity'] = self.path, lambda: 'test-daemon'
        self.read = ns['historical_tracking']
        self.base = 1792886400
        self.path.write_text(''.join(self.row(s) for s in range(6000)))

    def row(self, second, token='1.234e-05'):
        at = self.h.datetime.datetime.fromtimestamp(self.base+second, self.h.datetime.timezone.utc)
        return f'{at:%Y-%m-%d %H:%M:%S} GPS 2 0 0 0 N 1 0 0 0 0 {token}\n'

    def page(self, cursor=None, boundary=None):
        return self.read(cursor, (self.base+400)*1000 if boundary is None else boundary, self.base+100, self.base+300)

    def test_large_history_precision_and_inclusive_boundaries(self):
        # Distinct equal-second rows at both inclusive ends must survive.
        self.path.write_text(''.join(self.row(s)+ (self.row(s, '1.234e-005') if s in (100, 300) else '') for s in range(6000)))
        rows, cursor, pages = [], None, 0
        while True:
            page = self.page(cursor)
            self.assertNotIn('cause', page)
            self.assertLessEqual(len(self.h.base64.b64encode(self.h.gzip.compress(json.dumps(page).encode()))), 23998)
            rows.extend(page['rows']); pages += 1
            if page['readComplete']:
                break
            cursor = page['cursor']
            with self.path.open('a') as f:
                f.write(self.row(6000+pages))
        self.assertGreater(pages, 1)
        self.assertEqual(len(rows), 203)
        self.assertEqual([r['maxErrorDecimal'] for r in rows if r['unixSeconds'] in (self.base+100, self.base+300)], ['1.234e-05', '1.234e-005']*2)
        self.assertTrue(all(r['beforeRun'] for r in rows))
        self.assertEqual(self.page(boundary=(self.base+301)*1000)['runStartUnixMs'], (self.base+301)*1000)
        self.assertEqual(self.page(boundary=(self.base+301)*1000-1)['cause'], 'clock-history-invalid')

    def test_changed_snapshot(self):
        for change in ('rotation', 'truncation', 'rewrite'):
            self.path.write_text(''.join(self.row(s) for s in range(6000)))
            cursor = self.page()['cursor']
            if change == 'rotation':
                self.path.rename(self.path.with_suffix('.old'))
                self.path.write_text(''.join(self.row(s) for s in range(6000)))
            elif change == 'truncation':
                self.path.write_text(self.row(0))
            else:
                self.path.write_text(self.path.read_text().replace('1.234e-05', '1.235e-05'))
            self.assertEqual(self.page(cursor)['cause'], 'clock-history-changed')

    def test_gap_precision_and_incomplete(self):
        for text, cause in [(''.join(self.row(s) for s in range(100, 302) if s < 150 or s > 220), 'gap'),
                            (''.join(self.row(s, '0.00001234' if s == 100 else '1.234e-05') for s in range(100, 302)), 'precision'),
                            (self.row(100)+self.row(300), 'incomplete')]:
            self.path.write_text(text)
            cursor = None
            while True:
                page = self.page(cursor)
                if 'cause' in page:
                    self.assertEqual(page['cause'], 'clock-history-'+cause)
                    break
                self.assertFalse(page['readComplete'])
                cursor = page['cursor']
