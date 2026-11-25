"""Offline R-only service restoration transcripts."""
from pathlib import Path
import unittest
from unittest.mock import patch
import preview_process as host
from preview_process_test import Transcript, P
exec(Path(__file__).with_name("relay_loss_process.py").read_text(),host.__dict__)

class RelayLossRestorationTests(unittest.TestCase):
    def test_single_relay_restart_confirms_exit_before_start(self):
        t = Transcript()
        p = dict(P, slot="relay-two")
        with patch.object(host, "command", t.command), patch.object(host, "configure") as configure, patch.object(host.pathlib.Path, "exists", return_value=False):
            self.assertEqual(host.relay_loss_execute("relay-start", p), {"completed": True})
        stop = t.calls.index(["systemctl", "stop", "relais-relay.service"])
        start = t.calls.index(["systemctl", "start", "relais-relay.service"])
        self.assertLess(stop, start)
        self.assertIn(["systemctl", "show", "--value", "-p", "MainPID", "relais-relay.service"], t.calls[stop+1:start])
        configure.assert_called_once_with("relais-relay.service", "relay", P["targetDir"])
        self.assertNotIn(["systemctl", "start", "relais-relay-standby.service"], t.calls)

    def test_unconfirmed_predecessor_never_starts(self):
        t = Transcript()
        with patch.object(host, "command", t.command), patch.object(host, "configure") as configure, patch.object(host.pathlib.Path, "exists", return_value=True):
            with self.assertRaisesRegex(ValueError, "predecessor exit unconfirmed"):
                host.relay_loss_execute("relay-start", dict(P, slot="relay-two"))
        configure.assert_not_called()
        self.assertFalse(any(c[:2] == ["systemctl", "start"] for c in t.calls))

    def test_lost_stop_is_not_replayed(self):
        t = Transcript()
        def lost(args):
            if args[:2] == ["systemctl", "stop"]:
                t.calls.append(args)
                raise TimeoutError("unknown stop")
            return t.command(args)
        with patch.object(host, "command", lost), patch.object(host, "configure") as configure:
            with self.assertRaises(TimeoutError):
                host.relay_loss_execute("relay-start", dict(P, slot="relay-two"))
        self.assertEqual(sum(c[:2] == ["systemctl", "stop"] for c in t.calls), 1)
        configure.assert_not_called()

    def test_primary_cannot_start_second_nonstandby(self):
        with patch.object(host, "command") as command:
            with self.assertRaisesRegex(ValueError, "requires relay-two"):
                host.relay_loss_execute("relay-start", dict(P, slot="relay"))
        command.assert_not_called()
