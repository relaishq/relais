import base64
import unittest

from relais_agent.conversation import Conversation


def attached():
    return dict(scope=dict(session_id="call", bot_instance="bot", epoch=2,
                           generation=3, lineage_id="successor"), checkpoint_seq=7,
                limits=dict(working_bytes=1800, history_bytes=8192))


class ConversationTests(unittest.TestCase):
    def test_restore_baseline_and_exact_ack_before_generation(self):
        state = attached() | dict(working=base64.b64encode(b"user").decode())
        conversation = Conversation(state)
        checkpoint = conversation.user_text("completed user text")
        self.assertEqual(checkpoint["seq"], 8)
        self.assertFalse(conversation.generation_ready())
        ack = checkpoint | dict(type="checkpointed", scope=state["scope"])
        for changed in (dict(ack, seq=7), dict(ack, working=""),
                        dict(ack, history="wrong"),
                        dict(ack, scope=dict(state["scope"], generation=2))):
            self.assertFalse(conversation.acknowledge(changed))
            self.assertFalse(conversation.generation_ready())
        self.assertTrue(conversation.acknowledge(ack))
        self.assertTrue(conversation.generation_ready())
        self.assertFalse(conversation.acknowledge(ack))

    def test_user_overflow_refuses_before_generation_without_mutation(self):
        conversation = Conversation(attached())
        with self.assertRaisesRegex(ValueError, "user_text_too_large"):
            conversation.user_text("😀" * 1000)
        self.assertEqual(conversation.sequence, 7)
        self.assertEqual(conversation.working, b"")
        self.assertIsNone(conversation.pending)

    def test_history_pressure_preserves_whole_tool_exchange_and_discloses_truncation(self):
        conversation = Conversation(attached())
        exchange = dict(user="u" * 500, assistant="a" * 500,
                        tools=[dict(call="lookup", result="r" * 400)])
        for _ in range(12):
            conversation.exchange(exchange, summary="bounded summary")
        state = conversation.history_state
        self.assertEqual(len(state["exchanges"]), 4)
        self.assertEqual(state["truncated"], 8)
        self.assertTrue(all(e == exchange for e in state["exchanges"]))
        self.assertLessEqual(len(conversation.history), 8192)
        before = conversation.history, conversation.sequence
        with self.assertRaisesRegex(ValueError, "exchange_too_large"):
            conversation.exchange(dict(user="x" * 9000))
        self.assertEqual((conversation.history, conversation.sequence), before)
        restored = Conversation(attached() | dict(history=base64.b64encode(conversation.history).decode()))
        self.assertEqual(restored.history_state, state)

    def test_latest_pair_cannot_be_acknowledged_by_an_older_candidate(self):
        conversation = Conversation(attached())
        first = conversation.user_text("first")
        second = conversation.user_text("second")
        self.assertFalse(conversation.acknowledge(first | dict(type="checkpointed", scope=conversation.scope)))
        self.assertTrue(conversation.acknowledge(second | dict(type="checkpointed", scope=conversation.scope)))

class InterruptedReplacementTests(unittest.TestCase):
    def test_revoke_chain_recovers_crash_before_successor_assignment(self):
        from relais_agent.__main__ import Runner
        runner = Runner("isolated-test-token", 1)
        runner.bot_instance = "bot"
        runner.accept(dict(session_id="call", bot_instance="bot", epoch=1,
                           generation=1, lineage_id="old"))
        first = dict(type="revoke", session_id="call", bot_instance="bot",
                     old_epoch=1, old_generation=1, new_epoch=1,
                     new_generation=2, request_id="first")
        self.assertIsNotNone(runner.revoke(first))
        second = dict(first, old_generation=2, new_epoch=2,
                      new_generation=3, request_id="after-crash")
        self.assertIsNone(runner.revoke(dict(second, bot_instance="foreign")))
        self.assertIsNotNone(runner.revoke(second))
        self.assertEqual(runner.floors["call"], ((2, 3), None))
        runner.accept(dict(session_id="call", bot_instance="bot", epoch=2,
                           generation=3, lineage_id="recovered"))
        self.assertIsNone(runner.revoke(first))

    def test_takeover_reconciles_revoke_when_only_old_scope_was_checkpointed(self):
        from relais_agent.__main__ import Runner
        runner = Runner("isolated-test-token", 1)
        runner.bot_instance = "bot"
        runner.accept(dict(session_id="call", bot_instance="bot", epoch=1,
                           generation=1, lineage_id="old"))
        first = dict(type="revoke", session_id="call", bot_instance="bot",
                     old_epoch=1, old_generation=1, new_epoch=1,
                     new_generation=2, request_id="before-crash")
        self.assertIsNotNone(runner.revoke(first))
        replacement = dict(first, new_epoch=2, request_id="new-owner")
        self.assertIsNone(runner.revoke(dict(replacement, bot_instance="foreign")))
        self.assertIsNotNone(runner.revoke(replacement))
        runner.accept(dict(session_id="call", bot_instance="bot", epoch=2,
                           generation=2, lineage_id="settled"))
        self.assertIsNone(runner.revoke(first))

    def test_pending_successor_can_move_before_its_first_assignment(self):
        from relais_agent.__main__ import Runner
        runner = Runner("isolated-test-token", 1)
        runner.bot_instance = "bot"
        runner.accept(dict(session_id="call", bot_instance="bot", epoch=1,
                           generation=1, lineage_id="old"))
        runner.revoke(dict(type="revoke", session_id="call", bot_instance="bot",
                           old_epoch=1, old_generation=1, new_epoch=1,
                           new_generation=2, request_id="replace"))
        with self.assertRaises(ValueError):
            runner.accept(dict(session_id="call", bot_instance="bot", epoch=2,
                               generation=3, lineage_id="unrevoked"))
        runner.accept(dict(session_id="call", bot_instance="bot", epoch=2,
                           generation=2, lineage_id="first-assignment"))
        with self.assertRaises(ValueError):
            runner.accept(dict(session_id="call", bot_instance="bot", epoch=3,
                               generation=2, lineage_id="different"))


class GenerationGateTests(unittest.TestCase):
    def test_provider_guard_waits_for_completed_user_text_checkpoint(self):
        from relais_agent.__main__ import Runner, Session
        runner = Runner("isolated-test-token", 1)
        bot = runner.sessions["call"] = Session(1)
        bot.conversation = Conversation(attached())
        ticket = bot.permit.bind("owner", bot.conversation.scope)
        request = bot.permit.request()
        self.assertTrue(bot.permit.receive("owner", dict(request, type="permit", remaining_ms=5000)))
        checkpoint = bot.conversation.user_text("completed input")
        with self.assertRaises(PermissionError):
            runner.start_provider("call", "generation", ticket)
        self.assertTrue(bot.conversation.acknowledge(checkpoint | dict(type="checkpointed", scope=bot.conversation.scope)))
        runner.start_provider("call", "generation", ticket)
        with self.assertRaisesRegex(ValueError, "user_text_too_large"):
            bot.conversation.user_text("x" * 9000)
            runner.start_provider("call", "generation", ticket)
        self.assertEqual(len(bot.permit.ledger), 1)


class PendingConversationMoveTests(unittest.TestCase):
    def test_pending_pair_keeps_its_sequence_across_an_exact_move(self):
        conversation = Conversation(attached())
        candidate = conversation.user_text("not yet acknowledged")
        moved = attached() | dict(scope=dict(conversation.scope, epoch=3))
        resumed = conversation.resumed(moved)
        self.assertEqual(resumed.sequence, 8)
        self.assertEqual(resumed.pending, candidate)
        self.assertFalse(resumed.generation_ready())
        self.assertFalse(resumed.acknowledge(candidate | dict(type="checkpointed", scope=conversation.scope)))
        self.assertTrue(resumed.acknowledge(candidate | dict(type="checkpointed", scope=resumed.scope)))
        self.assertEqual(resumed.user_text("next")["seq"], 9)
        replacement = moved | dict(scope=dict(resumed.scope, generation=4, lineage_id="replacement"))
        self.assertEqual(resumed.resumed(replacement).user_text("new generation")["seq"], 8)


class ExactHistoryBoundaryTests(unittest.TestCase):
    def test_truncation_counter_growth_cannot_drop_the_new_exchange(self):
        from relais_agent.conversation import encode
        conversation = Conversation(attached())
        conversation.history_state = dict(summary="", exchanges=[dict(user="old")] * 4, truncated=9)
        conversation.history = encode(conversation.history_state)
        exchange = dict(user="x" * 200)
        conversation.history_bytes = len(encode(dict(summary="", exchanges=[exchange], truncated=9)))
        before = conversation.history, conversation.sequence
        with self.assertRaisesRegex(ValueError, "exchange_too_large"):
            conversation.exchange(exchange)
        self.assertEqual((conversation.history, conversation.sequence), before)
