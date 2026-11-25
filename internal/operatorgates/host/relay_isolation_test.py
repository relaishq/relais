"""Offline firewall boundary and independent expiry tests."""
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import relay_isolation as isolation
import preview_process as host


class IsolationTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        lock = patch.object(isolation, "ISOLATION_LOCK", str(Path(tmp.name) / "lock"))
        lock.start()
        self.addCleanup(lock.stop)
        self.calls = []
        self.present = False

    def command(self, args):
        self.calls.append(args)
        if args[1:4] == ["-j", "list", "tables"]:
            return json.dumps({"nftables": [{"table": {"family": "inet", "name": isolation.ISOLATION_TABLE}}] if self.present else []})
        if args[1] == "delete":
            self.present = False
        return ""

    def test_rules_keep_management_clock_and_resolver_directions(self):
        rules = isolation.isolation_rules(120, "abc")
        self.assertIn('create table inet relais_gate_isolation', rules)
        self.assertEqual(rules.count('timeout 120s'), 2)
        self.assertIn('ipv4 timeout', rules)
        self.assertIn('ipv6 timeout', rules)
        self.assertEqual(rules.count('meta nfproto @isolated drop'), 2)
        self.assertIn('iifname "lo" accept', rules)
        self.assertIn('oifname "lo" accept', rules)
        self.assertIn('tcp sport 443 ct direction reply ct state established accept', rules)
        self.assertIn('tcp dport 443 ct direction original accept', rules)
        self.assertNotIn('ct state established,related accept', rules)
        for direction, field in (('input', 'saddr'), ('output', 'daddr')):
            self.assertIn('ip ' + field + ' 169.254.0.0/16 accept', rules)
            self.assertIn('ip6 ' + field + ' fe80::/10 accept', rules)
        self.assertEqual(rules.count('icmpv6 type { 133, 134, 135, 136 } accept'), 2)
        for protocol in ('udp', 'tcp'):
            self.assertIn(protocol + ' sport 53 ct direction reply ct state established accept', rules)
            self.assertIn(protocol + ' dport 53 ct direction original accept', rules)


    def test_dispatch_and_timer_precede_atomic_firewall_apply(self):
        def apply(args, **kw):
            self.assertEqual(self.calls[-1][0], 'systemd-run')
            self.assertEqual(args, ['/sbin/nft', '-f', '-'])
            self.assertIn('timeout 120s', kw['input'])
        with patch.object(host, 'relay_isolation', isolation.relay_isolation, create=True), patch.object(host, 'command', self.command), patch.object(isolation.shutil, 'which', return_value='/sbin/nft'), patch.object(isolation.subprocess, 'run', apply):
            self.assertEqual(host.execute('relay-isolate', {}), {'completed': True})
        timer = self.calls[-1]
        self.assertIn('--on-active=120s', timer)
        self.assertIn('--timer-property=AccuracySec=1s', timer)
        self.assertIn('--collect', timer)
        self.assertIn('flock', timer[-1])
        self.assertIn("get('comment')", timer[-1])
        # The expiry executable contains everything it needs after its parent dies.
        compile(timer[-1], '<expiry>', 'exec')

    def test_expiry_removes_only_its_generation(self):
        with patch.object(isolation.shutil, 'which', return_value='/sbin/nft'), patch.object(isolation.subprocess, 'run'):
            isolation.relay_isolation('relay-isolate', {'isolationSeconds': 1}, self.command)
        script = self.calls[-1][-1]
        for same in (True, False):
            calls = []
            def run(args, **kw):
                calls.append(args)
                generation = self.calls[-1][3].split('isolation-', 1)[1]
                return type('Result', (), {'returncode': 0, 'stdout': json.dumps({'nftables': [{'table': {'comment': generation if same else 'other'}}]})})()
            with patch.object(subprocess, 'run', run):
                exec(script, {})
            self.assertEqual(len(calls), 2 if same else 1)
            if same:
                self.assertEqual(calls[-1], ['/sbin/nft', 'delete', 'table', 'inet', isolation.ISOLATION_TABLE])

    def test_bounds_and_missing_prerequisite_never_mutate(self):
        for seconds in (0, -1, 301, 1.5, True, '120'):
            with self.subTest(seconds=seconds), patch.object(isolation.shutil, 'which', return_value='/sbin/nft'), self.assertRaises(ValueError):
                isolation.relay_isolation('relay-isolate', {'isolationSeconds': seconds}, self.command)
        with patch.object(isolation.shutil, 'which', return_value=None), self.assertRaises(ValueError):
            isolation.relay_isolation('relay-isolate', {}, self.command)
        self.assertEqual(self.calls, [])

    def test_timer_failure_does_not_apply_and_duplicate_does_not_extend(self):
        with patch.object(isolation.shutil, 'which', return_value='/sbin/nft'), patch.object(isolation.subprocess, 'run') as apply:
            def refuse(args):
                if args[0] == 'systemd-run':
                    raise subprocess.CalledProcessError(1, args)
                return self.command(args)
            with self.assertRaises(subprocess.CalledProcessError):
                isolation.relay_isolation('relay-isolate', {}, refuse)
            apply.assert_not_called()
            self.present = True
            with self.assertRaisesRegex(ValueError, 'already isolated'):
                isolation.relay_isolation('relay-isolate', {}, self.command)
            apply.assert_not_called()

    def test_restore_is_idempotent_and_preserves_other_tables(self):
        self.present = True
        with patch.object(isolation.shutil, 'which', return_value='/sbin/nft'):
            for _ in range(2):
                self.assertEqual(isolation.relay_isolation('relay-restore', {}, self.command), {'completed': True})
        self.assertEqual([c for c in self.calls if c[1] == 'delete'], [['/sbin/nft', 'delete', 'table', 'inet', isolation.ISOLATION_TABLE]])

    def test_relay_status_uses_the_selected_endpoint(self):
        endpoints = {'relay': 'http://192.0.2.1:24700', 'relayTwo': 'http://192.0.2.2:24700'}
        for slot, key in (('relay', 'relay'), ('relay-two', 'relayTwo')):
            with patch.object(host, 'http', return_value={}) as http:
                host.execute('relay-status', dict(endpoints, slot=slot))
                http.assert_called_once_with(endpoints[key] + '/status')
