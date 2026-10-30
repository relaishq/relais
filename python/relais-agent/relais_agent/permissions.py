"""Local provider/tool admission. Applications must guard every external start."""
from collections import deque
import time
import uuid


class Permit:
    def __init__(self, clock=time.monotonic):
        self.clock = clock
        self.scope = None
        self.owner = None
        self.ticket = None
        self.pending = None
        self.deadline = 0
        self.ledger = deque(maxlen=128)

    def disable(self):
        self.ticket = None
        self.pending = None
        self.deadline = 0

    def bind(self, owner, scope):
        self.disable()
        self.scope = {k: scope[k] for k in ("session_id", "epoch", "generation")}
        self.owner = owner
        self.ticket = object()
        return self.ticket

    def disconnect(self, ticket):
        if ticket is self.ticket:
            self.disable()

    def request(self):
        if self.ticket is None:
            raise PermissionError("owner socket unavailable")
        request = dict(self.scope, type="permit_request", nonce=uuid.uuid4().hex)
        self.pending = request, self.clock()
        return request

    def receive(self, owner, message):
        if self.pending is None or self.ticket is None or owner != self.owner:
            return False
        request, start = self.pending
        if (message.get("type") != "permit"
                or any(message.get(k) != request[k] for k in ("session_id", "epoch", "generation", "nonce"))
                or type(message.get("remaining_ms")) is not int
                or not 0 <= message["remaining_ms"] <= 5000):
            return False
        self.pending = None  # A nonce is consumed even by an expired/zero reply.
        deadline = start + message["remaining_ms"] / 1000
        if deadline <= self.clock():
            return False
        self.deadline = deadline
        return True

    def start(self, kind, ticket):
        if self.ticket is None or ticket is not self.ticket or self.clock() >= self.deadline:
            raise PermissionError("live permit required")
        record = (self.scope["session_id"], self.scope["epoch"], self.scope["generation"], kind, self.clock())
        self.ledger.append(record)
        return record
