"""Bounded text checkpoints. A local Save is not a durable acknowledgement."""
import base64
import json


def encode(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode()


class Conversation:
    def __init__(self, attached):
        self.scope = dict(attached["scope"])
        self.working_bytes = attached["limits"]["working_bytes"]
        self.history_bytes = attached["limits"]["history_bytes"]
        self.sequence = attached["checkpoint_seq"]
        self.durable_sequence = self.sequence
        self.working = base64.b64decode(attached.get("working") or "", validate=True)
        self.history = base64.b64decode(attached.get("history") or "", validate=True)
        self.pending = None
        self.history_state = (json.loads(self.history) if self.history else
                              {"summary": "", "exchanges": [], "truncated": 0})

    def user_text(self, text):
        working = encode({"user_text": text})
        if len(working) > self.working_bytes:
            raise ValueError("user_text_too_large")
        self.working = working
        return self.checkpoint()

    def exchange(self, exchange, summary=None):
        # Exchanges are indivisible, including tool/result pairs supplied by apps.
        state = dict(self.history_state)
        state["exchanges"] = [*state["exchanges"], exchange]
        if summary is not None:
            state["summary"] = summary
        while (len(state["exchanges"]) > 4
               or (len(state["exchanges"]) > 1 and len(encode(state)) > self.history_bytes)):
            state["exchanges"].pop(0)
            state["truncated"] += 1
        history = encode(state)
        if len(history) > self.history_bytes:
            raise ValueError("exchange_too_large")
        self.history_state, self.history = state, history
        return self.checkpoint()

    def checkpoint(self):
        # One latest complete pair; callers must not start generation until its
        # exact scoped acknowledgement. A superseded candidate stays unpromised.
        self.sequence += 1
        self.pending = dict(type="checkpoint", seq=self.sequence, version=1,
                            working=base64.b64encode(self.working).decode(),
                            history=base64.b64encode(self.history).decode())
        return dict(self.pending)

    def acknowledge(self, message):
        if (self.pending is None or message.get("type") != "checkpointed"
                or message.get("scope") != self.scope
                or message.get("seq") != self.pending["seq"]
                or any((message.get(k) or "") != self.pending[k] for k in ("working", "history"))):
            return False
        self.durable_sequence = self.pending["seq"]
        self.pending = None
        return True

    def generation_ready(self):
        return self.pending is None and self.durable_sequence == self.sequence

    def resumed(self, attached):
        restored = Conversation(attached)
        identity = ("session_id", "bot_instance", "generation", "lineage_id")
        if any(restored.scope[k] != self.scope[k] for k in identity):
            return restored
        if restored.scope["epoch"] < self.scope["epoch"]:
            raise ValueError("stale conversation scope")
        restored.sequence = max(restored.sequence, self.sequence)
        if self.pending and self.pending["seq"] > restored.durable_sequence:
            restored.working, restored.history = self.working, self.history
            restored.history_state = (json.loads(self.history) if self.history else
                                      {"summary": "", "exchanges": [], "truncated": 0})
            restored.pending = dict(self.pending)
        elif self.pending and self.pending["seq"] == restored.durable_sequence:
            if (self.working, self.history) != (restored.working, restored.history):
                raise ValueError("checkpoint_conflict")
        return restored
