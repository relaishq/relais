"""Recorded host metrics and ethtool transcripts; no sockets or host commands."""
from pathlib import Path
import tempfile
import types
import unittest
from unittest.mock import patch
import preview_process as host

exec((Path(__file__).parent / "preview_telemetry.py").read_text(), host.__dict__)


class TelemetryTests(unittest.TestCase):
    def test_raw_buckets_and_ena_with_missing_evidence(self):
        with tempfile.TemporaryDirectory(prefix="preview-telemetry-") as folder:
            root = Path(folder)
            stats = root / "eth0" / "statistics"
            stats.mkdir(parents=True)
            for key in ("rx_bytes", "tx_bytes"):
                (stats / key).write_text("123")
            response = unittest.mock.MagicMock()
            response.__enter__.return_value.read.return_value = b'# HELP ignored\nrelais_redis_op_seconds_bucket{le="0.1",op="set"} 42\nrelais_redis_op_seconds_bucket{le="+Inf",op="set"} 43\n'
            opener = types.SimpleNamespace(open=lambda *a, **kw: response)
            paths = lambda p: root if p == "/sys/class/net" else Path(p)
            p = dict(slot="w1", workers={"w1": dict(http="http://10.0.0.1:24701")})
            with patch.object(host, "http", return_value={"sessions": 0, "inventory_known": True, "truncated": False, "inventory": []}), patch.object(host.pathlib, "Path", paths), patch.object(host.urllib.request, "build_opener", return_value=opener), patch.object(host, "command", return_value="bw_in_allowance_exceeded: 2\npps_allowance_exceeded: 3") as command:
                result = host.execute("telemetry", p)
            self.assertEqual(len(result["metrics"]), 2)
            self.assertIn('le="+Inf"', result["metrics"][1])
            self.assertEqual(result["ena"]["eth0"]["pps_allowance_exceeded"], 3)
            self.assertIsNone(result["ena"]["eth0"]["bw_out_allowance_exceeded"])
            command.assert_called_once_with(["ethtool", "-S", "eth0"])
            with patch.object(host, "http", return_value={"sessions": 0, "inventory_known": True, "truncated": False, "inventory": []}), patch.object(host.pathlib, "Path", paths), patch.object(host.urllib.request, "build_opener", side_effect=OSError), patch.object(host, "command", side_effect=OSError):
                missing = host.execute("telemetry", p)
            self.assertIsNone(missing["metrics"])
            self.assertIsNone(missing["ena"]["eth0"])


if __name__ == "__main__":
    unittest.main()
