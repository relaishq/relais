"""Both relay slots must read their own private metrics endpoint."""
from pathlib import Path
import types
import unittest
from unittest.mock import MagicMock, patch
import preview_process as host

exec((Path(__file__).parent / 'preview_telemetry.py').read_text(), host.__dict__)


class RelayTelemetryTests(unittest.TestCase):
    def test_second_relay_endpoint(self):
        p = dict(relay='http://192.0.2.1:24700', relayTwo='http://192.0.2.2:24700')
        response = MagicMock()
        response.__enter__.return_value.read.return_value = b'relais_relay_packets 7\n'
        for slot, key in (('relay', 'relay'), ('relay-two', 'relayTwo')):
            opener = MagicMock()
            opener.open.return_value = response
            with patch.object(host.pathlib, 'Path', return_value=types.SimpleNamespace(iterdir=lambda: [])), patch.object(host.urllib.request, 'build_opener', return_value=opener):
                result = host.telemetry(dict(p, slot=slot))
            opener.open.assert_called_once_with(p[key] + '/metrics', timeout=5)
            self.assertEqual(result['metrics'], ['relais_relay_packets 7'])
