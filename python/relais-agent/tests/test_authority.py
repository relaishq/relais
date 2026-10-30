import unittest

from relais_agent.__main__ import Runner, Session


def assignment(epoch=1, generation=10, **fields):
    return dict(session_id="call", epoch=epoch, generation=generation,
                bot_instance="bot", lineage_id="lineage", **fields)


def revoke(epoch=1, generation=10, new_epoch=1, new_generation=11, request_id="replace"):
    return dict(type="revoke", session_id="call", bot_instance="bot",
                old_epoch=epoch, old_generation=generation, new_epoch=new_epoch,
                new_generation=new_generation, request_id=request_id)


class RevocationTests(unittest.TestCase):
    def test_revoke_advances_floor_before_ack_and_exact_retries_only(self):
        runner = Runner("isolated-test-token", 150)
        old = assignment()
        runner.accept(old)
        bot = runner.sessions["call"] = Session(1)
        old_ticket = bot.permit.bind("owner", old)
        pending = bot.permit.request()
        self.assertTrue(bot.permit.receive("owner", dict(pending, type="permit", remaining_ms=5000)))
        bot.permit.start("provider", old_ticket)
        runner.reservations["owner"] = dict(session_id="call")
        message = revoke()
        ack = runner.revoke(message)
        self.assertEqual(runner.reservations, {})
        with self.assertRaises(PermissionError):
            bot.permit.start("tool", old_ticket)
        self.assertFalse(bot.permit.receive("owner", dict(pending, type="permit", remaining_ms=5000)))
        self.assertEqual(len(bot.permit.ledger), 1)
        self.assertEqual(ack, dict(message, type="revoked"))
        self.assertEqual(runner.revoke(message), ack)
        for changed in (old, dict(old, epoch=0, generation=999),
                        dict(old, generation=10, lineage_id="other")):
            with self.assertRaises(ValueError):
                runner.accept(changed)
        with self.assertRaises(ValueError):
            runner.revoke(dict(message, new_generation=12))
        successor = dict(old, generation=11, lineage_id="new")
        runner.accept(successor)
        new_ticket = bot.permit.bind("owner", successor)
        pending = bot.permit.request()
        self.assertTrue(bot.permit.receive("owner", dict(pending, type="permit", remaining_ms=5000)))
        with self.assertRaises(PermissionError):
            bot.permit.start("tool", old_ticket)
        bot.permit.start("provider", new_ticket)
        self.assertEqual(bot.permit.ledger[-1][1:3], (1, 11))
        with self.assertRaises(ValueError):
            runner.accept(dict(old, generation=11, lineage_id="other"))
        with self.assertRaises(ValueError):
            runner.revoke(message)  # Cannot acknowledge an obsolete request after assignment.

    def test_epoch_precedes_generation_and_unconfirmed_lineage_is_not_reconnect(self):
        runner = Runner("isolated-test-token", 150)
        runner.accept(assignment())
        runner.revoke(revoke(new_epoch=2, new_generation=1))
        with self.assertRaises(ValueError):
            runner.accept(assignment(generation=999))
        runner.accept(dict(assignment(2, 1), lineage_id="new"))
        with self.assertRaises(ValueError):
            runner.accept(dict(assignment(3, 1), lineage_id="different"))
        runner.accept(dict(assignment(3, 1), lineage_id="new"))
        with self.assertRaises(ValueError):
            runner.revoke(revoke(new_epoch=2, new_generation=2, request_id="stale"))


class PermitTests(unittest.TestCase):
    def test_ledger_rejects_expired_revoked_and_delayed_permits(self):
        from relais_agent.permissions import Permit
        now = [100.0]
        p = Permit(clock=lambda: now[0])
        scope = assignment()
        ticket = p.bind("owner", scope)
        with self.assertRaises(PermissionError):
            p.start("provider", ticket)
        request = p.request()
        now[0] += 4
        reply = dict(request, type="permit", remaining_ms=5000)
        self.assertTrue(p.receive("owner", reply))
        p.start("provider", ticket)
        now[0] += 1
        with self.assertRaises(PermissionError):
            p.start("tool", ticket)
        self.assertFalse(p.receive("owner", reply), "duplicate reply must not replenish time")
        request = p.request()
        now[0] += 6
        self.assertFalse(p.receive("owner", dict(request, type="permit", remaining_ms=5000)))
        request = p.request()
        p.disable()
        self.assertFalse(p.receive("owner", dict(request, type="permit", remaining_ms=5000)))
        with self.assertRaises(PermissionError):
            p.start("tool", ticket)
        self.assertEqual(len(p.ledger), 1)
        self.assertEqual(p.ledger[0][1:3], (1, 10))
        new_ticket = p.bind("new-owner", dict(scope, epoch=2))
        p.disconnect(ticket)  # Late source close must not disconnect its successor.
        request = p.request()
        self.assertFalse(p.receive("owner", dict(request, type="permit", remaining_ms=5000)))
        self.assertTrue(p.receive("new-owner", dict(request, type="permit", remaining_ms=2000)))
        with self.assertRaises(PermissionError):
            p.start("tool", ticket)
        p.start("tool", new_ticket)
        self.assertEqual(p.ledger[-1][1:3], (2, 10))

    def test_permit_scope_nonce_and_remaining_time_are_validated(self):
        from relais_agent.permissions import Permit
        p = Permit(clock=lambda: 100)
        ticket = p.bind("owner", assignment())
        request = p.request()
        for changes in (dict(epoch=0), dict(generation=11), dict(nonce="wrong"),
                        dict(session_id="other"), dict(remaining_ms=5001),
                        dict(remaining_ms=-1)):
            self.assertFalse(p.receive("owner", dict(request, type="permit", remaining_ms=5000) | changes))
        self.assertTrue(p.receive("owner", dict(request, type="permit", remaining_ms=5000)))
        p.start("provider", ticket)
