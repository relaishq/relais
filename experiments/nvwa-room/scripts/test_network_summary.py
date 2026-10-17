"""Exercise the evidence reducer against duplicates, echoes and missing cells."""
import gzip
import importlib.util
import json
import pathlib
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location('network_summary', pathlib.Path(__file__).with_name('summarize-network.py'))
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class SummaryTests(unittest.TestCase):
    def fixture(self, root, fault):
        players = []
        for index in range(2):
            update = {'type': 'state-update', 'player_id': f'p{2-index}', 'seq': 2, 'sent_ns': 1_000_000_000}
            later = {**update, 'seq': 4, 'sent_ns': 1_300_000_000}
            players.append({'messages': [
                {'index': 0, 'type': 'trigger', 'bytes': 300, 'rtt_ms': 1, 'schedule_lag_ms': 0},
                {'index': 1, 'type': 'state', 'bytes': 240, 'schedule_lag_ms': 0},
                {'index': 2, 'type': 'stop', 'bytes': 280, 'rtt_ms': 3, 'schedule_lag_ms': 0},
                {'index': 3, 'type': 'state', 'bytes': 240, 'schedule_lag_ms': 0}], 'observations': [
                    {'at_ns': 1_100_000_000, 'reply': update},
                    {'at_ns': 1_200_000_000, 'reply': update},
                    {'at_ns': 1_250_000_000, 'reply': {**update, 'type': 'snapshot'}},
                    {'at_ns': 1_300_000_000, 'reply': {**update, 'type': 'state'}},
                    {'at_ns': 1_400_000_000, 'reply': later},
                    {'at_ns': 3_500_000_000, 'reply': {**later, 'seq': 6}}]})
        run = {'profile': 'lan', 'variant': 'ws', 'fault': fault, 'messages_per_client': 4,
               'StartNS': 1_000_000_000, 'EndNS': 3_000_000_000, 'errors': None,
               'proof': {'recovery_ms': None}, 'room_metrics': {'deduplicated': 1, 'stale_state': 0},
               'players': players}
        folder = root / fault
        folder.mkdir()
        with gzip.open(folder / 'result.json.gz', 'wt') as stream:
            json.dump(run, stream)
        return {'name': fault, 'profile': 'lan', 'variant': 'ws', 'fault': fault, 'trial': 1, 'exit_code': 0}

    def test_unique_peer_updates_exclude_snapshots_echoes_and_fault_window(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            ledger = [self.fixture(root, fault) for fault in ['normal', 'disconnect', 'worker-kill', 'lost-receipt']]
            (root / 'ledger.json').write_text(json.dumps(ledger))
            result = MODULE.summarize(root)['groups'][0]
            self.assertTrue(result['complete'])
            self.assertEqual(result['counts']['states_unique'], 16)
            self.assertEqual(result['counts']['state_duplicates'], 8)
            self.assertEqual(result['state_age_ms']['n'], 16)
            self.assertEqual(result['state_age_ms']['max'], 100)
            self.assertEqual(result['state_gap_ms']['max'], 300)
            self.assertEqual(result['command_rtt_ms']['n'], 16)
            self.assertEqual(result['command_rtt_ms']['p99'], 3)

    def test_missing_or_failed_cells_cannot_be_pass(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            ledger = [self.fixture(root, 'normal')]
            ledger.append({'name': 'startup-failed', 'profile': 'lan', 'variant': 'ws', 'fault': 'disconnect', 'trial': 1, 'exit_code': 1})
            (root / 'ledger.json').write_text(json.dumps(ledger))
            result = MODULE.summarize(root)
            self.assertFalse(result['groups'][0]['complete'])
            self.assertEqual(result['groups'][0]['failed_cells'], ['startup-failed'])
            self.assertIn('PARTIAL/FAIL', MODULE.table(result))


    def test_uploaded_summary_only_merge_is_repeatable(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            first = {'schema': 1, 'groups': [{'profile': 'lan', 'variant': 'ws', 'complete': True}], 'failed_cells': []}
            second = {'schema': 1, 'groups': [{'profile': 'region', 'variant': 'ws', 'complete': True}], 'failed_cells': []}
            for name, document in [('lan', first), ('region', second)]:
                folder = root / name
                folder.mkdir()
                (folder / 'summary.json').write_text(json.dumps(document))
            result = MODULE.merge_summaries(root)
            self.assertEqual(len(result['groups']), 2)
            (root / 'summary.json').write_text(json.dumps(result))
            self.assertEqual(MODULE.merge_summaries(root), result)


if __name__ == '__main__':
    unittest.main()
