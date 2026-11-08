import json
import pathlib
import tempfile
import types
import unittest
from unittest.mock import patch


class HistoryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = pathlib.Path(self.temp.name) / 'tracking.log'
        ns = {}
        root = pathlib.Path(__file__).parent
        exec((root/'clock.py').read_text() + '\n' + (root/'clock_history.py').read_text(), ns)
        self.h = types.SimpleNamespace(**ns)
        self.ns = ns
        ns['TRACKING_LOG'], ns['daemon_identity'] = self.path, lambda: 'test-daemon'
        self.read = ns['historical_tracking']
        self.base = 1792886400
        self.path.write_text(''.join(self.row(s) for s in range(6000)))

    def row(self, second, token='1.234e-05', reference='GPS', leap='N', offset='0'):
        at = self.h.datetime.datetime.fromtimestamp(self.base+second, self.h.datetime.timezone.utc)
        return f'{at:%Y-%m-%d %H:%M:%S} {reference} 2 0 0 {offset} {leap} 1 0 0 0 0 {token}\n'

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
            self.assertEqual(self.page(cursor).get('cause'), 'clock-history-changed')

    def collect(self):
        pages, cursor = [], None
        while True:
            page = self.page(cursor)
            self.assertNotIn('cause', page)
            pages.append(page)
            if page['readComplete']:
                return pages
            cursor = page['cursor']

    def test_reports_gap_and_precision_for_go(self):
        self.path.write_text(''.join(self.row(s) for s in range(100, 302) if s < 150 or s > 220))
        self.assertTrue(any(p['breaks'] for p in self.collect()))
        self.path.write_text(''.join(self.row(s, '0.00001234' if s == 100 else '1.234e-05') for s in range(100, 302)))
        self.assertEqual(self.collect()[0]['rows'][0]['maxErrorDecimal'], '0.00001234')
        self.path.write_text(self.row(100)+self.row(300))
        self.assertEqual(self.page().get('cause'), 'clock-history-incomplete')

    def test_post_interval_events_and_old_malformed_rows(self):
        prefix = 'undated old garbage\n' + self.row(0).replace('GPS 2', 'GPS broken')
        body = ''.join(self.row(s) for s in range(100, 301))
        for tail in (self.row(301, reference='OTHER'), self.row(301)+self.row(305, leap='+'),
                     self.row(301)+self.row(400), self.row(301, offset='1')):
            self.path.write_text(prefix+body+tail)
            pages = self.collect()
            self.assertEqual(sum(len(p['rows']) for p in pages), 201)
            self.assertFalse(any(p['breaks'] for p in pages))

    def test_last_second_spans_page_edge(self):
        self.path.write_text(''.join(self.row(s) for s in range(100, 301)) + self.row(300, '1.234e-005')*200 + self.row(301))
        pages = self.collect()
        self.assertGreater(sum(any(r['unixSeconds'] == self.base+300 for r in p['rows']) for p in pages), 1)
        self.assertEqual(sum(len(p['rows']) for p in pages), 401)

    def test_change_during_page(self):
        original = self.ns['tracking_evidence']
        def changing(*args, **kwargs):
            page = original(*args, **kwargs)
            self.path.write_text(self.path.read_text().replace('1.234e-05', '1.235e-05'))
            return page
        with patch.dict(self.ns, tracking_evidence=changing):
            self.assertEqual(self.page().get('cause'), 'clock-history-changed')

    def test_byte_position_stall(self):
        def stalled(cursor, boundary, **kwargs):
            return dict(rows=[], breaks=[], runStartUnixMs=boundary, cursor=dict(cursor, wallNs=cursor['wallNs']+1))
        with patch.dict(self.ns, tracking_evidence=stalled):
            self.assertEqual(self.page().get('cause'), 'clock-history-incomplete')

    def test_unparseable_next_line_is_incomplete(self):
        first = self.row(100)
        # Put a malformed timestamp exactly at the next page's byte offset.
        padding = '#' * (4096 - len(first) - 1) + '\n'
        self.path.write_text(first + padding + 'unparseable\n' + ''.join(self.row(s) for s in range(101, 302)))
        page = self.page()
        self.assertEqual(page['cursor']['tracking']['position'], 4096)
        self.assertEqual(len(page['rows']), 1)
        self.assertFalse(page['readComplete'])
        self.assertTrue(any(b['kind'] == 'log-loss' for b in self.page(page['cursor'])['breaks']))
