import asyncio
import json
import time
import unittest
from unittest.mock import AsyncMock, patch

from relais_agent.__main__ import Runner, Session


class Messages:
    def __init__(self, messages):
        self.messages = iter(messages)
        self.sent = []

    def __aiter__(self):
        return self

    async def __anext__(self):
        try:
            return json.dumps(next(self.messages))
        except StopIteration:
            raise StopAsyncIteration

    async def send(self, raw):
        self.sent.append(json.loads(raw))


def scope(runner, session="call", generation=1):
    return dict(session_id=session, epoch=1, generation=generation,
                bot_instance=runner.bot_instance, lineage_id=f"lineage-{generation}")


def reservation(session="call"):
    return dict(type="reserve", session_id=session, epoch=1,
                reservation_id=session, expires_ms=time.time_ns() // 1000000 + 5000)


def assigned(runner, reserved, generation=1):
    return reserved | scope(runner, reserved["session_id"], generation) | dict(type="assign", attach_token="token")



class RunnerFaultTests(unittest.IsolatedAsyncioTestCase):
    async def test_clean_registry_loss_ends_owned_sessions(self):
        runner = Runner("isolated-test-token", 1)
        bot = runner.sessions["call"] = Session(1)
        self.addAsyncCleanup(bot.close)
        bot.permit.bind("owner", scope(runner))
        bot.worker_task = asyncio.create_task(asyncio.Event().wait())
        bot.producer = asyncio.create_task(asyncio.Event().wait())
        with patch.object(runner, "registry_connected", AsyncMock()):
            await runner.registry("owner", asyncio.Event())
        self.assertTrue(bot.worker_task.done())
        self.assertTrue(bot.producer.done())
        self.assertIsNone(bot.permit.ticket)
        self.assertNotIn("call", runner.sessions)

    async def test_refusals_do_not_end_registry_or_acknowledge_stale_revoke(self):
        runner = Runner("isolated-test-token", 1)
        old = scope(runner)
        runner.accept(old)
        reserve = reservation()
        revoke = dict(type="revoke", session_id="call", bot_instance=runner.bot_instance,
                      old_epoch=1, old_generation=1, new_epoch=1, new_generation=2, request_id="replace")
        next_reserve = reservation()
        ws = Messages([dict(revoke, old_generation=0), reserve, revoke,
                       assigned(runner, reserve), dict(revoke, new_generation=3),
                       next_reserve, assigned(runner, next_reserve, 2)])
        with patch.object(Session, "attach", AsyncMock()) as attach:
            await runner.registry_messages(ws, "owner", asyncio.Event())
        self.assertEqual(attach.await_count, 1)
        self.assertEqual([m for m in ws.sent if m["type"] == "revoked"], [dict(revoke, type="revoked")])
        self.assertEqual(runner.floors["call"][0], (1, 2))

    async def test_attach_failure_is_local_and_frees_capacity(self):
        runner = Runner("isolated-test-token", 1)
        first, second = reservation(), reservation("other")
        ws = Messages([first, assigned(runner, first), second, assigned(runner, second)])
        with patch.object(Session, "attach", AsyncMock(side_effect=[OSError("attachment closed"), None])) as attach:
            await runner.registry_messages(ws, "owner", asyncio.Event())
        self.assertEqual(attach.await_count, 2)
        self.assertNotIn("call", runner.sessions)
        self.assertIn("other", runner.sessions)

    async def test_post_revoke_assignment_cannot_bind_another_bot(self):
        runner = Runner("isolated-test-token", 1)
        runner.accept(scope(runner))
        runner.revoke(dict(type="revoke", session_id="call", bot_instance=runner.bot_instance,
                           old_epoch=1, old_generation=1, new_epoch=1, new_generation=2, request_id="replace"))
        with self.assertRaises(ValueError):
            runner.accept(dict(scope(runner, generation=2), bot_instance="foreign"))
        self.assertEqual(runner.floors["call"], ((1, 2), None))
