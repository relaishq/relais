# ADR 0016: Move live calls to a new relay address through ICE restart

Status: **Proposed** 2026-10-29. It stays Proposed until the owner reviews it.

**Amended 2026-11-01 (owner decision A+C).** Receiver reports give a fast
delivery signal; a capable caller reports decoded media through signaling.
The amendment defines completion in sections 5, 7 and 10.

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
- **Completion (A+C).** Worker receiver-report evidence (C) confirms packet
  delivery on the new path. In A mode, C is diagnostic; decoded-media proof for
  every required track (A) plus the fenced switch commit completes recovery.
  Other restart-capable callers use C for every required track plus that commit.
  A switch commit alone never completes recovery.
- **Planned change** (we retire a relay): keep the call through rollback. If
  rollback stays unconfirmed, an explicit end can occur after about 25 s. The
  caller gap target stays the same as for planned moves: pooled p95 ≤ 150 ms.
- **Unplanned relay-host loss:** every call recovers within **5 s**, with no
  dropped calls. This needs a second relay that is already running.
- **Fence.** The worker, which holds the media keys, is the fence. It sends a
  call's media through exactly one relay. A per-call relay generation in the
  store names which relays may carry the call, and it only goes up.
- **Fallback.** A planned change that does not finish rolls back to the old
  relay. An unplanned change retries on another relay. An unconfirmed rollback
  also enters stall recovery. One 15 s deadline, fixed at loss detection, covers
  every retry and stall restart in that episode. Without confirmation by that
  deadline, the call ends explicitly. No call waits forever in a frozen state.
- **Browsers this round:** real headless Chrome and the Pion caller. Safari is
  later and untested.
- **No feature flag.** Ordinary calls still work with one relay. A capable
  single-slot call can restart on that slot; changing to another relay address
  needs another slot.
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
   by its lease epoch, then reports the switch to control. The commit also
   records its time for section 7's confirmation bound. Control tells R1 to
   forget the call's routes with the existing tenure-checked cleanup.

The switch in step 6 does not wait for the store write: R2 is already
authorized by the pending assignment from step 1. A failed commit is retried. It
never moves the path back.

A worker move or takeover after the commit works as today: the snapshot carries
the current generation, its credentials and the path's slot. A snapshot never
carries pending credentials. If the owner dies during a change, the new owner
resumes on generation `g`. The caller's checks for `g + 1` then fail, and the
caller's own restart deadline (section 4) starts a new change. Control first
retires the attempted generation as in section 6; it never reuses it.

### 4. Triggers

| Trigger | When | What control does |
| --- | --- | --- |
| Planned: relay drain | Operator or management API: `POST /relays/{name}/drain` | Mark the slot draining (no new calls), then change each of its calls to another slot. Bounded concurrency (default 8 calls at a time). |
| Unplanned: relay-loss rule | The slot's ADR 0004 lease shows no renewal for **2 s** of reachable observation (the same rule as ADR 0004: failed store polls add no time) | Mark the slot lost (no new calls), remove it from every worker's relay set, and change all its calls to live slots at once. |
| Caller stall | The caller sees ICE `disconnected` or `failed`, or no inbound RTP or RTCP for 2 s while the call is live. During an unconfirmed A-mode attempt only, decoded output also fails to advance for 2 s on a required track while fresh RTP keeps arriving and the worker guarantees decodable media. | The caller calls `POST /calls/{id}/ice-restart` without an event. Control makes a new generation: on the same slot if it is live and untried in this episode (this also repairs a caller NAT change), otherwise on another live, untried slot. For an already committed but unconfirmed A attempt, use section 7's retry rule instead. The reply carries the generation, required MIDs and unchanged episode deadline (section 5). |

The 2 s relay-loss threshold is above the measured same-host standby takeover
(below 1 s, [guarantees](../guarantees.md#same-host-standby-relay)), so a
process crash with a standby normally recovers in place without a restart. A
wrong relay-loss decision is safe: it causes one extra relay change, not a lost
call. The caller-stall trigger covers a relay that renews its lease but has lost
its public network.

### 5. The event stream and caller media report

`relais-signal` and control add `GET /calls/{id}/events`, a long-lived
Server-Sent Events stream over the existing HTTPS listener, with the same bearer
authorization as `DELETE /calls/{id}`. The caller opens it right after `POST
/calls`. Events are JSON: `relay-change` (`generation`, `deadline`,
`required_mids`, `confirmation`) and `ended` (`reason`, `detail`). Confirmation
is `decode` for A mode or `delivery` for C-only mode. The deadline is an absolute
UTC time. The stream sends a comment line every 5 s so that proxies keep it
open. A caller reconnects with backoff and receives the current `relay-change`
again, including a committed generation still awaiting confirmation.

Required tracks are the negotiated audio and video MIDs on which the worker is
expected to send media for this change. Control and the worker fix the set
before switching, using negotiated directions and server-side media state.
The caller cannot omit a stalled track. Until confirmation, the worker must
keep required audio sending decodable media and required video sending at
least **4 frames/s**. For a silent source it supplies encoded silence (for
example Opus silence with DTX disabled); for paused video it supplies decodable
repeated still frames. Injected media fills gaps in caller output only. It
never replaces available source output and is never delivered to the agent.
Missing source input must not silently remove a required MID. The worker must
advertise support for this behaviour with its confirmation contract. This
requirement ends on confirmation, abort, supersession or call end. No required
track may rely on packet-loss concealment to pass A.

Every successful `POST /calls/{id}/ice-restart` reply keeps the
`application/sdp` answer and adds these response headers, including when the
caller requested the restart without an event:

| Header | Value |
| --- | --- |
| `Relais-Relay-Generation` | The assigned generation as a decimal integer. |
| `Relais-Relay-Deadline` | The same absolute UTC deadline as the event, in RFC 3339 format. |
| `Relais-Required-Mids` | The same MID set as a JSON array of strings. |
| `Relais-Relay-Confirmation` | `decode` or `delivery`, the call's accepted mode. |

`relais-signal` forwards these headers and exposes them to browser code. A
reissued event, answer or caller retry does not extend a deadline. When a
planned change becomes a stall episode, control reissues `relay-change` with
the episode deadline. If the current attempt has expired, first supersede it
with a higher generation under the abort or rollback rules. An unexpired
attempt may keep its generation, but its existing attempt bound cannot grow.
Every later generation shares that episode deadline until completion or end.

The create request has a `relay_change` capability field. Control only plans
relay changes for capable calls. A call without the capability is not moved by
a drain; the drain waits for it within its deadline and reports it. On relay
loss it ends with `relay_lost`.

The create request adds the boolean capability `media_progress`, alongside
`relay_change`. It is true only if the caller can report decode progress for
every required receive track. Control echoes the accepted values in the create
response and stores the accepted mode for the whole call (section 10).
`media_progress` requires `relay_change`; a missing or false value selects C
only. This includes older restart-capable callers and callers without a decoder.
The accepted mode never changes because A is missing. A single-slot set accepts
`media_progress: true` when signaling, control and the worker support the
contracts; caller-stall recovery can restart on the same slot.

A caller requesting A checks both capability echoes. If neither capability is
echoed as true, it keeps the call without relay change, as today, and reports
`relay_unsupported` to its application. Relay loss then ends that call with
`relay_lost`. If `relay_change: true` is echoed without `media_progress: true`,
the caller deletes the call with `DELETE /calls/{id}` and reports
`relay_unsupported`: that echo identifies a commit-only control. A requires
both echoes to be true. A new C-only call is allowed only under an explicit
application fallback policy and with known C support; that is a separate create.

**A freshness.** Let `t` be the time the caller has both applied the answer and
selected that generation's candidate pair. Discard decode observations through
`t + 500 ms`, matching the previous-path receive grace. Then take fresh
per-MID baselines and observe at least another **500 ms** of output while that
same pair remains selected. A MID qualifies only with sustained progress:

| Kind | Observation after the grace period |
| --- | --- |
| Audio | `inbound-rtp.totalSamplesReceived - inbound-rtp.concealedSamples`, or an equivalent non-concealed decoded-sample counter. Require at least 100 ms of new decoded samples at the track's sample rate, with positive increments in both halves of the observation window. `totalSamplesReceived` alone includes concealment and is insufficient. |
| Video | `inbound-rtp.framesDecoded`, or an equivalent actual decoder-output counter. Require increases at two observations separated by at least 500 ms after the fresh baseline; repeated rendering of one old frame does not count. |

These are minimum observation windows, not a new deadline. Reset all baselines
and observations if the generation or selected pair changes. For each MID,
`packetsReceived` must also increase in both halves of the observation window.
Receipt and decode must both continue after the grace period; old-pair
observations and buffered output alone cannot qualify. A caller unable to distinguish these observations
must declare C-only at create. This gate assumes old-path buffered media drains
within the grace period; a longer buffer needs a longer caller freshness window
or media provenance that distinguishes new-path output. As a tuning option,
the grace may follow the reported receive-buffer delay instead of a fixed
500 ms; this must not extend the attempt or episode deadline. The acceptance
tests must include a broken new return path with old media buffered and require
no A.
The current Pion test caller has no decoder and declares C-only. Depacketized
complete frames count as reception evidence, not decode. An A test with Pion
requires a real decoder and the same freshness checks.

A capable caller sends A through the existing authenticated signaling path:

| Item | Contract |
| --- | --- |
| Request | `POST /calls/{id}/media-progress`, `Content-Type: application/json`, through `relais-signal` to control. Same bearer authorization and call access checks as the event stream and `DELETE /calls/{id}`. |
| Body | `{"generation": 3, "decoded_mids": ["0", "1"]}`. Each MID has passed the freshness gate for that generation. Control binds the generation to its assigned slot; the caller supplies no slot or success flag. |
| Retention and completion | Control unions the reported MIDs for the matching pending or switched generation. It durably saves the set, required MIDs, generation, slot and attempt state before returning `204`, using the session-slot attempt/intent fence. All required MIDs plus the matching fenced switch commit complete the change. Whichever prerequisite is written last must atomically record the confirmed outcome too. Thus a saved full pre-commit report is consumed by the matching commit, including after restart. |
| Ordering and replies | Current or pending reports and duplicates return `204` only after durable retention. An already confirmed current generation also returns `204`. Stale, aborted, expired, superseded, future or unknown generations, and MIDs outside the required set, return `409` with no effect. Malformed bodies or a call without accepted `media_progress` return `400`. An ended call returns `410`. A failed or uncertain retention write returns `503`; no successful acknowledgement is sent. |
| Idempotency and pacing | Retain one cumulative MID set for the current attempt. The caller normally reports at most once per second. It may report immediately when the set grows, within the server burst. Control permits one request per second per call, with burst 2, and returns `429` with `Retry-After: 1` above that bound. The caller stops only after `204` for its full required set, or after supersession or end. A lost response is retried idempotently. `409` is never treated as success; the caller follows the current event or restart reply. |
| Size and time bounds | Body limit: 4 KiB; MID count and lengths are bounded by the negotiated session. Oversize bodies return `413`. Partial sets, write retries and repeated reports never extend the attempt or episode deadline. |

Control must hold authority before writing. The atomic write checks the
session-slot attempt/intent fence; it does not check a control lease in another
Redis Cluster slot. Control durably records completion before stopping the
watchdog. That transition is ordered against timeout and supersession. It must
occur before the applicable
deadline. On restart, saved full A plus a matching switch commit completes the
same attempt within its remaining deadline; A is not requested again. A timeout
or aborted generation cannot be reversed by a delayed write. This durable
acknowledgement contract needs no additional confirmation event.

A is the authenticated caller's assertion, not independent inspection by
control. Tests must check the declared evidence. A restart offer can disrupt a
call but cannot decrypt it: DTLS is not repeated, and the fingerprint check
stops a new certificate. This is the same power as `DELETE /calls/{id}`.

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
   slot again. An acknowledged abort consumes the attempted generation: clear
   pending, retain the old slot and advance the assignment counter to the
   aborted generation. The live ICE credentials may still be from the earlier
   generation. This retirement is not a switch commit and cannot confirm media.
   The next attempt uses a strictly higher generation, including after owner
   death. Delayed reports for an aborted generation are refused, never reused.
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

A worker's switch and fenced assignment commit (section 3, step 7) establish
the new path. They do not complete the relay change. A connected ICE state or
a new selected candidate pair does not count as success by itself.

**C: fast delivery signal.** After the switch, the owning worker reads the
caller's authenticated SRTCP receiver reports arriving through the new relay.
It uses the existing private worker-control link; there is no new caller
endpoint for C. For each required MID, the worker maps the report block's SSRC
to that track. The reported extended highest received sequence must cover RTP
sent only on the new path, beyond the last sequence sent on the old path
(with sequence wrap accounted for). An old report arriving late cannot satisfy
this test. Merely receiving RTCP, an empty report or a report for only another
track does not count.

The worker reports delivery per MID to control, with the call, current lease
epoch, generation and slot. Control checks these against the matching
**pending-or-committed** attempt before retaining C. It evaluates C for success
only after the matching switch commit. C for every required MID sets that
generation's `delivered` state to true. It proves packet reception, not decode.
Nonzero loss does not by itself fail C if the track has fresh reception
evidence. A track without that evidence remains unconfirmed. Audio alone
cannot confirm a change that also requires video.

Private status, per-call metrics and logs expose `generation`, `slot`,
`confirmation`, `delivered` and the delivered and decoded MID sets. Each new
generation starts with `delivered = false`; evidence from a previous attempt
cannot set it. A-mode delivery is diagnostic: it does not start, stop or reset
an attempt timer, and it does not cause a retry. At terminal failure, the
`relay_change_failed` reason has detail `delivered_decode_unconfirmed` when C
is complete but required A is missing, or `delivery_unconfirmed` when C is
incomplete. These describe confirmed evidence; a missing report is not proof
that packets were physically lost or that decode never occurred. The detail
also names the generation, slot, mode and missing MIDs.

**A: decoded-media proof.** For a call with accepted `media_progress`, only A
for every required MID, plus the matching fenced commit, completes the change
and ends the recovery watchdog. C remains diagnostic. For a restart-capable
call without `media_progress`, C for every required MID plus that commit
completes the change and ends the watchdog. This weaker result is recorded as
packet delivery, never as proven decode. Such callers can still have a decode
failure that C cannot detect. An empty required set confirms neither mode;
the same deadline and fallback apply.

| Signals for the committed generation | With `media_progress` | Without `media_progress` |
| --- | --- | --- |
| C for every required track; no complete A | Record `delivered = true`; wait for A until the attempt bound, then retry or roll back. The episode deadline still applies. | Complete on C; stop the watchdog. |
| A for every required track; missing or incomplete C | Complete on A; stop the watchdog. A does not wait for C. | A is not accepted. Wait for C. |
| Neither signal, or only some required tracks confirmed | Keep the watchdog active. Retry at the attempt bound within the remaining budget; end at the episode deadline. | Wait for C or a caller-stall restart. At the applicable deadline, use the fallback below. |
| C reports loss or no progress on one track | Complete only if A proves decode on every required track. Otherwise use the same A attempt bound. | Nonzero loss with fresh reception on every track can pass. A missing track cannot; keep waiting within the same deadline. |

**Unconfirmed A attempts.** Each switched A-mode generation has a **3 s**
confirmation bound from its fenced commit time, clipped by the active outer
deadline: the planned 10 s deadline before a loss episode, or the episode
deadline after it starts. Compute and persist that attempt bound once. Changing
from planned to recovery never extends an existing attempt; only a new, higher
generation gets a bound under the active episode deadline. The commit time
and bound are durable; repeated messages and takeover do not restart them.
No A by this bound fails that attempt even if RTP keeps arriving and C is
complete. In an unplanned or stall episode, retry with a higher generation on
a live, non-draining slot not yet tried in that episode. Record a slot as tried
when issuing its attempt, including pending attempts and C-only mode. When an
episode adopts an existing attempt, its target is already tried. Retain this
set across restart. Never clear it to retry an exhausted slot: wait for an
untried live slot or the fixed deadline. Thus a blocked public address is not
retried merely because its lease still renews.

A planned failure first rolls back to the original live slot with a higher
generation. A rollback uses the same attempt bound and active outer deadline.
Before a loss episode, retries stay on that original slot while it is live.
Once an episode starts, the tried-slot rule also applies to rollback. C-only
mode has no 3 s A timer. Missing C alone does not trigger that timer or its
retry rule.

During an unconfirmed A-mode attempt only, the caller requests a stall restart
when a required track has no non-concealed decoded audio or decoded video
progress for **2 s** while fresh RTP keeps arriving (section 4). The worker
guarantees decodable media during this period, including silent or paused
sources as section 5 requires. After confirmation, this decode-stall trigger
is disabled; ordinary Opus DTX pauses must not cause an ICE restart. Missing
transport progress still uses the existing stall trigger. A committed,
unconfirmed attempt uses the same retry rule. Control serializes these
requests with its attempt timer; neither parallel generations nor deadline
extensions are allowed.

**Confirmation fence.** Control matches C against pending-or-committed
attempt state and the worker lease, generation and slot. Success requires an
actual switch commit, not an abort that consumed a generation. The worker
discards previous-path reports, including the 500 ms receive grace, and resets
its per-track sequence boundaries for each switch. A is bound to its attempt's
generation, slot and required MIDs. Old, aborted or superseded generations and
old slots cannot complete the current attempt. Partial evidence never moves to
a higher generation. Durable completion, abort and deadline expiry are ordered;
completion must be recorded before the deadline, and no late report revives
an ended call.

**One loss episode, one deadline.** Control fixes the hard deadline at loss
detection plus **15 s**, before waiting for the per-call lock or store work.
For a caller-stall episode, detection is control's first accepted stall request.
A control crash before that detection time is saved may cause detection again
after restart and a later deadline. This pre-persistence window is accepted.
Once saved, all retries, new generations, caller stall restarts and waits for
A or C in that unfinished episode share the deadline. A switch commit, C-only
delivery for an A-capable caller, repeated events and control restart never
reset that saved deadline. Control restores the saved detection time and mode.
A planned change that remains switched but unconfirmed at its 10 s deadline
enters a stall episode then, unless an earlier loss or stall already set the
deadline. Reissue the current event with that episode deadline (section 5).
Without a crash before detection is saved, the worst case from the original
planned event is **10 s + 15 s = 25 s** before explicit end; earlier stall
detection shortens that bound. The **5 s** recovery target and section 9's
numbers remain unchanged.

| Case | Deadline | Fallback |
| --- | --- | --- |
| Planned change | 10 s from the event to confirmation, including the switch. Before switching, media continues on the old relay, so that wait is not a gap. A mode also has the clipped 3 s post-commit attempt bound. | Before switching, abort and consume the attempted generation; clear pending and delete its credentials. If the caller still uses the old pair, nothing else happens. After switching, or if the caller dropped the old pair, rollback uses a higher generation on the old live slot; never undo a committed generation. Confirm rollback under the same mode. Retry the drain later with backoff; after 3 failures report the call as not movable. Keep it while the old path still works. If still switched and unconfirmed at the planned deadline, rollback and retries share the stall episode's single 15 s deadline, preserving any earlier detection time. |
| Unplanned loss or caller-stall episode | 5 s target. One hard deadline 15 s from detection, below the 30 s consent limit (RFC 7675 section 5.1). A mode also has the clipped 3 s post-commit attempt bound. | Retry within the remaining time. Without required confirmation at the hard deadline, end with `relay_change_failed` and the delivery detail above, then send `ended`. This includes complete C but missing required A. The page shows an error and may start a new call. This is a dropped call and fails the gate. |
| Caller cannot restart, or worker cannot supply C | Immediate on loss or restart failure | Without `relay_change`, end with `relay_lost`. If restart throws, end with `relay_change_failed`. A relay-capable call whose worker lacks the C contract ends with `relay_change_failed`, detail `confirmation_unsupported`, including in C-only mode. It is not silently completed on commit. A planned drain instead leaves that call on the old slot and reports it as not movable. |

**C timing risk (unverified).** Browser audio receiver reports may be several
seconds apart. The reviewers' estimate of about 5 s with randomization has not
been measured or verified for the target browsers here. C-only confirmation can
therefore approach the 10 s planned bound even when media flows. Measure this
before tuning the defaults; do not relax the fixed loss-episode deadline.

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

Relay change is unreleased: `d5735a6` is not an ancestor of `v0.3.0` or
`v0.3.1`. No release ships relay change without this amendment.

The [#252 release manifest](https://github.com/relaishq/relais/issues/252) adds:

- the relay behaviour for `+<generation>` ufrags and assignment checks, as a
  raised relay contract version;
- the private worker API `restart` command and relay set updates;
- the session state format: live ICE generation, consumed assignment generation
  and path slot;
- the store schema fields `relay`, `relay_gen`, `relay_pending`,
  `relay_confirmation` and `relay_confirmation_state`. `relay_confirmation` is
  `none`, `delivery` or `decode`, accepted at create and fixed for the whole call.
  The bounded `relay_confirmation_state` record holds the current attempt's
  generation, slot, phase (pending, switched, expired, aborted or confirmed),
  required MIDs, durably acknowledged A set, delivered MID set, operation kind
  (planned or recovery), tried-slot set, commit time, attempt bound, planned
  deadline, first saved loss detection time and episode deadline.
  Supersession replaces the attempt fields and consumes the old generation,
  preserving the episode's tried-slot set and saved deadline;
- a worker-control contract for fenced per-track C, durable switch time and
  continued decodable media on required tracks until confirmation;
- a signaling contract for capability acceptance, restart response metadata,
  the event stream, `POST /calls/{id}/ice-restart` and
  `POST /calls/{id}/media-progress`;
- a control recovery contract for durable A acknowledgement and completion.
  A takeover restores saved A, mode, attempt state and original deadlines.
  It must not complete on commit alone or ask for A already acknowledged.

Preflight rules:

1. Control starts a relay change only when control, each slot and the owning
   worker support their required contracts, including C and confirmation media.
   An old worker that reports only commit is ineligible for a change. Ordinary
   single-slot calls without relay change still need no new contracts.
2. Signaling and control must support the signaling contract before accepting
   `media_progress: true`; the selected worker must support confirmation too.
   Otherwise create is refused as `relay_unsupported`. A single-slot set is
   sufficient when these checks pass. With neither capability echo, the caller
   keeps an ordinary call without relay change and reports `relay_unsupported`.
   With `relay_change: true` but no `media_progress: true`, it deletes the call
   as section 5 requires. There is no implicit C downgrade.
3. Calls created by the upgraded control without `media_progress` use persisted
   `delivery` mode if `relay_change` is true, otherwise `none`. At upgrade,
   existing relay-capable calls with no stored mode are assigned `delivery`
   before any further change; they do not remain commit-only. Calls without
   `relay_change` get `none`. This migration never invents A support. Completed
   historical commits are not relabelled as C proof. An unfinished episode keeps
   its original detection time and must collect fresh C within the remaining
   deadline. If its worker lacks C, apply section 7's unsupported-worker rule.
4. Before acknowledging any A, control must persist the union of MIDs fenced
   against supersession, abort and timeout. A restart restores it before
   processing reports, retries or timeout. The write that supplies the last
   prerequisite (full A or matching commit) must also record confirmation
   atomically. A full saved set awaiting commit is consumed by that commit; a
   durable confirmed outcome stays confirmed. If the deadline expired first,
   the call ends. Lost or unacknowledged A may be retried by the caller;
   acknowledged A never depends on another report. C evidence that was not
   durably saved is re-established by the current worker from fresh post-resume
   sequence boundaries and new receiver reports, within the original deadline. A worker takeover that
   aborts a pending change consumes that generation and requires new proof for
   the higher generation. An already valid saved confirmation is not discarded
   merely because control restarted.
5. A call with `relay_gen > 0` cannot move to a worker without the relay-change
   release. A call using this amendment cannot move to a worker without C and
   confirmation-media support or a control without durable acknowledgement.
   Its accepted mode, consumed generations and unfinished deadlines survive
   moves and takeover. An old signaling frontend without the A route or reply
   metadata cannot serve A-capable calls. Missing routes return an explicit
   error, never an acknowledgement.
6. Control or relays cannot roll back below the relay-change release while any
   call has `relay_gen > 0` or the set has more than one slot. Control and workers
   cannot roll back below this amendment while a call uses its confirmation
   contract or an episode is unfinished. Signaling cannot roll back while an
   A-capable call exists. Drain or end such calls first; commit-only completion
   cannot substitute for either confirmation mode.

## Measurement

Evidence comes from the in-process harness (Pion caller), `crash-run` and a new
headless Chrome driver with fake media. Every row reports p50, p95 and max per
call. Planned and unplanned results are reported separately. Report packet
delivery, decoded recovery and confirmation latency separately. Pion C-only
results do not establish the decoded-media target in section 9.

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

**Chrome success signal.** With `media_progress`, the driver applies section 5's
500 ms grace and sustained decode window on every required MID, then sends A.
Audio uses non-concealed sample progress; video uses new decoded frames. If a
caller cannot observe audio decode, it must use C mode and label the result as
packet delivery. Audio
`packetsReceived` and RTP timestamp progress alone do not prove decode. The
driver also records the selected candidate pair before and after (including the
remote address), `packetsSent`, received bytes and frames for 2 s after the
switch, and the stall length. Pair changes are evidence of the transition, not
of success. The Pion caller declares C-only until a real decoder is added; its
packet and complete-frame observations are not decode proof.

**Confirmation regressions.** Include a broken new return path with old buffered
media; concealment-only audio; a lost A reply; control restart after a full-set
`204`, before and after switch notification; superseded and aborted generations;
a silent source; and complete C with no A. Check the durable outcome, correct
retry slot, delivered detail and unchanged episode deadline in each case.

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

- Control holds one event stream per capable call through `relais-signal` and
  accepts authenticated decoded-media reports from A-capable callers.
- Workers report fresh receiver-report evidence per track. Control records
  packet delivery separately from decoded-media confirmation. A switch commit
  alone no longer completes recovery; old callers receive the weaker C result.
- Workers keep a leg to every slot. Relay set changes become a worker command.
- The session hash gains assignment and confirmation fields. Acknowledged A
  and completion require durable writes; a store failure cannot receive `204`.
  The relay owner lookup reads the assignment as before.
- Workers supply decodable silence or still frames on required tracks until
  confirmation. C-only browser report intervals need measurement before tuning.
- Relays become replaceable and can start on demand, which #280 needs.
- Private status and metrics add, per slot: calls, relay changes started,
  switched, delivered, decode-confirmed, rolled back and failed, relay-loss
  decisions, and per-call switch and confirmation times. Logs name the
  generation, both slots, the accepted mode, per-generation delivered state and
  missing MIDs. End details distinguish delivery confirmed without A from
  delivery still unconfirmed.
- The glossary gains "relay slot", "relay set" and "relay assignment" when this
  ADR is accepted.
