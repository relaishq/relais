import ast
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock

import private_evidence as evidence


class PrivateEvidenceTest(unittest.TestCase):
    def test_private_errors_survive_caller_failure(self):
        with tempfile.TemporaryDirectory() as root:
            logs = Path(root)
            status = {'workers': [{'dead': True}], 'takeovers': [{'error': 'no target'}]}
            caller = Mock()
            caller.wait.return_value = 1
            request = Mock(return_value=status)
            self.assertEqual(1, evidence.wait_for_caller(caller, request, logs,
                             'http://127.0.0.1:1', 70, lambda: 0, subprocess.TimeoutExpired))
            self.assertEqual(status, json.loads((logs / 'control-private-00.json').read_text()))
            self.assertEqual(status['takeovers'], json.loads((logs / 'takeovers-private-00.json').read_text()))
            request.assert_called_once_with('http://127.0.0.1:1/status')

    def test_capture_failures_do_not_mask_original_error(self):
        with tempfile.TemporaryDirectory() as root:
            logs = Path(root)
            evidence.capture_private_status(lambda _: {'takeovers': [{'error': 'older sample'}]},
                                            logs, 'http://127.0.0.1:1', 'final')
            request = Mock(side_effect=OSError('secret response must not leak'))
            evidence.capture_private_status(request, logs, 'http://127.0.0.1:1', 'final')
            self.assertEqual({'capture_error': 'OSError'}, json.loads((logs / 'control-private-final.json').read_text()))
            self.assertEqual({'capture_error': 'OSError'}, json.loads((logs / 'takeovers-private-final.json').read_text()))
            evidence.capture_private_status(request, logs / 'missing', 'http://127.0.0.1:1', 'final')

    def test_periodic_evidence_is_bounded(self):
        with tempfile.TemporaryDirectory() as root:
            logs = Path(root)
            caller = Mock()
            caller.wait.side_effect = [subprocess.TimeoutExpired('caller', 1)] * 40 + [0]
            request = Mock(return_value={'takeovers': []})
            self.assertEqual(0, evidence.wait_for_caller(caller, request, logs,
                             'http://127.0.0.1:1', 70, lambda: 0, subprocess.TimeoutExpired))
            self.assertEqual(2 * evidence.SAMPLE_SLOTS, len(list(logs.iterdir())))
            evidence.capture_private_status(lambda _: {'large': 'x' * evidence.MAX_SAMPLE_BYTES},
                                            logs, 'http://127.0.0.1:1', 'large')
            self.assertEqual({'capture_error': 'ValueError'}, json.loads((logs / 'control-private-large.json').read_text()))

    def test_final_capture_precedes_teardown(self):
        driver = Path(__file__).with_name('run.sh').read_text().split("python3 - <<'PY'\n", 1)[1].rsplit('\nPY', 1)[0]
        tree = ast.parse(driver)
        teardown = [node for node in tree.body if isinstance(node, ast.Try)][0].finalbody
        calls = [node.value.func.id for node in teardown if isinstance(node, ast.Expr)
                 and isinstance(node.value, ast.Call) and isinstance(node.value.func, ast.Name)]
        self.assertEqual('capture_private_status', calls[0])
        self.assertEqual('stop_owned', calls[1])


if __name__ == '__main__':
    unittest.main()
