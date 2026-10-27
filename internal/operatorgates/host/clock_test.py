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
