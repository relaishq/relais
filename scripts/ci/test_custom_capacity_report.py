"""Approval scope regressions; run with python3 -m unittest discover -s scripts/ci."""
import copy
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(os.environ.get('CAPACITY_REPORT_SCRIPT', Path(__file__).with_name('custom-capacity-report.py')))


def fixture():
    def step(kind, callers, status='pass'):
        return dict(kind=kind, service_outcome=status, causes=['generator saturation'] if status != 'pass' else [],
                    custom_agent=dict(evidence_verdict=status, save_cpu_seconds_per_event=1e-6,
                                      save_wall_seconds_per_event=2e-6, ForcedAttempts=0, ForcedSuccesses=0, ForcedFailures=0),
                    run=dict(callers=callers, telemetry=dict(processes=[])),
                    store_commands_per_call_second=70, store_bytes_per_call_second=99000,
                    store_bytes_by_client_per_call_second={})
    return dict(outcome='pass', planned_load_validated=True, worker_capacity_lower_bound=8,
                relay_capacity_lower_bound=8, candidate_n_plus_one_callers=8,
                custom_agent=dict(worker_sha256='exact-build', agent='delayed-loopback/v1'),
                store_commands_budget_per_call_second=100, store_bytes_budget_per_call_second=131072,
                steps=[step('worker', 8), step('worker', 16, 'inconclusive'), step('relay', 8),
                       step('relay', 16, 'inconclusive'), step('n-plus-one', 8), step('drain', 8)])


class ApprovalScope(unittest.TestCase):
    def run_report(self, data):
        with tempfile.TemporaryDirectory() as tmp:
            report = Path(tmp) / 'capacity.json'
            report.write_text(json.dumps(data))
            result = subprocess.run(['python3', str(SCRIPT), str(report), '--save-cpu-us', '10', '--save-wall-us', '20'],
                                    capture_output=True, text=True, check=False)
            return result, json.loads(report.with_suffix('.approval.json').read_text())

    def test_stopping_step_is_raw_data(self):
        result, report = self.run_report(fixture())
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertEqual('inconclusive', report['steps'][1]['service'])
        self.assertEqual('raw-beyond-validated-bound', report['steps'][1]['approval_scope'])
        self.assertIn('ReadOnly', report['steps'][0]['forced_writes']['interpretation'])

    def test_forced_writes_are_explicit_for_readonly_workload(self):
        data = fixture()
        data['steps'] = [s for s in data['steps'] if s['service_outcome'] == 'pass']
        result, report = self.run_report(data)
        self.assertEqual(0, result.returncode)
        self.assertEqual(0, report['steps'][0]['forced_writes']['attempts'])
        self.assertIn('ReadOnly', report['steps'][0]['forced_writes']['interpretation'])

    def test_required_steps_still_block(self):
        for index in (0, 2, 4, 5):
            for failure in ('service', 'cpu', 'evidence'):
                with self.subTest(index=index, failure=failure):
                    data = copy.deepcopy(fixture())
                    if failure == 'service':
                        data['steps'][index]['service_outcome'] = 'fail'
                    elif failure == 'cpu':
                        data['steps'][index]['custom_agent']['save_cpu_seconds_per_event'] = 1
                    else:
                        data['steps'][index]['custom_agent']['evidence_verdict'] = 'blocking-approval'
                    result, _ = self.run_report(data)
                    self.assertEqual(1, result.returncode)

    def test_missing_required_proof_blocks(self):
        for kind in ('worker', 'relay', 'n-plus-one', 'drain'):
            with self.subTest(kind=kind):
                data = fixture()
                data['steps'] = [s for s in data['steps'] if s['kind'] != kind]
                result, _ = self.run_report(data)
                self.assertEqual(1, result.returncode)
