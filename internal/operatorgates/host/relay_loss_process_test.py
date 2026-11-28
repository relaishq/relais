"""Offline R-only service restoration transcripts."""
from pathlib import Path
import unittest
from unittest.mock import patch
import preview_process as host
from preview_process_test import Transcript, P
exec(Path(__file__).with_name("relay_loss_process.py").read_text(),host.__dict__)

class RelayLossRestorationTests(unittest.TestCase):
    def test_relay_loss_stop_confirms_normal_stop(self):
        t = Transcript()
        with patch.object(host, "command", t.command), patch.object(host, "http", t.http), patch.object(host.pathlib.Path, "exists", return_value=False):
            self.assertEqual(host.relay_loss_execute("relay-loss-stop", P), {"completed": True})
        self.assertIn(["systemctl", "stop", P["activeUnit"]], t.calls)
        self.assertEqual(t.pids[P["activeUnit"]], "0")

    def test_relay_loss_stop_refuses_changed_active(self):
        t = Transcript()
        def changed(args):
            if args[0] == "ss":
                t.calls.append(args)
                return 'LISTEN 0 128 10.0.0.2:24700 0.0.0.0:* users:(("relay",pid=222,fd=1))'
            return t.command(args)
        with patch.object(host, "command", changed), patch.object(host, "http", t.http):
            with self.assertRaisesRegex(ValueError, "active relay changed"):
                host.relay_loss_execute("relay-loss-stop", P)
        self.assertFalse(any(c[:2] == ["systemctl", "stop"] for c in t.calls))

    def test_relay_loss_stop_accepts_already_exited_unit(self):
        for unit, old_pid, listener in (
            ("relais-relay.service", "111", "222"),
            ("relais-relay.service", "111", None),
            ("relais-relay-standby.service", "222", "111"),
            ("relais-relay-standby.service", "222", None),
        ):
            with self.subTest(unit=unit, listener=listener):
                p = dict(P, activeUnit=unit)
                t = Transcript()
                t.pids[unit] = "0"
                def exited(args):
                    if args[0] == "ss":
                        t.calls.append(args)
                        return "" if listener is None else f'LISTEN 0 128 10.0.0.2:24700 0.0.0.0:* users:(("relay",pid={listener},fd=1))'
                    if args[:5] == ["systemctl", "show", "--value", "-p", "ExecMainPID"]:
                        t.calls.append(args)
                        return old_pid
                    return t.command(args)
                with patch.object(host, "command", exited), patch.object(host, "http", t.http), patch.object(host.pathlib.Path, "exists", autospec=True, return_value=False) as exists:
                    self.assertEqual(host.relay_loss_execute("relay-loss-stop", p), {"completed": True})
                exists.assert_called_once_with(Path(f"/proc/{old_pid}"))
                self.assertFalse(any(c[:2] == ["systemctl", "stop"] for c in t.calls))

    def test_relay_loss_stop_accepts_exit_during_inspection(self):
        for listener in ("222", None):
            with self.subTest(listener=listener):
                t = Transcript()
                def exits(args):
                    if args[0] == "ss":
                        t.calls.append(args)
                        t.pids[P["activeUnit"]] = "0"
                        return "" if listener is None else f'LISTEN 0 128 10.0.0.2:24700 0.0.0.0:* users:(("relay",pid={listener},fd=1))'
                    return t.command(args)
                with patch.object(host, "command", exits), patch.object(host, "http", t.http), patch.object(host.pathlib.Path, "exists", autospec=True, return_value=False) as exists:
                    self.assertEqual(host.relay_loss_execute("relay-loss-stop", P), {"completed": True})
                exists.assert_called_once_with(Path("/proc/111"))
                self.assertFalse(any(c[:2] == ["systemctl", "stop"] for c in t.calls))

    def test_relay_loss_stop_accepts_exit_just_before_stop(self):
        t = Transcript()
        def exits(args):
            if args[:2] == ["systemctl", "stop"]:
                t.pids[P["activeUnit"]] = "0"
            return t.command(args)
        with patch.object(host, "command", exits), patch.object(host.pathlib.Path, "exists", return_value=False):
            self.assertEqual(host.relay_loss_execute("relay-loss-stop", P), {"completed": True})
        self.assertEqual(sum(c[:2] == ["systemctl", "stop"] for c in t.calls), 1)

    def test_relay_loss_stop_refuses_unconfirmed_exit(self):
        for main_pid, old_exists in (("111", False), ("0", True)):
            with self.subTest(main_pid=main_pid, old_exists=old_exists):
                t = Transcript()
                def unconfirmed(args):
                    if args[:2] == ["systemctl", "stop"]:
                        t.calls.append(args)
                        t.pids[P["activeUnit"]] = main_pid
                        return ""
                    return t.command(args)
                with patch.object(host, "command", unconfirmed), patch.object(host, "http", t.http), patch.object(host.pathlib.Path, "exists", return_value=old_exists):
                    with self.assertRaisesRegex(ValueError, "relay predecessor exit unconfirmed"):
                        host.relay_loss_execute("relay-loss-stop", P)

    def test_relay_loss_stop_refuses_unconfirmed_already_exited_pid(self):
        for old_pid, old_exists in (("111", True), ("0", False)):
            with self.subTest(old_pid=old_pid, old_exists=old_exists):
                t = Transcript()
                t.pids[P["activeUnit"]] = "0"
                def unconfirmed(args):
                    if args[:5] == ["systemctl", "show", "--value", "-p", "ExecMainPID"]:
                        t.calls.append(args)
                        return old_pid
                    return t.command(args)
                with patch.object(host, "command", unconfirmed), patch.object(host.pathlib.Path, "exists", return_value=old_exists):
                    with self.assertRaisesRegex(ValueError, "relay predecessor exit unconfirmed"):
                        host.relay_loss_execute("relay-loss-stop", P)
                self.assertFalse(any(c[:2] == ["systemctl", "stop"] for c in t.calls))

    def test_relay_loss_stop_refuses_running_unit_without_listener(self):
        t = Transcript()
        def no_listener(args):
            if args[0] == "ss":
                t.calls.append(args)
                return ""
            return t.command(args)
        with patch.object(host, "command", no_listener), patch.object(host.pathlib.Path, "exists", return_value=False):
            with self.assertRaisesRegex(ValueError, "active relay unit unknown"):
                host.relay_loss_execute("relay-loss-stop", P)
        self.assertFalse(any(c[:2] == ["systemctl", "stop"] for c in t.calls))

    def test_relay_loss_stop_uncertain_stop_is_not_replayed(self):
        t = Transcript()
        def lost(args):
            if args[:2] == ["systemctl", "stop"]:
                t.calls.append(args)
                raise TimeoutError("unknown stop")
            return t.command(args)
        with patch.object(host, "command", lost), patch.object(host, "http", t.http):
            with self.assertRaises(TimeoutError):
                host.relay_loss_execute("relay-loss-stop", P)
        self.assertEqual(sum(c[:2] == ["systemctl", "stop"] for c in t.calls), 1)

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
