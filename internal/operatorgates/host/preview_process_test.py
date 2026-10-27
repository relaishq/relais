"""Offline operator transcripts. Nothing reaches a socket, systemd or AWS."""
import copy
import hashlib
import json
import re
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import preview_process as host
import preview_inventory as inventory

P = dict(control="http://10.0.0.1:24710", relay="http://10.0.0.2:24700", worker="w1",
         workers={"w1": {"http": "http://10.0.0.3:24701", "media": "10.0.0.3:24711"}},
         targetDir="/opt/relais/preview/0.3.0", activeUnit="relais-relay.service")
# v0.3.0 sets the worker's own draining flag only on its SIGTERM path; a control drain marks the registration.
LOCAL = dict(inventory_known=True, truncated=False, sessions=0, inventory=[], draining=False)
CONTROL = dict(ready=True, pool=dict(reservations=0), calls=[],
               workers=[dict(name="w1", dead=True, recovering=False, call_count=0, draining=True)])
STORE = dict(owners=dict(w1=0), transfers=0)


class Transcript:
    def __init__(self):
        self.calls = []
        self.pids = {"relais-relay.service": "111", "relais-relay-standby.service": "222"}
        self.default = "333"

    def command(self, args):
        self.calls.append(args)
        if args[0] == "ss":
            return 'LISTEN 0 128 10.0.0.2:24700 0.0.0.0:* users:(("relay",pid=111,fd=1))'
        if args[0] == "journalctl":
            return "STANDBY_WAIT pid=222 lease=preview"
        if args[0] == "signal":
            self.pids["relais-worker.service"] = "0"
            return ""
        if args[1] in ("stop", "kill"):
            self.pids[args[-1]] = "0"
        if args[1] == "start":
            self.pids[args[-1]] = "222"
        if args[1] == "show":
            return self.pids.get(args[-1], self.default)
        return ""

    def signals(self):
        # Route the pidfd signal through command() so tests observe and fault it like systemctl.
        return (patch.object(host.os, "pidfd_open", lambda pid: pid, create=True),
                patch.object(host.signal, "pidfd_send_signal", lambda fd, sig: self.command(["signal", fd, sig]), create=True),
                patch.object(host.os, "close", lambda fd: None))

    def http(self, url, method="GET"):
        if "/workers/" in url:
            return {"moves": []}
        return copy.deepcopy(CONTROL if url.startswith(P["control"]) else LOCAL)


class ProcessTests(unittest.TestCase):
    def run_step(self, op, transcript=None):
        t = transcript or Transcript()
        opened, sent, closed = t.signals()
        with patch.object(host, "command", lambda args: t.command(args)), patch.object(host, "http", t.http), patch.object(host, "ownership", return_value=STORE), patch.object(host, "configure"), patch.object(host.pathlib.Path, "exists", return_value=False), opened, sent, closed:
            return host.execute(op, P), t

    def test_all_fixed_steps_pass_offline(self):
        for op in ("status", "worker-status", "relay-status", "relay-inspect", "worker-empty", "freeze", "unfreeze", "drain", "worker-stop", "replace", "worker-start", "control-stop", "control-start", "worker-down", "relay-stage", "relay-stop", "relay-replenish"):
            with self.subTest(op=op):
                result, _ = self.run_step(op)
                self.assertIsInstance(result, dict)

    def test_each_independent_empty_dimension_blocks_stop(self):
        variants = [(LOCAL, "sessions", 1), (LOCAL, "inventory", ["one"]), (LOCAL, "inventory_known", False), (LOCAL, "truncated", True), (STORE, "transfers", 1), (STORE["owners"], "w1", 1)]
        for target, key, value in variants:
            with self.subTest(key=key), patch.dict(target, {key: value}):
                with self.assertRaises(ValueError):
                    self.run_step("worker-empty")

    def test_control_drain_mark_is_the_placement_fence(self):
        # A control-driven drain changes the placement registration.
        for op in ("worker-empty", "worker-stop"):
            try:
                self.run_step(op)
            except ValueError as error:
                self.fail(f"{op} refused a worker drained through control: {error}")
        with patch.dict(CONTROL["workers"][0], {"draining": False}), self.assertRaises(ValueError):
            self.run_step("worker-stop")

    def test_worker_empty_reads_only_pure_observations(self):
        # Control /status can reconcile ended calls in v0.3.0; the driver reads it in its own step.
        _, t = self.run_step("worker-empty")
        urls = []
        with patch.object(host, "http", lambda url, method="GET": urls.append((url, method)) or copy.deepcopy(LOCAL)), patch.object(host, "ownership", return_value=STORE):
            host.execute("worker-empty", P)
        self.assertEqual(urls, [(P["workers"]["w1"]["http"] + "/status", "GET")])
        self.assertEqual(t.calls, [])

    def test_read_failures_leave_only_fixed_codes_or_class_names(self):
        variants = [(LOCAL, "sessions", 1, "local-inventory"), (STORE, "transfers", 1, "store-transfer"),
                    (STORE["owners"], "w1", 1, "store-owner")]
        for target, key, value, code in variants:
            with self.subTest(key=key), patch.dict(target, {key: value}):
                with self.assertRaises(ValueError) as raised:
                    self.run_step("worker-empty")
                self.assertEqual(host.read_failure(raised.exception), code)
        self.assertEqual(host.read_failure(ValueError("worker still has authoritative work: 10.0.0.3")), "value-error")
        self.assertEqual(host.read_failure(host.ssl.SSLCertVerificationError("hostname mismatch for x.cache")), "ssl-cert-verification-error")
        self.assertEqual(host.read_failure(KeyError("draining")), "key-error")
        self.assertEqual(len(set(host.READ_FAILURES.values())), len(host.READ_FAILURES))
        source = Path(host.__file__).read_text()
        start, end = source.index("class RedisRead"), source.index("def execute")
        for message in re.findall(r'raise ValueError\("([^"]+)"\)', source[start:end]):
            self.assertIn(message, host.READ_FAILURES)

    def test_store_status_reuses_authoritative_reader_without_mutations(self):
        store = dict(owners=dict(w1=0, w2=1), transfers=2)
        with patch.object(host, "ownership", return_value=store) as read, patch.object(host, "http") as http, patch.object(host, "command") as command:
            self.assertEqual(host.execute("store-status", P), store)
            read.assert_called_once_with(P)
            http.assert_not_called()
            command.assert_not_called()
        with patch.object(host, "ownership", side_effect=TimeoutError("unknown read")):
            with self.assertRaises(TimeoutError):
                host.execute("store-status", P)

    def test_missing_empty_evidence_is_unknown(self):
        t = Transcript()
        t.http = lambda *args: {}
        with self.assertRaises(KeyError):
            self.run_step("worker-empty", t)

    def test_unknown_mutation_never_repeats(self):
        for op in ("freeze", "worker-stop", "worker-start", "control-stop", "control-start", "worker-down", "relay-stage", "relay-stop", "relay-replenish"):
            t = Transcript()
            original = t.command
            mutations = []
            def lost(args):
                if args[:2] in (["systemctl", "stop"], ["systemctl", "start"], ["systemctl", "kill"]) or args[0] == "signal":
                    mutations.append(args)
                    raise TimeoutError("reply lost")
                return original(args)
            t.command = lost
            with self.subTest(op=op), self.assertRaises(TimeoutError):
                self.run_step(op, t)
            self.assertEqual(len(mutations), 1)
        calls = []
        def lost_post(url, method="GET"):
            calls.append((url, method))
            raise TimeoutError("reply lost")
        with patch.object(host, "http", lost_post), self.assertRaises(TimeoutError):
            host.execute("drain", P)
        self.assertEqual(len(calls), 1)

    def test_predecessor_must_exit_and_no_signal_restart_in_control_steps(self):
        for op in ("worker-stop", "control-stop", "worker-down", "relay-stop"):
            t = Transcript()
            opened, sent, closed = t.signals()
            with patch.object(host.time, "sleep"), patch.object(host.pathlib.Path, "exists", return_value=True), patch.object(host, "http", t.http), patch.object(host, "command", t.command), opened, sent, closed:
                with self.subTest(op=op), self.assertRaises(ValueError):
                    host.execute(op, P)
        _, t = self.run_step("control-start")
        self.assertEqual([x for x in t.calls if x[1] == "start"], [["systemctl", "start", "relais-control.service"]])

    def test_takeover_is_sigkill_on_exact_worker_unit(self):
        _, t = self.run_step("worker-down")
        self.assertEqual([x for x in t.calls if x[0] == "signal" or x[1] in ("stop", "kill")], [["signal", 333, host.signal.SIGKILL]])
        self.assertEqual(t.calls[-1], ["systemctl", "show", "--value", "-p", "MainPID", "relais-worker.service"])

    def test_signal_bracket_times_only_the_kill(self):
        # The bracket contains only the signal, without systemctl observation time.
        t, order = Transcript(), []
        base = t.command
        def command(args):
            order.append(args[0])
            return base(args)
        t.command = command
        clock = iter(range(1_000_000_000_000_000_000, 1_000_000_000_000_000_000 + 1000, 100))
        def time_ns():
            order.append("clock")
            return next(clock)
        with patch.object(host.time, "time_ns", time_ns):
            result, _ = self.run_step("worker-down", t)
        start = order.index("clock")
        self.assertEqual(order[start:start + 3], ["clock", "signal", "clock"])
        self.assertEqual(int(result["signalAfterUnixNs"]) - int(result["signalBeforeUnixNs"]), 100)
        changed = Transcript()
        changed.default = "444"
        opened, sent, closed = changed.signals()
        with patch.object(host, "command", changed.command), patch.object(host.os, "pidfd_open", lambda pid: (changed.pids.__setitem__("relais-worker.service", "555"), pid)[1], create=True), sent, closed, self.assertRaises(ValueError):
            host.execute("worker-down", P)
        self.assertNotIn("signal", [x[0] for x in changed.calls])

    def test_unsettled_death_and_missing_standby_refuse(self):
        with patch.dict(CONTROL["workers"][0], recovering=True):
            with self.assertRaises(ValueError):
                self.run_step("replace")
        t = Transcript()
        original = t.command
        t.command = lambda args: "" if args[0] == "journalctl" else original(args)
        with patch.object(host.time, "monotonic", side_effect=[0, 0, 16]), patch.object(host.time, "sleep"), self.assertRaises(ValueError):
            self.run_step("relay-stage", t)


class InventoryTests(unittest.TestCase):
    def test_live_identity_and_manifest_are_required(self):
        with tempfile.TemporaryDirectory(prefix="preview-host-") as folder:
            root = Path(folder)
            installed = root / "opt/relais/bin"
            installed.mkdir(parents=True)
            (root / "etc/relais").mkdir(parents=True)
            (root / "opt/relais/boot").mkdir()
            (root / "proc/42").mkdir(parents=True)
            manifest = dict(version="0.3.0", commit="a" * 40)
            raw = json.dumps(manifest).encode()
            (installed / "compatibility.json").write_bytes(raw)
            (installed / "relais-worker").write_text("fixture")
            (root / "proc/42/exe").symlink_to(installed / "relais-worker")
            (root / "proc/42/cmdline").write_bytes(b"relais-worker\x00-agent\x00echo\x00")
            (root / "opt/relais/boot/run.sh").write_bytes(b"reviewed")
            (root / "etc/relais/node.json").write_text(json.dumps(dict(architecture="arm64", components=["worker"], sha256="b" * 64)))
            pin = dict(version="0.3.0", commit="a" * 40, archiveSha256="b" * 64, manifestSha256=hashlib.sha256(raw).hexdigest(), bootSha256=hashlib.sha256(b"reviewed").hexdigest())
            def run(args):
                if args[0] != "systemctl":
                    return "relais-worker version=0.3.0 commit=" + "a" * 40 + " date=fixture"
                if args[1] == "cat":
                    return "ExecStart=/opt/relais/boot/run.sh worker"
                return "42" if args[-2] == "MainPID" else ""
            result = inventory.collect("w1", [pin], root, run)
            self.assertEqual(result["participants"][0]["name"], "w1")
            def stopped(args):
                return "0" if args[0] == "systemctl" and args[-2] == "MainPID" else run(args)
            inventory.collect("w1", [pin], root, stopped)  # Compatibility reads can observe a deliberate stop.
            with self.assertRaises(ValueError) as refused:
                inventory.collect("w1", [pin], root, stopped, require_running=True)
            # The fixed code survives without forwarding host free text.
            self.assertEqual(inventory.failure_category(refused.exception), "not-running")
            self.assertEqual(inventory.failure_category(FileNotFoundError("/proc/42/exe secret")), "file-not-found-error")
            self.assertEqual(inventory.failure_category(ValueError("free text 10.0.0.4")), "value-error")
            self.assertLess(result["observedAt"], 2**53)
            for path, value in [("proc/42/cmdline", b"-snapshot-budget\x00"), ("opt/relais/boot/run.sh", b"changed"), ("opt/relais/bin/compatibility.json", b"{}")]:
                target = root / path
                original = target.read_bytes()
                target.write_bytes(value)
                with self.subTest(path=path), self.assertRaises((ValueError, KeyError)):
                    inventory.collect("w1", [pin], root, run)
                target.write_bytes(original)



class TrialTests(unittest.TestCase):
    def test_move_checks_real_owner_epoch_then_posts_once(self):
        p = dict(P, worker="w2", session="one", owner="w1", epoch=7)
        log = []
        def http(url, method="GET"):
            log.append((url, method))
            if method == "GET":
                return {"calls": [{"id": "one", "owner": "w1", "lease_epoch": 7}]}
            return {"kind": "move"}
        with patch.object(host, "http", http):
            self.assertEqual(host.execute("move", p), {"kind": "move"})
        self.assertEqual(log[-1], (P["control"] + "/calls/one/move?to=w2", "POST"))
        for change in ({"owner": "w2"}, {"epoch": 8}):
            log.clear()
            with patch.object(host, "http", http), self.assertRaises(ValueError):
                host.execute("move", dict(p, **change))
            self.assertEqual(len(log), 1)

    def test_unknown_move_result_never_retries(self):
        log = []
        def http(url, method="GET"):
            log.append(method)
            if method == "GET":
                return {"calls": [{"id": "one", "owner": "w1", "lease_epoch": 7}]}
            raise TimeoutError("unknown result")
        with patch.object(host, "http", http), self.assertRaises(TimeoutError):
            host.execute("move", dict(P, worker="w2", session="one", owner="w1", epoch=7))
        self.assertEqual(log, ["GET", "POST"])


if __name__ == "__main__":
    unittest.main()
