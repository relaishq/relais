# ADR 0001: Keep an active caller's address on its session

## Context

The shared worker socket (#4) and the relay (#5) route a caller's media by
its source address. An authenticated ICE check proves that the sender knows
a session's credentials. It does not prove that the source address belongs
to that session. So an attacker with valid credentials for its own session
B could send B's checks from caller A's spoofed address and move A's media
away from A's worker (#20).

## Decision

An address sticks to its session while that session is active. "Active"
means that the session authenticated an ICE check from that address within
the stickiness window. While the address is active, a check for a different
session from that address is dropped, even if its credentials are valid.

- **Only the session's own authenticated checks count.** Media, DTLS,
  unanswered or unauthenticated STUN requests, and checks for other sessions
  never keep an address active.
- **The default window is 30 seconds,** the workers' consent timeout
  (RFC 7675). A worker keeps a call alive for 30 seconds without a fresh
  check, so the address stays protected for as long as the worker considers
  the call alive. A shorter window would let an attacker take a call that
  the worker still serves. It is configurable:
  `relay.Config.RouteStickinessWindow`,
  `mediaworker.SocketConfig.RouteStickinessWindow` (applies to every worker
  on the socket), and `mediaworker.Config.RouteStickinessWindow` (a worker on
  its own socket). Use the same value everywhere. Zero selects the default;
  the rule cannot be turned off.
- **The same session may always move.** It can authenticate from a new
  address, re-nominate, or follow its session to a new worker. Trusted
  control-plane moves (`MoveSession`, `ForgetSession`, uncertain-route
  repair) change the worker for a session, not the session for an address,
  so this rule does not affect them.
- **A quiet address can be claimed.** After a full window without an
  authenticated check for the incumbent, another session can take the
  address, as before. A hangup on a shared socket releases the address at
  once. The relay learns of a hangup only through an owner lookup or
  `ForgetSession`; otherwise it waits for the window.
- **Both places follow the same rule.**
  - Shared socket and worker: the check is applied after a request
    authenticates and before it changes nomination, consent or the address
    map. A rejected check gets no answer. The worker's own address map
    applies the same rule, which also covers two sessions on one worker.
  - Relay: the relay cannot check credentials, so it uses the worker's
    answer. It remembers a few unanswered requests per route. A binding
    success from the route's worker that answers one of them renews the
    route, as of the request's arrival (a request held during a planned move
    counts from the hold's release). Each success consumes its request, so a
    replayed or unmatched success renews nothing. An active route rejects
    candidates for other sessions, and the relay checks again before it
    promotes a candidate, because the incumbent may have renewed meanwhile.
    Idle cleanup does not drop an active route.
- **Moves keep the evidence.** A relay move keeps the route's last renewal
  time and discards requests still waiting for the old worker. A shared
  socket keeps its flows across a handover. A worker that resumes a session
  has no check time for its address until the next check; the relay or
  shared socket protects the address until then.

## Consequences

- A new session that reuses an address can wait up to one window after the
  previous session's last check.
- Each relay caller holds two fixed-size request sets and one timestamp. The
  socket and worker timestamps are removed with their flows. No state grows
  per packet.
- The rule protects against credentials for another session. It does not
  protect against stolen credentials for the victim's session or a
  compromised worker.
- An attacker that also knows A's session ID could flood unauthenticated
  checks for A from A's address. That can push A's real requests out of the
  relay's small request set before the answers arrive, so A's route would
  stop renewing and lapse after one window. Rate limiting per caller would
  close this; it is not part of this decision.

## Restart residual risk and persisted routes (#42)

A restarted relay has an empty flow table, so it does not know which
addresses are active. Until A's next authenticated check, an attacker can
claim A's address. Normal restart recovery still works. Persisted routes
(#42) close this gap. They must restore the address, the session, and the
last renewal time, and take the current worker from authoritative
ownership. Restoring a route or moving it to another worker must never
create a fresh renewal time.
