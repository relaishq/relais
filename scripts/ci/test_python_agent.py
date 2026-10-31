"""The Python test gate may skip locally, but never silently skip in CI."""
import contextlib
import importlib.util
import io
import os
from pathlib import Path
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location(
    "python_agent_gate", Path(__file__).resolve().parents[1] / "dev/test-python-agent.py")
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


class PythonAgentGateTests(unittest.TestCase):
    def test_missing_python_fails_in_ci(self):
        for value in ("true", ""):
            with self.subTest(value=value), mock.patch.dict(os.environ, {"CI": value}, clear=True), \
                    mock.patch.object(gate, "python312", return_value=None), \
                    contextlib.redirect_stdout(io.StringIO()) as output:
                self.assertEqual(gate.main(), 1)
                self.assertIn("FAIL", output.getvalue())

    def test_missing_python_skips_locally(self):
        with mock.patch.dict(os.environ, {}, clear=True), \
                mock.patch.object(gate, "python312", return_value=None), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(gate.main(), 0)
            self.assertIn("SKIP", output.getvalue())
