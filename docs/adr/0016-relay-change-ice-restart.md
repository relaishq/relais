# ADR 0016: Move live calls to a new relay address through ICE restart

Status: **Proposed** 2026-10-29. It stays Proposed until the owner reviews it.
Part of [#278](https://github.com/relaishq/relais/issues/278), step 3 of the
scale-to-zero map [#275](https://github.com/relaishq/relais/issues/275).
It chooses the "several relays / ICE restart" path that
[ADR 0011](0011-deploy-and-autoscale.md) section 3 left open, and it builds on
the proposed worker dial-out mode ([ADR 0015](0015-worker-dial-out.md), open on
`docs/276-dial-out-adr`). Code baseline: `cb64d96`.

Today every call is tied to one relay address for its whole life. A relay
process can restart in place (ADR 0003, ADR 0004), but a lost relay host drops
every call (ADR 0011). This ADR lets a live call change to a relay with a
different public address. The caller learns the new address from signaling and
completes an ICE restart. DTLS and SRTP do not change, so media continues with
the same keys.

## Owner summary

- **Mechanism.** Control asks the caller, over a signaling event stream, to
  restart ICE. The caller sends a new offer. The owning worker answers with new
  ICE credentials and the new relay's address as its only candidate.
- **Planned change** (we retire a relay): every call is kept. The caller gap
  target is the same as for planned moves: pooled p95 ≤ 150 ms.
- **Unplanned relay-host loss:** every call recovers within **5 s**, with no
  dropped calls. This needs a second relay that is already running.
- **Fence.** The worker, which holds the media keys, is the fence. It sends a
  call's media through exactly one relay. A per-call relay generation in the
  store names which relays may carry the call, and it only goes up.
- **Fallback.** A planned change that does not finish rolls back to the old
  relay. An unplanned change retries on another relay. After a hard deadline the
  call ends with an explicit reason. No call waits forever in a frozen state.
- **Browsers this round:** real headless Chrome and the Pion caller. Safari is
  later and untested.
- **No feature flag.** One relay works exactly as today. Relay change is
  possible when control knows more than one relay.
- **Open source.** The whole mechanism in this ADR is open source and works on
  its own: the event stream, the ICE restart, the relay generation fence and
  control's relay-loss rule. Tasks such as starting replacement relay hosts,
  keeping warm spares, managed failover policy and alerting may later be offered
  as an operations layer, through the documented interfaces below.

## Today

| Topic | Current behaviour |
| --- | --- |
| How the caller learns the relay | Once, in the SDP answer to `POST /calls`. The worker's answer has one host candidate: the relay's public address (`RelayConfig.PublicAddr`, worker flag `-relay-media`). |
| Signaling | Request and reply only: `POST /calls`, `DELETE /calls/{id}`, `POST /calls/{id}/agent/control` through `relais-signal`. The server cannot send anything to the caller after the answer. |
| ICE role | The worker is ICE-lite (RFC 8445 section 2.5). It never sends checks. It answers the caller's checks with `pion/stun` and keeps ufrag, password and nominated address in the session state. A `USE-CANDIDATE` from a new address re-nominates, and the worker moves the session there. |
| Session ID | The worker's ICE ufrag **is** the session ID (16 characters from `[a-zA-Z0-9]`). The relay routes a binding request by the ufrag before the `:` in `USERNAME`. |
| Relay set | One. Control has one `-relay` URL. A worker has one `-relay-leg` and one `-relay-media` address, and accepts leg datagrams only from that leg. |
| In-band channel | None. The worker rejects `application` m-lines, so there is no data channel. A web page cannot read RTCP. |
| Relay recovery | ADR 0004: a same-host standby takes over the **same** addresses after verified process exit or lease staleness. Measured caller gap below 1 s. No cross-host takeover. |

## What an ICE-lite server can and cannot start

An ICE restart replaces the ICE ufrag and password on both sides (RFC 8445
section 9). In browsers, the side that makes the offer starts it
(`RTCPeerConnection.restartIce()` or `createOffer({iceRestart: true})`, JSEP
RFC 8829). The DTLS association and SRTP keys stay. Only the ICE path changes.

An ICE-lite worker **cannot**:

- send connectivity checks, so it cannot test or nominate a new path itself;
- signal in-band: there is no data channel, and RTCP is not visible to page code;
- make a browser switch to another candidate pair after nomination without a
  restart (RFC 8445 section 8.1.1, cited as E39 in ADR 0011).

An ICE-lite worker **can**:

- answer a restart offer with new credentials and new candidates;
- in theory, send a new offer itself. Relais has no server-to-caller channel for
  this, and the worker only answers offers today;
- stop answering checks, so the browser's ICE state goes to `disconnected` and
  later `failed`. This is slow, it looks like a real outage, and the page still
  needs code to restart.

**Decision: server-requested, caller-started ICE restart.** Control sends a
`relay-change` event on a signaling event stream. The caller runs the standard
browser restart and sends the new offer. The caller also starts a restart on
its own when its media stops (section 4), so a lost event stream does not block
recovery.

## Decision

### 1. Relay set, relay slot and relay assignment

New terms (added to the glossary when this ADR is accepted):

- **Relay slot:** one relay's public UDP address, private leg address, private
  HTTP address and ADR 0004 lease key. A slot can have a same-host standby, as
  today. A standby takeover keeps the slot. The slot ID is the lease key.
- **Relay set:** the slots control knows. Control takes `-relay name=URL`,
  repeated. The old single `-relay URL` form means a set of one.
- **Relay assignment:** per call, stored in the call's existing session hash
  (`{sess:ID}`, next to the lease and the ADR 0003 routes): `relay` (slot ID),
  `relay_gen` (a counter that never goes down) and, during a change,
  `relay_pending` (slot ID for `relay_gen + 1`).

Control chooses the slot for each new call (least loaded, not draining, not
lost) and passes it to the worker in create. The worker's answer uses that
slot's public address. Workers learn the relay set from control: an inbound
worker through its private API, a dial-out worker through its command link
(ADR 0015). The worker flags `-relay-leg` and `-relay-media` remain as the
bootstrap slot.

A worker keeps a leg to every slot in the set. A session's **path** becomes
(relay slot, caller address), not only the caller address. The relay-leg header
does not change.

### 2. The restarted ufrag

ICE requires a new ufrag on restart, but routing depends on the ufrag. The
worker's ufrag for generation `g > 0` is `<session ID>+<g>`, for example
`a1B2c3D4e5F6g7H8+3`. `+` is a valid ICE character and never appears in a
session ID, so the split is exact. Generation 0 keeps the bare session ID, so
existing calls and old callers do not change. The relay and the worker take the
session ID from the part before `+`. A new random password is made for each
generation.

### 3. One relay change, step by step

Control runs a relay change under the call's existing per-call lock. A worker
move, takeover or agent operation for the same call cannot run at the same time.

```text
caller            signal/control              worker (owner)         relay R2   relay R1
  |                     |-- CAS assignment: pending=R2, gen g -> g+1 (store)    |
  |<- event: relay-change (g+1)                   |                     |        |
  |-- restart offer --->|-- restart(offer, g+1, R2) ->|                 |        |
  |                     |   worker checks offer, makes creds g+1 bound to R2   |
  |<-- answer (ufrag id+g+1, R2 candidate) ---------|                   |        |
  |== media continues on R1 =========================================================|
  |-- checks (g+1) ------------------------------------------------->|  |        |
  |                     |                         |<- forwarded check --|        |
  |                     |                         |   USE-CANDIDATE or caller media: switch|
  |<-------------------------------------- binding success ----------|          |
  |== media on R2 ====================================================|         |
  |                     |<- switched (g+1) -------|                    |        |
  |                     |   worker commits: relay=R2, gen=g+1 (fenced store write)
  |                     |-- forget session tenure ---------------------------->|
```

1. Control writes the pending assignment with a compare-and-set on
   `{sess:ID}`. It checks the current lease epoch and `relay_gen = g`. A call
   has at most one pending change.
2. Control sends `relay-change` with `g + 1` on the call's event stream
   (section 5).
3. The caller restarts ICE and sends the full new offer to
   `POST /calls/{id}/ice-restart`. Control forwards it to the owner with
   `g + 1` and the target slot. Control refuses an offer for any other
   generation.
4. The worker checks that the offer keeps the session: same DTLS fingerprint,
   same MIDs and BUNDLE group, same payload types and SSRCs. It refuses
   anything else with a typed `restart_mismatch` outcome. It answers with the
   original `a=setup` role and fingerprint, the new ufrag and password, and the
   target slot's public address as the only candidate. It keeps generation `g`
   credentials valid on the current path.
5. The caller sends checks to R2. R2 routes the binding request by session ID,
   as today. It forwards it only if the assignment names R2 as current or
   pending (an extra field on the existing owner lookup).
6. The worker authenticates the check with the `g + 1` credentials. It
   switches the session's path to (R2, caller address) **at once** on the
   first of two signals: `USE-CANDIDATE` on an authenticated check, or the
   first authenticated SRTP packet from the caller through R2 after such a
   check. The second signal follows a caller that starts sending on the new
   pair before it nominates it; such a caller may already ignore packets from
   the old relay. The worker answers the check and asks the caller for a
   keyframe, as after a planned move. RTP sequence numbers, timestamps and the
   SRTP state continue.
7. The worker commits `relay = R2, relay_gen = g + 1` with a store write fenced
   by its lease epoch, then reports the switch to control. Control tells R1 to
   forget the call's routes with the existing tenure-checked cleanup.

The switch in step 6 does not wait for the store write: R2 is already
authorized by the pending assignment from step 1. A failed commit is retried. It
never moves the path back.

A worker move or takeover after the commit works as today: the snapshot carries
the current generation, its credentials and the path's slot. A snapshot never
carries pending credentials. If the owner dies during a change, the new owner
resumes on generation `g`. The caller's checks for `g + 1` then fail, and the
caller's own restart deadline (section 4) starts a new change.

### 4. Triggers

| Trigger | When | What control does |
| --- | --- | --- |
| Planned: relay drain | Operator or management API: `POST /relays/{name}/drain` | Mark the slot draining (no new calls), then change each of its calls to another slot. Bounded concurrency (default 8 calls at a time). |
| Unplanned: relay-loss rule | The slot's ADR 0004 lease shows no renewal for **2 s** of reachable observation (the same rule as ADR 0004: failed store polls add no time) | Mark the slot lost (no new calls), remove it from every worker's relay set, and change all its calls to live slots at once. |
| Caller stall | The caller sees ICE `disconnected` or `failed`, or no inbound RTP or RTCP for 2 s while the call is live | The caller calls `POST /calls/{id}/ice-restart` without an event. Control makes a new generation: on the same slot if that slot is live (this also repairs a caller NAT change), otherwise on another slot. |

The 2 s relay-loss threshold is above the measured same-host standby takeover
(below 1 s, [guarantees](../guarantees.md#same-host-standby-relay)), so a
process crash with a standby normally recovers in place without a restart. A
wrong relay-loss decision is safe: it causes one extra relay change, not a lost
call. The caller-stall trigger covers a relay that renews its lease but has lost
its public network.

### 5. The event stream

`relais-signal` and control add `GET /calls/{id}/events`, a long-lived
Server-Sent Events stream over the existing HTTPS listener, with the same bearer
authorization as `DELETE /calls/{id}`. The caller opens it right after `POST
/calls`. Events are JSON: `relay-change` (generation, deadline) and `ended`
(reason). The stream sends a comment line every 5 s so that proxies keep it
open. A caller reconnects with backoff and receives any pending `relay-change`
again. The create request has a `relay_change` capability field. Control only
plans relay changes for capable calls. A call without the capability is not
moved by a drain; the drain waits for it within its deadline and reports it.
On relay loss it ends with `relay_lost`.

A restart offer can disrupt a call but cannot decrypt it: DTLS is not repeated,
and the fingerprint check stops a new certificate. This is the same power that
`DELETE /calls/{id}` already gives.

### 6. Fencing: two relays never both forward a call

The worker holds the keys, so the worker is the fence. The store assignment is
the second line, as kernel socket exclusivity is for ADR 0004.

1. **One forwarding relay.** At any moment the worker sends a call's media and
   keyframe requests on exactly one path. It never sends the same packet
   through two relays. The only exception: a binding response to an
   authenticated check of the pending generation goes back on the path that
   check arrived on, as ICE requires. A caller nominates the pending relay only
   after it has answered a check there.
2. **Bounded receive.** The worker accepts the caller's media from the current
   path. It also accepts it, receive only, from two other paths: from the
   pending path once a check of the pending generation has been authenticated
   through it, until the change commits or rolls back; and from the previous
   path for **500 ms** after a switch. Some callers start sending on the
   pending path before they nominate it, and others still have packets in
   flight on the old path. SRTP replay protection drops any duplicate index.
3. **Assignment bound.** A relay that is not current or pending in the
   assignment gets nothing from the worker for that call, and the worker drops
   what it sends for the call. A relay drops a binding request for a call whose
   assignment does not name it.
4. **Generation only goes up.** Credentials of an older generation are deleted
   after the switch. A rollback is a new, higher generation that names the old
   slot again. A delayed message for an old generation is refused.
5. **Lost slots are cut off.** When control marks a slot lost, workers drop all
   leg traffic from that slot. A frozen relay host that comes back can still send
   packets, but no worker accepts them, and no worker sends to it. It serves
   again only after it restarts as an empty relay with a new instance token.

These rules do not depend on the old relay stopping. This matters because
ADR 0004 cannot fence a relay on another host, and a relay keeps forwarding
existing routes while its store is unreachable.

Relay-held state stays per slot. ADR 0005 replay and ADR 0009 holds use only the
call's current slot. A relay change drops the old slot's ring for that call, as
ADR 0011 already records for relay replacement. ADR 0003 route records gain the
slot ID. A relay restores only routes whose assignment names its slot.

### 7. Fallback when ICE restart fails

Success is **media progress at the caller**, not ICE state. A connected ICE
state or a new selected candidate pair does not count by itself.

| Case | Deadline | Fallback |
| --- | --- | --- |
| Planned change | 10 s from the event to the worker's switch. Media continues on the old relay during this time, so it is not a gap. | Roll back: clear the pending assignment, delete the pending credentials. If the caller still uses the old pair, nothing else happens. If it dropped the old pair, its stall trigger asks for a restart, and control gives a new generation on the old slot, which is still live. Retry the change later with backoff; after 3 failures the drain reports the call as not movable. The call is kept. |
| Unplanned loss | 5 s target. Hard deadline 15 s, well below the 30 s consent limit (RFC 7675 section 5.1). | Retry on another live slot if one exists. After the hard deadline, control ends the call with reason `relay_change_failed` and sends `ended` on the event stream. The page shows an error and may start a new call. This counts as a dropped call and fails the gate. |
| Caller cannot restart (no capability, older browser, or `restartIce` throws) | Immediate | Same as above. The page must end the call and say why. It must not stay "connected" with frozen media. |

A reconnect into the same agent session (new peer connection, same call and
agent state) is a possible later improvement. It is not part of this ADR.

### 8. How this composes with other relay and worker mechanisms

- **Same-host standby (ADR 0004).** Unchanged. Each slot keeps its lease,
  its standby and its fixed addresses. A process crash recovers in place with no
  restart. Relay change handles a lost or retired host, which ADR 0004 does not
  cover. The two do not race on fencing: a standby takeover keeps the slot ID,
  and the assignment uses the slot ID.
- **Planned moves and takeover (ADR 0009, ADR 0013).** One operation per call
  at a time. A worker move keeps the assignment. The new owner must have a leg to
  the call's current slot; control already registers every worker with every slot.
- **Dial-out workers (ADR 0015).** A dial-out worker keeps one leg binding per
  slot: a leg hello and keepalives to each relay. When a slot joins, control
  sends the worker a leg token for it over the command link. Behind a NAT, each
  relay can see a different worker address. The lease keeps one worker identity
  (the address seen by the first relay, as ADR 0015 section 5 says). Control
  gives each other relay the mapping from that identity to the address it saw,
  in `POST /workers`. This needs a short amendment to ADR 0015 section 5 if both
  are accepted. Inbound workers use the same private address for every relay.
- **Store outage (ADR 0008).** A relay change needs store writes. During a store
  outage, changes wait like other ownership changes. Media on the current relay
  continues.
- **Scale to zero (#275).** A new relay is a new slot that joins the set. A slot
  with no calls can stop. Starting a relay on demand is slower than 5 s today, so
  the unplanned 5 s target needs a second slot that is already running.

### 9. Unplanned recovery budget

Measured per call, from the last caller media before the loss to the first
decoded caller media after the switch.

| Stage | Allowance |
| --- | ---: |
| Detection (relay-loss rule, or caller stall) | 2.0 s |
| Assignment write and event push | 0.3 s |
| Restart offer to worker answer | 0.5 s |
| Checks and nomination through the new relay | 0.5 s |
| Keyframe and first decoded media | 0.7 s |
| Margin for many calls changing at once | 1.0 s |
| **Total** | **5.0 s** |

### 10. Release compatibility

The [#252 release manifest](https://github.com/relaishq/relais/issues/252) adds:

- the relay behaviour for `+<generation>` ufrags and the assignment check, as a
  raised relay contract version;
- the private worker API `restart` command and the relay set update;
- the session state format: ICE generation and path slot;
- the store schema fields `relay`, `relay_gen` and `relay_pending`;
- the signaling event stream and `POST /calls/{id}/ice-restart`.

Preflight rules:

1. Control starts a relay change only when every slot in the set and the owning
   worker read the new contracts. A set of one needs nothing new.
2. A call with `relay_gen > 0` does not move to a worker without this release.
3. Control or relays cannot roll back below this release while any call has
   `relay_gen > 0` or the set has more than one slot.

## Measurement

Evidence comes from the in-process harness (Pion caller), `crash-run` and a new
headless Chrome driver with fake media. Every row reports p50, p95 and max per
call. Planned and unplanned results are reported separately.

| # | Criterion | Method | Target |
| --- | --- | --- | --- |
| 1 | Planned change gap | Two slots; at least 20 changes with the Pion caller (smoke size), and 10 in Chrome; also 8 calls changed in one drain | Existing planned-move policy: each track p95 ≤ same-run baseline + 50 ms and ≤ 150 ms, pooled; zero reconnects |
| 2 | Unplanned host loss | Kill the active **and** standby relay of a slot (SIGKILL), and drop all slot traffic in a network namespace; ten interleaved `crash-run` runs per mode; 8 live calls per run; Pion and Chrome | Every call recovers within 5 s; zero dropped calls |
| 3 | Frozen host | SIGSTOP both relay processes of a slot, recover on another slot, then SIGCONT | After the cut-off, zero packets from the old slot accepted by any worker; zero worker packets sent to it |
| 4 | Caller-stall path | Block the slot's public address only; its lease keeps renewing | Calls recover within 5 s through the caller trigger |
| 5 | Wrong relay-loss decision | Control loses its view of a live relay | Calls change relay; zero dropped calls; gap within the row 1 target |
| 6 | Failed restart | The worker refuses the restart offer; the target relay drops checks | Planned: rollback, call kept. Unplanned: retry, then `relay_change_failed` within 15 s; never a frozen "connected" call |
| 7 | Decryption | Worker SRTP and SRTCP authentication failures; Chrome `inbound-rtp` decrypt errors where exposed; Pion caller counters | Zero in every row |
| 8 | No duplicate media | Caller duplicate RTP count (existing standby metric); per-slot forwarded-packet counters for the call after switch plus 500 ms | Zero duplicates at the caller; zero worker-to-caller packets through the old slot after the switch |
| 9 | Existing relay guarantees | ADR 0004 standby, ADR 0003 route restore, planned-move and crash gates with one slot | Unchanged, same thresholds |

**Chrome success signal.** A call has recovered when, after the switch, video
`framesDecoded` advances and audio `packetsReceived` and the RTP timestamp
advance in `inbound-rtp` stats. The driver also records the selected candidate
pair before and after (including the remote address), `packetsSent`, received
bytes and frames for 2 s after the switch, and the stall length. Pair changes
are evidence of the transition, not of success.

## Browser limits

- **Chrome:** tested this round, headless, with fake media.
- **Pion caller:** tested this round, in the harness and `crash-run`.
- **Safari:** later and untested. The page uses the same code, so a failed
  restart there ends the call explicitly (section 7).
- **Firefox and mobile browsers:** not tested and not claimed. Mobile stays with
  [#98](https://github.com/relaishq/relais/issues/98).
- **Third-party WHIP clients** without the event stream: no planned relay change
  and no unplanned recovery. Their calls drop on relay loss.
- **Consent:** the new path starts fresh consent with the new credentials. A
  restart must finish before the 30 s consent limit on the old path.

## Assumptions

- *Assumed:* Chrome keeps the DTLS association and SRTP keys across an ICE
  restart, keeps sending media and consent checks on the old selected pair until
  a new pair is selected, and keeps the old pair if the new generation fails.
  The Chrome gate checks this. If the last point is false, rollback is always a
  new generation on the old slot (already the fallback in section 7).
- *Assumed:* signaling and control run on a different host from the lost relay.
  A shared failure of control and the relay is out of the 5 s target.
- *Assumed:* the 5 s target needs a second relay slot that is already running.
- *Assumed:* the defaults (2 s relay-loss threshold, 500 ms receive grace,
  10 s planned deadline, 15 s hard deadline, drain concurrency 8) are starting
  values. Each is a documented parameter and is tuned from the measurements.

## Alternatives considered

- **Floating address (Elastic IP and a movable network interface, ADR 0011
  phase B).** It keeps the caller's address, so no client change. Rejected as the
  general mechanism: it depends on one cloud, keeps one Availability Zone as the
  failure domain, its cloud fencing time is not measured, and a relay started on
  demand at a new address cannot use it. An operator may still use it with the
  same-host standby.
- **Two relay candidates in every answer.** Rejected. After nomination, a
  standard controlling agent needs an ICE restart to switch pairs (RFC 8445
  section 8.1.1). It also ties every call to a second relay from the start, which
  blocks relays that start later and adds consent traffic.
- **UDP load balancer or anycast in front of relays.** Rejected. It carries no
  routes, holds or rings, and health routing does not move established flows
  (ADR 0011).
- **The worker sends a new offer (server-started restart).** Rejected. It needs a
  server-to-caller channel anyway, offer generation in the worker, and glare
  handling. A caller-started restart gets the same result with the standard
  browser call.
- **Stop answering checks to make the browser restart.** Rejected as the main
  trigger. It is slow, it looks like an outage, and the page needs restart code
  anyway. A stalled call still triggers a restart (section 4).
- **Caller polls for relay changes.** Rejected. The poll interval adds to the
  5 s budget, and idle polling adds load. One event stream per call costs less.
- **WebSocket for the event stream.** Not chosen. Server-Sent Events only need
  the server to push, and they go through the existing HTTPS listener and
  `relais-signal` proxy with no new dependency.
- **WHIP ICE restart with an `application/trickle-ice-sdpfrag` PATCH.** Not
  chosen for this round. The client must rebuild its remote description by
  editing SDP. A full new offer uses only standard browser calls. It may be added
  later for WHIP clients.
- **A new peer connection (full reconnect) as the main path.** Rejected. It
  repeats the DTLS handshake, changes SRTP keys and is visible to the
  application. It remains the application's choice after an explicit end.
- **Waiting for the old relay to stop before switching.** Rejected. A relay on
  another host cannot be fenced by ADR 0004, and a partitioned relay keeps
  forwarding. The worker fence does not need the old relay to stop.

## Consequences

- Control holds one event stream per capable call through `relais-signal`.
- Workers keep a leg to every slot. Relay set changes become a worker command.
- The session hash gains three fields. The relay owner lookup reads one more.
- Relays become replaceable and can start on demand, which #280 needs.
- Private status and metrics add, per slot: calls, relay changes started,
  switched, rolled back and failed, relay-loss decisions, and per-call switch
  time. Logs name the generation and both slots.
- The glossary gains "relay slot", "relay set" and "relay assignment" when this
  ADR is accepted.
