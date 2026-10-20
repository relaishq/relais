# Phase-2 guarantee matrix

This document records the evidence for [phase 2 (#31)][spec] and
[ticket #49][ticket]. It describes the tested conditions, not a production
service-level agreement. The project lead's phase-2 decision is **GO**, with
the known limits below, recorded on [#31][spec].

The main reference is commit `765db53`, [CI run 38197061923][main-run].
Capacity, soak and browser evidence have their own sources below. A passing
sample applies only to its stated workload and topology. Independent machines,
real cameras and sustained load cannot be inferred from namespace or loopback
tests.

The full 50-call, one-hour soak **passes** ([#85 result][soak-pass]):
`outcome=pass`, `acceptance=true`, 119/119 chaos events pass, and no audio unit
or video frame was lost. [#41][soak-ticket] and [#85][full-soak] are closed.
The first full soak **failed** ([#85 first result][soak-fail]): 9 of 432 planned
move recoveries and 1 of 900 takeovers lost one packet or frame per affected
track. The passing result names kernel UDP receive-buffer drops as the cause;
[PR #132][udp-buffer-pr] sizes the media socket buffers. A remaining
planned-move ordering race, which a test reproduced, is fixed by
[PR #146][move-fix-pr] ([ADR 0009][move-fix-adr]) when the relay buffer is on.
The estimated cost is **about $0.01–0.02 per call-hour** for
the echo/fixture workload ([#31 cost estimate][cost]).
Desktop Chrome with a real 720p camera **passes** with the relay buffer on and
off ([#45 result][device-result]). Mobile browsers are deferred to [#98][mobile].
Cloud confirmation is **Pass for the real-process driver** ([#48 result][cloud-result]).
The demo-page run with Derek's Chrome over the real internet is **accepted with
a stated limit**: its browser-side crash gaps were not measurable
([#48 browser result][cloud-browser]). The checkpoint-envelope row passes the
cited real-process store-stall sample ([#95][store-stall] is closed).
[PR #103][outage-pr] and [ADR 0008][outage-adr] add a bounded store-outage
guarantee. **#51 remains open**; **#127 is fixed by [PR #146][move-fix-pr]**;
**#94 is fixed by [PR #97][early-media-pr]**;
**#114 was a test timing assumption, fixed by [PR #143][second-crash-fix]**.

## How to read the matrix

- **Pass** means the cited sample met its acceptance rules. It does not mean
  that every deployment or every failure path has passed.
- **Fail/inconclusive** records a known failure or evidence that cannot prove
  the complete row. Missing measurements are written as **not measured**.
- A **planned move** transfers a live session to another media worker. A
  **takeover** restores a session after its worker crashes. A **drain** moves
  calls off a worker before it stops.
- The **relay buffer** keeps recent encrypted caller packets for replay. The
  **frame cache** keeps complete video pictures behind the worker. A cached
  picture is not proof of live video. See [ADR 0005][buffer-adr] and
  [frame-cache limits][frame-cache].

### Network profiles

These are emulated media paths on native Linux. The services use separate
network namespaces on one hosted machine. They are not separate cloud VMs.
Round-trip time (RTT) measures travel to the other endpoint and back.
The settings and link coverage come from [network-profiles.md][profiles].

| Profile | Caller ↔ relay | Private datacentre links |
| --- | --- | --- |
| `lan` | Unshaped, sub-millisecond | Unshaped |
| `region` | 20 ms RTT; 0.5% loss in each direction | About 0.5 ms RTT |
| `far-caller` | 150 ms RTT; up to 20 ms total jitter; 2% loss in each direction | About 0.5 ms RTT |
| `lossy-dc` | Unshaped | 1.5 ms RTT; up to 0.5 ms total jitter; about 0.1% one-way loss; 0.1% reordering per shaped stage |

The measured network matrix uses **one continuous call per trial**, with
**20 planned-move trials** or **10 takeover trials** per profile and a
**60-second consent observation**. Private Linux routes use
`rto_min 5ms quickack 1`, as required by [ADR 0007][tcp-adr]. This setting
applies to measured low-latency private routes. It is not a public-network
tuning rule. See [profile acceptance][profiles] and [topology limits][topology].

### Load and N+1 headroom

N+1 means the remaining workers can carry the calls after one worker dies.
The [capacity policy][capacity] derives a sample load from the demonstrated
worker floor C, relay floor R and worker count W:

```text
T = max(0, min((W - 1) * C - 1, R))
```

The extra free slot permits a planned move after the death. The final
[capacity run 38147888686][capacity-run] validated **T = 8 calls**, with
**C = 8**, **R = 8** and **W = 3**, on namespace LAN. Each step lasted
**30 seconds**. The audio/VP8 fixture, generators, services and Redis shared a
**four-vCPU Linux runner**. A saturated generator makes a step inconclusive.
See [capacity-results.md][capacity-results]. The full soak's capacity step
validated **T = 50 calls**, with **C = 50**, **R = 50** and **W = 3**, on one
**32-vCPU arm64 Linux VM** ([#85 result][soak-pass]). Its ramp stops at a
50-call cap ([#31 cost estimate][cost]). These are load floors, not maximum
capacities or an operating recommendation.

The one-call profile results and the LAN load results are separate experiments.
Moves and takeovers under `region`, `far-caller` or `lossy-dc` **at the validated
LAN load are not measured**. Relay continuity and agent browser trials also
do not establish loaded N+1 timing.

### Fixed-pool admission

A configured `-worker-call-cap` limits new calls and planned move/drain
reservations per worker. Concurrent placement and reservation settlement are
covered by control-plane race tests. A full ready pool returns 503
`at_capacity` with `Retry-After: 1`; no ready worker returns existing 409
`no_target`. Default 0 retains unlimited placement. A crash takeover may exceed
the cap rather than drop a live call; retained recovery of an exported planned
move uses that same rule. Telemetry counts those over-cap calls. Source rollback
slots protect placement without blocking unrelated drain or crash recovery.
The cap is not a media performance guarantee or automatic scaling policy.

The process bootstrap tests start control with a listed worker down and with
the relay down, then register those dependencies when they start. A healthy
worker can accept new calls while another listed worker remains down. This
proves tolerant fixed startup, not reconstruction of pre-restart call metadata.
Pool telemetry retains stale observations and represents unavailable demand
as null/omitted, rather than zero.

### Checkpoint-age envelope

A **checkpoint** is a successful session-store write. **Checkpoint age** starts
at that write. **Snapshot age** starts when the worker copied the state and
counters. Snapshot age includes time spent waiting for storage and is the
safety input. A periodic save is not an age guarantee.

[ADR 0002][checkpoint-adr] sets these default configuration bounds:

| Setting | Default |
| --- | --- |
| Snapshot-age envelope | 550 ms |
| Caller RTP source rate | 5,000 packets/s per stream (SSRC) |
| SRTCP rate | 100 packets/s |
| Safety factor | 1.25 |

Worker and control-plane settings must match. Callers must obey the configured
source-rate bound, including sequence advance for packets lost before the worker.
The store clock must remain stable. A takeover beyond the envelope widens its
counter margins if they still fit the safe sequence budget. Otherwise it ends
the call cleanly and counts the loss. This policy protects decryption; it does
not promise that a stale call survives.

Once every worker runs this build, crash takeover skips 64 DTLS record sequences
beyond the committed next sequence; an outgoing guard, armed at the first
checkpoint attempt and advanced only by acknowledged writes, prevents reuse of a
sent epoch/sequence pair in encrypted DTLS epochs. Plaintext epoch zero is exempt.
This guarantee assumes the store never loses an acknowledged write: unlike the
age-scaled SRTP margins, the fixed DTLS reservation cannot self-correct after an
asynchronous replica failover. [ADR 0011](adr/0011-deploy-and-autoscale.md) already
requires a durable store for ownership safety.

A confirmed live export for a planned move uses no crash margin. An uncertain
export or adoption uses the fenced checkpoint and its measured ages. Relay
restart alone keeps the live workers and their media state; it does not restore
a worker checkpoint. A concurrent worker failure still needs this envelope.

## Planned move

**Guarantee:** Under the tested conditions, successful planned moves keep
audio and video gap p95 within the same-run baseline plus 50 ms and at or below
150 ms, without reconnecting the caller.

**Conditions:** The four profiles above; the harness audio/VP8 source; one call
per profile trial; the relay buffer on; private-route TCP tuning on. N+1 load
has separate evidence: a short LAN sample at eight calls and the 50-call full
soak. A confirmed export transfers
the latest accepted state; uncertain recovery needs the checkpoint envelope.

The numbers below are caller gaps, not control-plane operation durations.
All values are milliseconds. The [final #37 result comment][network-result]
reports them from main run 38197061923.

| Profile | Audio move p95 | Audio baseline p95 | Video move p95 | Video baseline p95 | Status |
| --- | ---: | ---: | ---: | ---: | --- |
| `lan` | 20.9 | 20.7 | 34.2 | 34.0 | Pass |
| `region` | 29.0 | 20.8 | 34.0 | 33.8 | Pass |
| `far-caller` | 52.4 | 40.1 | 70.1 | 55.7 | Pass |
| `lossy-dc` | 35.0 | 20.9 | 35.3 | 34.1 | Pass |

The [profile policy][profiles] also requires every move trial to stay below
2 seconds. Known integrity or connection failures in any trial fail the run,
even if p95 passes. Moves record content loss but do not gate on zero content
loss. On lossy links, a passing move is not a promise that the network loses
no packets.

Browser samples use the camera's own frame cadence, so video gaps are larger
than in the harness:

| Browser sample | Moves | Audio gap | Video gap | Source |
| --- | --- | --- | --- | --- |
| Desktop Chrome, 1280×720 camera, buffer on | 10/10 pass | 0 ms max | 80 / 92 / 92 ms median / p95 / max | [#45 result][device-result] |
| Desktop Chrome, 1280×720 camera, buffer off | 10/10 pass | 0 ms max | 79 / 92 / 92 ms median / p95 / max | [#45 result][device-result] |
| Desktop Chrome over the real internet, buffer on | 7 pass, 3 inconclusive | 0 ms on every move | 85–106 ms; median 94.5 ms | [#48 browser result][cloud-browser] |

In #45, the camera's no-event video gaps were **75–91 ms**, so move gaps stayed
within camera cadence. The three #48 inconclusive moves had **102–106 ms** gaps
that overlapped camera frame starvation; the page limit is **100 ms**.

The [passing full soak][soak-pass] ran **424 planned call moves** (moves and
drains) at **50 calls**. Audio pause was **p50 / p99 / max 20 / 29 / 31 ms**;
video pause was **34 / 43 / 44 ms**. **No move lost content.** The
[first full soak][soak-fail] had lost content in **9 of 432** move recoveries:
one audio packet or video frame per affected track, both tracks in two
([loss rows][soak-gist]). That loss did not recur after
[PR #132][udp-buffer-pr] sized the UDP socket buffers ([#127][move-loss]).
This guarantee bounds the gap. The [profile policy][profiles] does not gate
moves on raw content loss. Zero content loss is **observed in the passing
soak**. With the relay buffer on, regression tests also show zero loss when
private-leg packets are reordered around the move ([PR #146][move-fix-pr]).
The full soak has not been rerun since that fix.

**Status:** **Pass for the cited profile matrix, short LAN load sample, 50-call
full soak [#85][soak-pass], real-process cloud confirmation [#48][cloud-result]
and desktop Chrome real camera [#45][device-result].** The #48 browser run is
accepted with a stated limit. Mobile browsers are deferred to #98. **#127's ordering
race is fixed** by [PR #146][move-fix-pr] when the relay buffer is on; with the
buffer off it remains a documented limit. **#51 remains a known exception
on aborted/rolled-back moves**;
the successful-move sample does not resolve it.

## Crash takeover with the relay buffer

**Guarantee:** With a safe checkpoint and complete retained caller media,
a worker crash resumes the call without losing outage content or waiting for
a new keyframe; on lossy paths, acceptance checks for loss beyond the network's own.

**Conditions:** Buffer on; confirmed caller route; matching checkpoint settings;
caller rates within the envelope; sufficient ring and replay-hold space.
[ADR 0005][buffer-adr] defaults the ring to **1.5 seconds**, **1 MiB per
session**, **64 MiB total** and **16 streams per session**. Byte limits can
evict packets before the time limit. Default worker failure detection remains
**400 ms**; faster detection has not been accepted.

Profile timing is measured at one call per trial. Loaded recovery is separately
validated on short LAN steps at eight calls with N+1 and in the 50-call full
soak. The separate real-process
cloud confirmation passes [#48][cloud-result]. The browser run over the real
internet could not measure crash gaps; see [Cloud confirmation](#cloud-confirmation).

| Profile | Audio takeover gap p95 | Video takeover gap p95 | Content-loss rule | Status |
| --- | ---: | ---: | --- | --- |
| `lan` | 423 ms | 423 ms | Trusted zero lost content | Pass |
| `region` | 449 ms | 463 ms | No excess over network baseline | Pass |
| `far-caller` | 446 ms | 447 ms | No excess over network baseline | Pass |
| `lossy-dc` | 460 ms | 473 ms | No excess over network baseline | Pass |

Source: [final #37 results][network-result], main run 38197061923. The
**2-second ceiling checks every takeover trial**, not just the displayed p95.
Lossy acceptance uses pooled, same-call baseline windows and a per-trial burst
guard. Its statistical bound has at least **95% combined one-sided confidence**
under independent, stationary content loss. It cannot detect every small
additional loss. Raw loss remains visible. See [the exact policy][profiles].

The separate [main buffered process job][buffer-job] passed **10/10** SIGKILL
trials, each with the full consent observation. Its `BUFFER_VS_BASELINE` table
and `BUFFER_PAUSE_MEDIAN` summary show:

| Measure | Main buffered-process result |
| --- | --- |
| Caller pause range | 377.7–446.2 ms |
| Median pause | 413.4 ms; phase-1 reference 416 ms; difference −2.6 ms |
| Lost audio / video | 0 ms / 0 frames in every trial |
| Decryption failures | 0 in every trial |
| First new decoded content | Outage media in every trial |
| Keyframe requests | 0 in every trial |
| Audio / video freshness recovery | At most 445.3 ms / 445.3 ms; strict recovery gate passed |

The [main strict harness job][harness-job] also passed buffer-on and buffer-off
controls on Memory and Redis. Buffer-off controls lost **340 ms of audio and
10 video frames** and returned keyframe content first. Buffer-on controls lost
**0 audio and 0 video** and returned outage media first. These are harness
measurements, not a browser audio-playback comparison.

The [#45 desktop Chrome run][device-result] is the browser comparison. It used a
**1280×720, 30 fps** camera, a same-Mac local cluster (relay, control and three
workers as separate processes) and ten SIGKILL takeovers per mode. The
[saved records][device-records] give these per-kill values, rounded to the
nearest millisecond:

| Measure | Buffer on | Buffer off |
| --- | --- | --- |
| Takeovers passed (2 s bound) | 10/10 | 10/10 |
| Video gap median / max | 391 / 450 ms | 448 / 506 ms |
| Audio gap | 0 ms in 5 kills; 41 ms in 1; 218–354 ms in 4 | 354–441 ms in every kill |
| First live frame after the kill, median / max | 434 / 521 ms | 494 / 536 ms |
| 60 s hold; reconnects / renegotiations / ICE restarts | Pass; 0 / 0 / 0 | Pass; 0 / 0 / 0 |

First live frame starts at the browser's kill request, so it is an upper bound.
Chrome does not expose SRTP decryption failures; both records mark them
`unverified`. Checkpoint ages are **not measured** in these records. The first
four buffered kills still had **218–354 ms** audio gaps. Their cause is **not
known**. The #48 browser run could not re-check them, because its crash gaps
were unverified. Every kill still passed the page's verdict.

In the [passing full soak][soak-pass], **893 call takeovers** after SIGKILL at
**50 calls** had audio pause **p50 / p99 / max 414 / 494 / 515 ms** and video
pause **421 / 503 / 529 ms**. **No takeover lost content.** Failure detection
took **p50 384 ms, max 434 ms**, most of the pause. In the
[first full soak][soak-fail], one of 900 takeovers lost one video frame
([#127][move-loss]).

**Status:** **Pass within the cited buffer, checkpoint, profile and load
conditions, including the 50-call full soak [#85][soak-pass], the real-process
cloud confirmation [#48][cloud-result] and desktop Chrome real camera
[#45][device-result].** The #48 browser run
completed **10/10** takeovers with **0 lost calls**, but its browser-side crash
gaps are unverified. Mobile playback is deferred to #98. The first soak's
single video-frame loss did not recur after [PR #132][udp-buffer-pr].
[#114][second-crash-flake] was a test timing assumption, not a product
failure: under heavy CPU starvation a harness test built an unusually old
checkpoint, which the takeover correctly refused. [PR #143][second-crash-fix]
fixes the test.
Overflow, expiry, relay-buffer loss and terminal adoption
failure can lose content or end a call. They have counted fallback/cleanup
paths, not a universal no-loss guarantee. [PR #77][buffer-pr] and
[ADR 0005][buffer-adr] record those failure paths. The incomplete-ring fix
[#82][incomplete-ring] preserves retained media before requesting a keyframe;
[#84][recovery-flags] preserves recovery flags across confirmation retries.

## Relay restart

**Guarantee:** With restorable confirmed routes, restarting the relay keeps
the measured caller gap below one second without reconnecting the call.

**Conditions:** Same public and private-leg addresses; persisted routes enabled;
store available for route restore; live workers and control plane; one fixture
call on a same-host loopback topology. Load at N+1, shaped profiles and large
route tables are **not measured** for this timing. Worker state stays live, so
the checkpoint envelope is not exercised unless a worker also fails.

The [main restart job][restart-job] passed **10/10 restore-on trials**. Its final
`RELAY_RESTART mode=restore-on` table reports **39.8–140.2 ms** caller gaps,
**0 reconnects**, **0 renegotiations**, **0 decryption failures**, and **one
restored route per trial**. Its restore-off control reports **1,899.7–1,967.6 ms**
gaps with the harness caller's **2-second** keepalive. A passing restore-off
control is not a pass of the under-one-second product threshold.

[ADR 0003][routes-adr] restores each route from the current session owner,
not a saved worker address. It preserves the original consent deadline. Writes
are best effort. A failed write, partial scan or expired route can leave a
caller waiting for its next authenticated check. The measured timing does not
apply to those missing routes.

**Status:** **Pass for restore-on, single-call loopback evidence.** Loaded,
shaped and cloud relay-restart gaps are not measured.

## Same-host standby relay

**Guarantee:** In the tested same-host topology, a standby replaces a dead or
paused relay within a caller gap below one second and fences the old process
before forwarding.

**Conditions:** Active and standby share a stable relay lease key on the same
Linux or macOS host and PID scope; their process identities are verifiable;
they can terminate one another; acknowledged store lease decisions are retained;
addresses and ports stay fixed; routes restore successfully. No shared-port
forwarding is used. The timing sample is one loopback fixture call on Linux.
Loaded N+1 timing and shaped profiles are **not measured**. Worker state stays
live; simultaneous worker failure still needs the checkpoint envelope.

| Main standby job | Trials | Caller gap range | Reconnects | Decryption failures | Duplicate media |
| --- | ---: | ---: | ---: | ---: | ---: |
| Active SIGKILL | 10/10 pass | 100.0–166.8 ms | 0 | 0 | 0 |
| Active SIGSTOP | 10/10 pass | 467.3–700.8 ms | 0 | 0 | 0 |

Source: [main standby job][standby-job], final `RELAY_STANDBY` tables. Every
trial reports the old process dead before the replacement is ready. The paused
process cannot return after the driver attempts resume. [ADR 0004][standby-adr]
defines the identity, fencing and exclusive-socket checks.

Without a successful store lease decision, the standby neither signals nor
binds. The active can keep forwarding existing routes while Redis is
unreachable. Safety under store outage does not promise continued availability
after the active also dies.

**Status:** **Pass for the cited same-host sample and fencing tests.**
Cross-host relay failover is excluded. Loaded, shaped and cloud standby timing
are not measured.

## Store outages

**Guarantee:** With live worker, relay and control-plane processes, established
calls retain their owner, keys and counters through a store outage within the
configured budget. The default budget is **30 seconds**. The tested live calls
stay connected, with caller gaps below **one second** and no standby takeover.
A worker crash still needs safe checkpoint margins. Unsafe checkpoints and
expired ownership end as counted losses instead of unsafe resumes.

**Conditions:** Worker, control plane and relay upgraded together; matching
worker/control budgets and sufficient lease TTL; retained acknowledged Redis
decisions; stable store clock; continued caller consent and media; bounded host
scheduling delay. [ADR 0008][outage-adr] retains leases and snapshots for
**34 seconds** by default: the budget plus **four seconds** of renewal, command
and recovery headroom. This does not widen [ADR 0002's][checkpoint-adr]
cryptographic envelope. New calls, moves, takeovers and hang-ups need the store;
they can wait or fail during an outage.

The [PR #103 CI store-stall job][outage-job] ran on `a363b60`, with separate
processes on one Linux host, one fixture call per trial and the driver's own
Redis paused by SIGSTOP. The table uses its `STORE_STALL` and
`STORE_STALL_CALLER` records. Local results and regression tests are labelled
separately; these samples do not establish loaded or shaped-profile timing.

| Scenario | Guarantee and measured result | Status and source |
| --- | --- | --- |
| Live worker; 5, 15 and 29 s store pauses | Same call and ownership survive. 5/5 at each duration; caller gaps 34.5–39.9 ms across all live trials; zero losses, decryption failures, reconnects or renegotiations. The standby stays waiting. | **Pass in CI sample**; [store-stall job][outage-job] |
| Worker crash; 1.5 s store pause | 5/5 resume with scaled margins. Snapshot age 1.985–1.988 s; RTP margin 12,475–12,493; SRTCP margin 266; caller gaps 420.4–460.6 ms. Each counts one envelope event, with zero losses, decryption failures or reconnects. | **Pass in CI sample**; [store-stall job][outage-job] |
| Worker crash; 5 s store pause | 5/5 counted definitive losses, each `checkpoint_unsafe` and one envelope event. Snapshot age 5.485–5.488 s; required RTP margin 34,344–34,362 exceeds the 22,702 cap. No resume margins spent; zero decryption failures or reconnects. | **Pass of safe-loss policy**; [store-stall job][outage-job] |
| Live worker; 35 s pause and actual ownership expiry | 5/5 count one `ownership_expired` loss each, with zero decryption failures and no resume. Checkpoint age is **not measured** because ownership and snapshot are gone; no checkpoint-envelope event is invented. Local cleanup was confirmed 0.6–0.75 s after recovery, with no later media. | **Pass of expiry policy**; [CI accounting][outage-job]; [PR #103 local cleanup][outage-pr] |
| Worker partitioned from its store adapter while ownership transfers | Memory and Redis regression tests move epoch 1→2 before expiry. None of 40 deliberately stale ciphertext packets from the old worker reach the caller; successor media continues. The old worker fences when access returns. Full-budget transport-partition survival is **not measured**. | **Pass of exclusivity test**; [PR #103][outage-pr]; [ADR 0008][outage-adr] |

The budget is not an exact termination timer. Headroom can allow longer pauses.
Forwarding can continue while Redis is unavailable even after ownership expires;
fencing and terminal accounting occur when authoritative access returns.
[PR #101][expiry-pr] counts actual absence once and cleans only the recorded
owner/epoch. A committed **five-minute ended marker** distinguishes a released
call from expired ownership. Failed release followed by expiry remains ambiguous
and counts as `ownership_expired`. Drain-timeout and agent-failure releases can
still count as normal ends; the marker proves release, not its reason.

[PR #105][outage-followup-pr] closes #102's uncounted move-time expiry path and
#104's excessive standby staleness credit after an unobserved host stall.
[ADR 0004][standby-adr] still requires a store claim, verified predecessor
fencing and exclusive sockets before forwarding. Store flapping has no fixed
standby takeover bound.

Persisted routes can expire after roughly **25–28 seconds** without a successful
store write. A later consent check republishes them, about **two seconds** in
the driver or **five seconds** in a browser. Relay restart inside that window
retains ADR 0003's partial-restore limits. The budget does not extend consent.
Source: [ADR 0008][outage-adr].

**Status:** **Pass for the cited pause, accounting and exclusivity samples.**
The CI job explicitly reports **60-second post-takeover consent acceptance
NOT-RUN**. Full-budget worker transport partitions, Redis failover with a hung
renewal, control-plane or relay restart during the outage, lost store data,
loaded/profile timing and cloud survival are **not measured**. The headroom
calculation is a configuration bound, not evidence for those outage types.

## Agent continuity

**Guarantee:** A compatible audio agent restores its accepted state and consumed
input position exactly on a successful planned move, and restores its checkpoint
on takeover with measured state age and a warning when input may repeat.

**Conditions:** Matching agent factory and supported state version on every
worker; bounded queues; callbacks honor their deadlines; valid checkpoint and
relay replay conditions. The demo is deterministic, audio-only and uses encoded
Opus clips without an external service. Profile-by-load or N+1 agent timing is
**not measured**. The browser evidence is a single call in the local demo.

V2 worker-local SIGTERM drain waits for task handlers only **when the operator
sets a measured `-agent-drain-move-reserve`**. Unset skips the handler wait; the
placement mark always applies, followed by urgent export for every v2 session.
One shutdown deadline includes marking placement,
the concurrent handler wait, moves, and waiting for sessions to leave. The wait
never includes buffered playout: its backlog/cursor move exactly on a successful
planned export. Unfinished handlers keep Retry/Abandon and typed outcomes.
A handler that paces speech by emitting small chunks remains in flight: the
host admits one emission per task per 100 ms, including the terminal result.
A SIGTERM during Demo A's chunked `Play` speech can therefore use the whole task
budget before urgent move, even though the playback queue itself is not awaited.
The accepted prefix moves and `Play` resumes above `After`.
Heartbeats/private APIs stay live; direct control drain stays urgent. See
[the guide](build-an-agent.md#sigterm-handler-quiescence) and
[one-machine measurements](dev/drain-quiescence.md). These measurements add no
capacity, provider, public-network, mixed-version, side-effect transaction, or
seamless crash guarantee. Crash loss of move-only state remains unchanged.

Legacy agents keep [ADR 0006][agent-adr]'s **8 KiB** default state size, a **32-packet** input
queue, **100 ms** callback deadlines and **100 ms** minimum changed-state write
interval. Queue overflow and callback timeout can drop input and count it. A
planned export drains accepted work within its budget and can count dropped
remaining input. Exact continuation means the accepted state/progress pair
continues; it is not a promise to process every sent packet.

The [#31 browser result comment][agent-result] reports **seven passing events**:
**three moves and one drain** restored exactly; **three kills** restored
checkpoints **372–460 ms** old; the **60-second hold** passed. The reported
recording includes a takeover from a **445 ms** checkpoint. This document cites
Derek's result comment; it does not independently inspect the private recording
or claim that it is publicly available.

State rollback uses **snapshot age**, not write age alone. The demo verdict
allows snapshot age plus **one 20 ms input packet**, as documented in the
[demo agent-continuity rules](demo.md#agent-continuity-verdicts). Replay can repeat work after the saved
input floor. [ADR 0005][buffer-adr] supplies duplicate input ranges and the
conservative duplication window before callbacks start. Applications must
make external side effects safe to repeat themselves.

**Status:** **Pass for the reported local Chrome run and cited hosting/replay
contract tests.** #94's startup packet loss is fixed by [PR #97][early-media-pr].
The #45 records ran in echo mode, with the agent verdict `inconclusive`
([records][device-records]). The #48 results report no agent evidence. Agent
continuity with a real camera or over the real internet is **not measured**.
Mobile browsers are deferred to #98. No universal no-loss application or
exactly-once side-effect guarantee is established.

V2 custom agents can produce task-emitted Opus output without caller input.
The bounded host, pacing, planned continuation and move-only crash-loss rules are
specified in [ADR 0010](adr/0010-pluggable-agents.md). Pools must use the same
registration and restore prior app state and pending task schemas. Restoration
uses the persisted agent name, task kinds and declared limits (including zeros
for host defaults), and refuses mismatches without an Echo fallback. Effective
limits are computed on each worker from local operator ceilings. Restored content
must fit those limits; an oversized adoption refuses with typed capacity, without
truncation. Crash-lost app data and audio do not consume target payload limits;
takeover reports the existing typed loss instead. Older experimental v8 envelopes retain their original bounds check.
Public registration does not
establish provider integration or custom-worker capacity approval. The manual
capacity, Save-CPU and store-stall gates remain pending. The legacy default 1 KiB app state
cannot preserve normal conversation history across a crash.

Demo A also has a default-budget split-checkpoint process proof:
[`TestDemoAHistoryProcessSIGKILL`](../examples/voice-agent/history_process_test.go)
starts an owned standalone local Redis and real Demo A worker subprocesses with
`-agent-split-checkpoints`, default working/history limits and no enlarged
state/store flags. Four exchanges reach a confirmed checkpoint. SIGKILL of only
the owner is followed by takeover: the replacement's actual Restore callback
receives byte-identical history, its next provider request contains the four
exchanges, and the deterministic reply uses their text. Recording, provider-wait
and playback cuts preserve the committed text, report `OutcomeUnknown` and a
Resumed warning, and clear lost audio without producing replacement audio.
A source-only store gate separately proves that a newer locally accepted
history update does not survive when it never reaches the checkpoint store.

The real Save size test measures **2,871 B working / 4,205 B history**, below
**3,072 B / 8,192 B** defaults, including escaped and non-ASCII bounded text.
This is a mock-provider proof on standalone local Redis, not a live paid-provider,
managed-store, loaded-capacity or seamless-audio proof. A crash can lose current
recording, buffered speech and unfinished provider work; provider work can
repeat. Only committed text is claimed. Demo A retains four exchanges and
deliberately evicts the oldest on the next completed reply. Legacy sessions keep
the original v4 inline layout and their existing operator overrides. See
[the worked example](build-an-agent.md#run-demo-a).

V2 snapshots have operator byte and rate limits. Ordinary callback state overflow
restores prior app state and produces a typed `Capacity` notice. Required transport
state which cannot fit refuses adoption or ends the call with explicit capacity
evidence. Failed store writes consume no byte tokens and do not produce `Capacity`.
Emit backpressures with cancellation; its first chunk is immediate and later chunks
and handler terminals share one 100 ms publication interval per task. A terminal
can wait up to 100 ms; a move during that wait follows Retry/Abandon or reports
`OutcomeUnknown`. The byte rate must cover periodic maximum-size snapshots plus
one maximum-size forced write per second; burst must cover two maximum snapshots.
Wall-clock rollback never subtracts tokens and refill resumes from the new clock.
Planned export carries the current remaining quota without waiting for store I/O.
An in-flight snapshot conservatively reserves one maximum snapshot in the exported
quota, floored at zero; failed writes still never debit the live bucket. The
worker-local drain flags above bound the optional handler wait. The raw move
memory flag can tighten the supported 1 MiB per-session ceiling; larger payloads
need a new wire-budget admission design before they can be enabled safely.

## Capacity and soak

**Capacity guarantee:** The cited short LAN sample recovered after one worker
death and completed a subsequent planned move at its validated eight-call load
without exceeding the demonstrated worker floor. The full soak's capacity step
validated its one-worker-down check at 50 calls on a 32-vCPU arm64 Linux VM.

**Conditions:** The workload, hosts and N+1 formula in the load section;
buffer on; safe checkpoint envelope; unsaturated generator. Other network
profiles at these loads are **not measured**. The full soak below supplies the
sustained 50-call evidence.

| Final capacity observation | Result | Source |
| --- | --- | --- |
| Worker floor | 8 calls; 16-call step inconclusive due to generator saturation | [Capacity results][capacity-results] |
| Relay floor | 8 calls; 1,370.7 forwarded datagrams/s; 16-call step inconclusive | [Capacity results][capacity-results] |
| N+1 death, then move | Pass at 8 calls across 3 workers | [Capacity results][capacity-results] |
| Loaded drain | 44 ms at 8 total calls | [Capacity results][capacity-results] |
| Store request/reply use | About 67 client commands and 85 KB per call-second | [Capacity results][capacity-results] |
| Store budget | 100 commands and 128 KiB per call-second, window averages | [Capacity policy][capacity] |
| Full-soak capacity, 32-vCPU arm64 Linux VM (c7g.8xlarge class) | Worker floor 50, relay floor 50, W = 3; one-worker-down check validated at 50 calls with no manual overrides. The first attempt read a relay floor of 16 because of a port race ([#141][port-race]); a rerun on the same VM gave 50 | [#85 passing result][soak-pass] |
| Store request/reply use at 50 calls | About 73–82 commands and 90–97 KB per call-second; the passing soak measured 81.5 commands and 97.3 KB | [#31 cost estimate][cost] |
| Maximum relay packet rate | Not measured; the ramps stop at the generator limit (four-vCPU runner) and at a 50-call cap (Linux VM) | [Capacity results][capacity-results]; [#31 cost estimate][cost] |
| Cost per call-hour, echo/fixture workload | About **$0.01–0.02**. Whole machines at validated T = 50: **$0.0172**. Measured resources only: **$0.0099**. Transfer, about **$0.0092**, dominates. Upper-bound estimate at the demonstrated load floor, from public on-demand list prices | [#31 cost estimate][cost] |

**Status:** **Pass for the short LAN sample and the 50-call capacity check.**
Store traffic at 50 calls fits the budget. Redis CPU binds first, at about
0.0024 vCPU per call. With 50% headroom, plan for **about 200 calls per Redis
primary, not 500**.
Redis shared the host with every other role, so a dedicated-Redis run may show
less CPU. The cost is an estimate, not a bill; the capacity tool's own cost
field stays `NOT-RUN` because the run supplied no prices. Agent-state workload
cost is not isolated by this echo/fixture capacity sample.

**Split checkpoint observation (2026-10-20):** On one four-vCPU GitHub-hosted
runner, two calls using a keyless four-exchange history fixture consumed
95,855 B of Redis request+reply per call-second with split defaults, versus
184,520 B with the legacy 8 KiB operator override. The split sample passed the
128 KiB store-traffic gate; the legacy sample exceeded it. Split N+1, planned
drain and rolling master-key rotation ended no calls and reported zero
decryption failures. At the rotation inventory check, all ten live component
references used the new key. Both sampled checkpoint and snapshot age p99
values stayed below 550 ms. These are short, standalone-Redis loopback
observations at two calls, not capacity or managed-store evidence. The
asynchronous fixture does not establish strict audio continuity: its raw audio
loss/freshness exceptions remain in the
[dated results](dev/capacity-results.md#2026-10-20--matched-split-checkpoint-traffic-run-39441746025).
The planning limit of about 200 calls per Redis primary above is unchanged.

**Soak guarantee sought:** Continuous calls remain within the move and takeover
rules through repeated moves, drains and kills, with no unintended lost calls,
decryption failures or leaked resources and no sustained memory growth.

**Conditions:** At least **50 calls for one hour**, an event
every **30 seconds**, hardware-specific capacity floors with N+1, and the
same checkpoint envelope. The [soak policy][soak] requires `outcome=pass` and
`acceptance=true`. It permits at most **1%** saturated wall-clock intervals and
requires at least **95%** conclusive chaos events, including each event kind.
It keeps all exclusions visible. Memory needs adequate per-process observation;
product process memory and heap remain gates, while callers gate on live heap.
Cleanup permits documented, bounded key retention; it does not require deletion
of persistent fencing metadata. See [soak.md][soak] and
[caller-memory.md][caller-memory].

The [short hosted run 38162635098][soak-run] used **six calls for 10m10s**,
**three workers**, LAN, and worker/relay floors of **eight**. It passed **20/20
events**, with **0% saturation**, **0 decryption failures**, **0 lost calls**
and bounded cleanup. Adequately observed memory rows passed. Caller live-heap
growth projected **2.8–3.4 MiB/hour** against a **16 MiB/hour** limit in this
sample. `acceptance=false`: it does not prove the full soak.
Source: [soak-results.md][soak-results].

A spare Mac could not carry the full soak: its relay floor was **R = 4**
because its generator saturated ([#85 Mac result][soak-mac]). The
[passing full run][soak-pass] used a temporary **32-vCPU arm64 Linux VM**
(c7g.8xlarge class) with Ubuntu 24.04 and Go 1.26.9, at `f779ec3`. One
generator process carried **16 callers**; the soak used **25 processes with 2
callers each**. Capacity gave **C = 50** and **R = 50**, and the playbook table
gave **W = 3**, with no manual overrides. The run had **50 calls for one
hour**, chaos every **30 seconds** with seed **41**, and **119 events**.

| Full-run check | Result |
| --- | --- |
| Verdict | `outcome=pass`, `acceptance=true` |
| Events | 119/119 conclusive and passing: 40 moves, 40 drains, 39 SIGKILLs |
| Generator saturation | 0 of 3,600 one-second intervals; budget 1% |
| Decryption failures after resume | 0 |
| Lost calls | 0 |
| Whole-call delivery | 9,000,000 audio units and 5,400,000 video frames sent; 0 lost, 0 unsettled; 50/50 calls pass |
| Kernel UDP receive-buffer errors | 0 for IPv4 and IPv6; softnet drops 0; sampled every second |
| Memory | Pass for relay, control, Redis, the sentinel worker and all 25 caller processes. Chaos restarts the other two workers, so the sentinel carries the worker verdict |
| Leaks at the end | 0 sessions in relay, control and workers; Redis keys only in bounded classes; no owned processes or listeners |
| Planned moves and drains (424 call moves) | Audio pause p50 / p99 / max 20 / 29 / 31 ms; video 34 / 43 / 44 ms; 0 lost |
| SIGKILL takeovers (893 call takeovers) | Audio pause p50 / p99 / max 414 / 494 / 515 ms; video 421 / 503 / 529 ms; 0 lost; failure detection p50 384 ms, max 434 ms |

Source: [#85 passing result][soak-pass] and the [#41 closing comment][soak-close].
The report's raw nested `run.outcome` reads `inconclusive`: one caller process
had one 20.1 ms sample over the 20 ms lateness limit. The soak judge tolerates
an isolated sample under twice the limit, so its saturation is 0%.
[#138][report-fields] tracks the mismatch.

The [first full run][soak-fail], at `b4fe991`, **failed**: 9 of 432 move
recoveries and 1 of 900 takeovers lost content ([#127][move-loss]). It also
hit two judge bugs ([#128][judge-bugs]), fixed by [PR #129][judge-pr]. A
[rerun at `e5e11cc`][soak-rerun], after [PR #132][udp-buffer-pr], had 0 kernel
UDP drops and 0 content loss but failed on takeover freshness and generator
stalls. The passing run also includes #133/#137, #134/#135 and #140.

**Status:** **Pass for the full soak ([#85][soak-pass]); [#41][soak-ticket] and
[#85][full-soak] are closed.** This is one one-hour run of the echo/fixture
workload on one Linux VM, with all roles, Redis and the generators on the same
host. Separate hosts, shaped network profiles and other workloads at this load
are **not measured**.

## Cloud confirmation

The [official #48 result][cloud-result] is **Pass for the real-process driver** for
planned moves and crash takeovers. Three internet callers ran on GitHub runners,
each with three audio/video calls, against **separate cloud VMs in one region**.
This is **one 75-minute run**, with **one judged caller (three calls)**.

Before any fault, the judged caller was pre-registered by measured clock-probe
round trip **≤10 ms**. The two far callers were diagnostics only: a far caller's
own measurements cannot fail the run. **Server-side failure signals count on
every call.** Host clock bounds were evidenced from chrony: **2 ms for control**
and **3 ms for the worker**.

The judged results are pooled over the three calls. All times are milliseconds.

| Measure | Planned moves | Crash takeovers |
| --- | ---: | ---: |
| Trials | 25 | 30 |
| Media resume p50 / max | 17 / 30 | 386 / 443 |
| Event gap max | 38 | 464 |
| Lost audio / video frames | 0 / 0 | 0 / 0 |
| Decryption failures after resume | 0 | 0 |

Every judged trial passed; **315/315 yardstick rows** passed. This sample does
not establish results for other callers or repeated runs. [#125][history-watermark],
the history watermark in a call's first retention window, is a **harness limit
worked around by scheduling**. [#122][caller-ice] (caller ICE checks) remains open.

The [demo-page run][cloud-browser] used Derek's desktop Chrome over the real
internet against the same separate-VM topology, with the relay buffer on.
Media went straight to the relay's public UDP port. Derek accepted it with a
stated limit; its own stage verdict is **INCONCLUSIVE** because of the
measurement method.

| Measure | Demo-page result |
| --- | --- |
| Camera | 1280×720 at 30 fps |
| Planned moves (10) | 0 ms audio gap on every move; video gap 85–106 ms, median 94.5 ms; 7 pass, 3 inconclusive |
| Crash takeovers (10) | 10/10 completed; control `lost_count=0`; detection about 390 ms after SIGKILL; takeover 7–9 ms |
| Call continuity | Connected throughout; no renegotiation or ICE restart; 60 s hold pass |
| Browser-side crash gaps | **Unverified** |
| Decryption after resume | **Not measured** for this run |

Each cloud crash action took about **20 seconds** through a remote-command
path. The SIGKILL landed about **5.5 seconds** after the click. The page assumes
an immediate kill, so every crash gap is `unverified`. Workers do not journal
resumes or decryption counters, and Chrome does not expose caller SRTP
decryption failures. #45 covers the browser crash measurement locally; the
real-process run covers internet crash timing and decryption. A freeze Derek
saw about six minutes into the call was the stage's planned teardown after the
results upload ([#48 clarification][cloud-teardown]).

**Status:** **Pass for the cited real-process driver sample.** The demo-page
run is **accepted with a stated limit**: moves are 7 pass and 3 inconclusive,
and browser-side crash gaps are unverified. [#48][cloud] is closed. The settled cost
figure is still to be posted on #48. The full soak passes separately; mobile
browsers stay deferred.

## Every threshold in the phase-2 spec

This table preserves all rows of [#31's threshold table][spec]. A pass is
limited to the cited conditions. The project lead's disposition: #51 is published
as a known limit. #127 was published as a known limit and is now fixed by
[PR #146][move-fix-pr]. #114 was classified as a test timing
assumption and fixed by [PR #143][second-crash-fix].

| Spec threshold | Status | Evidence or what the blocking run must show |
| --- | --- | --- |
| Takeover with relay buffer (`lan`): no lost content or keyframe wait; pause reported against phase 1; freshness back to baseline within 2 s | **Pass** | [Main buffered process job][buffer-job]: 10/10, median 413.4 ms versus 416 ms; zero loss; outage media first; freshness recovered. [Strict harness controls][harness-job] pass on both stores. |
| Planned move under `region` and `far-caller`: each track p95 ≤ baseline + 50 ms and ≤150 ms; zero decryption failures; no reconnect | **Pass in final sample** | [Final #37 matrix][network-result]. #51 remains a known integrity failure on an uncertain-reply rollback outside a successful planned move. This row does not gate content loss. The first soak's move content loss did not recur after [PR #132][udp-buffer-pr]. The remaining ordering race ([#127][move-loss]) is fixed by [PR #146][move-fix-pr] when the relay buffer is on. |
| Takeover under `region` and `far-caller`: pause <2 s; no lost content with buffer; zero decryption failures; no reconnect | **Pass under the implemented lossy-link policy** | [Final #37 matrix][network-result]. [Profile rules][profiles] judge loss against the network baseline and burst guard; exact zero loss applies only to lossless media paths. |
| Relay restart or standby: gap <1 s; no reconnect; pause → takeover → resume never creates two forwarders | **Pass** | [Main restart][restart-job] restore-on and [standby][standby-job] SIGKILL/SIGSTOP tables pass; old process dead before new readiness; zero duplicate media. Single-call loopback scope. |
| Planned load with N+1, including after forced worker death: move/takeover rules hold; drain time reported | **Pass for short LAN sample and 50-call full soak** | [Final capacity evidence][capacity-results]: validated T=8, death then move pass, drain 44 ms. Shaped-profile load and full consent at that load are not measured. [Passing full soak][soak-pass]: N+1 capacity validated at 50 calls with W=3 and no manual overrides; 424 call moves (moves and drains) and 893 takeovers lost 0 content; audio pause max 31 ms (moves) and 515 ms (takeovers). The [first full soak][soak-fail] lost one packet or frame per affected track in 9 of 432 move recoveries and 1 of 900 takeovers; that loss did not recur after [PR #132][udp-buffer-pr]. The planned-move ordering race ([#127][move-loss]) is fixed by [PR #146][move-fix-pr]. [#114][second-crash-flake], a harness second-crash failure under CPU starvation, was a test timing assumption ([PR #143][second-crash-fix]). |
| Checkpoint-age envelope: a store stall beyond it never produces undecryptable takeover; policy counted and documented | **Pass for cited real-process sample** | [PR #103 CI job][outage-job]: moderate crash 5/5 resumes with scaled margins and one envelope event each; long crash 5/5 counts `checkpoint_unsafe` with one envelope event each and no margins spent; live pauses pass 5/5 at each tested duration. Zero decryption failures. [Main harness][harness-job] supplies both-store failed/stalled/delayed-write controls. [ADR 0002][checkpoint-adr] and [ADR 0008][outage-adr] define the policy. Separate 60 s consent acceptance is NOT-RUN. |
| Soak: ≥50 calls for ≥1 h with chaos; zero decryption failures/unintended lost calls; flat memory; no leaked sessions, keys or processes | **Pass** | [#85 passing result][soak-pass] at `f779ec3`: 50 calls for 1 h, chaos every 30 s, `outcome=pass`, `acceptance=true`. 119/119 events pass; 0 of 3,600 intervals saturated; 0 decryption failures; 0 lost calls; 0 of 9,000,000 audio units and 0 of 5,400,000 video frames lost; memory passes for every judged process; no leaked sessions, unbounded keys, processes or listeners. One run on one Linux VM. The [first full run][soak-fail] failed on #127 content losses and two judge bugs ([#128][judge-bugs], fixed by [PR #129][judge-pr]). |
| Real camera at 720p in desktop Chrome: planned-move and takeover rows hold | **Pass** | [#45 result][device-result] and [records][device-records]: 1280×720 at 30 fps; buffer on and off each pass 10/10 moves, 10/10 takeovers and the 60 s hold. Takeover video gap max 450 ms (on) and 506 ms (off). First live frame median 434 ms (on) and 494 ms (off). Caller decryption failures are `unverified` in Chrome. |
| Android Chrome on local network: planned-move and takeover rows hold | **Deferred; explicit non-guarantee** | [#45 scope change][device-ticket] defers mobile browsers to [#98][mobile]. Results not measured. |
| Agent continuity: exact planned continuation; bounded checkpoint rollback on takeover; duplicated input flagged | **Pass for cited local demo and harness contract** | [Browser report][agent-result], [ADR 0006 hosting/replay tests][agent-adr] and [ADR 0005 notices][buffer-adr]. State-age interpretation uses snapshot age. [PR #97][early-media-pr] fixes #94's pre-move early-media loss. Real-camera and real-internet agent runs are not measured. |
| Cloud confirmation: move/takeover rows hold over real internet | **Pass for real-process driver; browser accepted with a stated limit** | [Official #48 result][cloud-result]: one judged caller (3 calls), one 75-minute run; 25 moves and 30 takeovers pass, with zero lost audio/video and zero decryption failures after resume; 315/315 yardstick rows pass. [Demo-page run][cloud-browser]: 7 moves pass and 3 are inconclusive from camera starvation; 10/10 takeovers complete with 0 lost calls, but browser crash gaps are unverified. |

## Open residuals and source differences

### #51: caller decryption failure after rollback

[The reproduced failure][rollback-hit] occurred in run **38174220164**,
`lossy-dc` move trial **15**, after an injected lost resume reply and rollback.
The caller counted **three authentication-tag failures and zero replay
failures**; workers counted **zero decryption failures**. Media continued and
later moves were clean. The cause remains unknown; no root-cause regression or
fix is established.

[The latest #51 comment][rollback-counts] records **one failing rollback in
about 210 untraced rollbacks**, versus **zero in 521 traced rollbacks**. The
later [#31 network summary][phase-result] reports about **230** untraced
rollbacks. The sources report different totals, not a stable rate or reliability
bound. Of the traced trials, **300** used short
observations and explicitly did not run the **60-second consent acceptance**.
Tracing may change timing. [PR #93][diagnostics-pr] enables tracing on alternate
nights for the nightly `lossy-dc` move lane; failures retain caller records and
worker logs. Monitoring remains active. A passing
matrix does not remove this exception to a blanket zero-decryption-failure claim.

**Disposition:** known limit, monitored nightly.

### #127: resolved planned-move ordering race

[Issue #127][move-loss] is closed. It began with content loss in the
[first full soak][soak-fail]: **9 of 432** planned move recoveries (2.1%) and
**one of 900** takeovers lost one audio packet or one video frame per affected
track. The [loss rows][soak-gist] show that two move recoveries lost both. The
pause stayed at one frame interval, and every call stayed connected.

**The soak loss: no longer seen.** A 20-minute diagnostic soak on a temporary
32-vCPU arm64 Linux VM counted **1,133** kernel `UdpRcvbufErrors`, about
**1,080** on worker sockets. The move losses fell in the same seconds as these
drop bursts ([#127 findings][move-loss-findings]). Worker sockets set no buffer
size, so each used the **208 KiB** default. [PR #132][udp-buffer-pr] (merged,
part of #127) sizes the relay, worker and caller UDP buffers and reads back what
the kernel grants. The relay and worker log a warning when the kernel clamps a
request. The [deployment guide](deploy.md#udp-socket-buffers) documents the recommended
`rmem_max`/`wmem_max` values. The
[rerun after PR #132][soak-rerun] counted **0** kernel drops in 65 million
datagrams and lost content in **0 of about 1,350** recoveries. The
[passing full soak][soak-pass] also had **0** kernel UDP receive-buffer errors
and **0** lost audio units or video frames. It names the buffer drops as the
root cause of the soak loss.

**The ordering race: fixed.** Before the fix, the drain marker's acknowledgement
proves only what the old worker received before the marker. It does not prove
that every packet the relay sent before the marker arrived. A caller datagram
that reaches the old worker after the marker is discarded once the session is
exported; planned moves do not replay it. The opt-in test
`TestPlannedMoveReorderedPrivateInput`, merged with PR #132, reorders packets on
the private leg: a loopback proxy delays one ciphertext datagram. With the race
detector, 20 runs per case gave:

| Case | Lost units |
| --- | --- |
| Ordered move | 0 |
| Delayed datagram, with a move | Exactly 1 audio unit; 0 held packets, worker no-flow drops and decryption failures |
| Delayed datagram, no move | 0 |

The passing soak did not show this race.

[PR #146][move-fix-pr] fixes it as described in [ADR 0009][move-fix-adr]. The
old worker's export now records which recent caller packets it consumed. When
the relay releases the move hold, it first sends the retained packets that the
export did not consume, then the held queue. The new worker restores the
receive window, so SRTP replay protection rejects duplicates. A second drain
marker on the return path keeps the old worker's last echoes. The ADR's
evidence section records the results:

| Test | Result |
| --- | --- |
| `TestPlannedMoveReorderedPrivateInput`, now a regular test | 20/20 with the race detector; zero loss in supported cases; the older-relay and buffer-off subtests confirm the loss limit |
| 10-call drain stress, reordering around every marker, race detector | 6/6 runs; 0 lost units; 105–136 late packets recovered per run; 3 runs wrapped sequence numbers |
| Same with 20 calls, no race detector | 8/8 runs; 0 lost units; 191–273 recovered per run; in 3 runs all 40 streams wrapped |
| Full CI matrix at PR #146's final head `54f699b` | [Run 38706006218][move-fix-run]: all executed jobs pass; two jobs skipped |

The added cost is small; the ADR records the move, drain and release timings.
The fix needs the relay buffer, which is on by default. With the buffer off
(`-buffer-off`), a late inbound datagram is still lost. Reordering on the
worker→relay leg, ring eviction, late SRTCP or STUN, and rollbacks with crash
margins are not recovered. The full soak has not been rerun since the fix.

**Disposition:** fixed by PR #146 when the relay buffer is on. The buffer-off
case is a documented limit.

### #114: resolved second-crash test timing

[Issue #114][second-crash-flake] is closed. The test
`TestRelayBufferFailurePaths/memory/second-crash-after` failed under CPU
contention. Either the control plane's own `/status` counted the second
takeover as `Lost`, or the takeover did not appear within **3 seconds**.

The [classification][second-crash-class] found a **test timing assumption**,
not a product failure. The test blocked worker B's checkpoints and slowed relay
replay about 80 times, so B's only checkpoint was old. Under heavy CPU
starvation on GitHub-hosted runners, the first replay stretched from about
0.3 s to 1.1–1.3 s. At a snapshot age of about **1.51 s** or more, the takeover
correctly refused the checkpoint as `checkpoint_unsafe` under
[ADR 0002][checkpoint-adr]. Otherwise the 1.5 s takeover budget expired, and a
retry completed the takeover at 3.0–3.6 s. Without CPU burners, **244 of 244**
runs passed.

[PR #143][second-crash-fix] uses the default replay pacing for this path and
waits for B to decrypt input before the second kill. The assertions are
unchanged. The fix passed **300 of 300** runs at 16–48 burners in the
classification, and **60 of 60** on x64 and on arm at 32 burners.

The `second-crash-during` path keeps the slow pacing on purpose. Under extreme
starvation it can still miss its 3 s window ([#144][second-crash-during]).
None of those failures reported `Lost`.

### #94: resolved early-media startup loss

[Issue #94][agent-flake] is closed by [PR #97][early-media-pr]. Caller media
could arrive before the worker installed SRTP keys and was silently dropped.
Early SRTP/SRTCP packets now wait in a **64-packet** queue and drain in order
when the keys are ready. Overflow drops the oldest packet and counts it; this
is bounded startup protection, not unlimited no-loss input.

With a forced **50 ms** key-installation delay, the old code failed **3/3** on
each store with the original **34 written, 33 consumed, InputDrops 0** signature.
The fix passed **50/50** on Memory and **50/50** on Redis under the race detector,
then **20/20** per store after review simplification. Real UDP tests cover
ordering, overflow, SRTCP replay rejection and resumed sessions. Source:
[PR #97][early-media-pr]. Natural failure frequency remains **not measured**.
This closes #94's residual; it does not fix #51.

### Differences that affect interpretation

| Sources | Difference | Interpretation used here |
| --- | --- | --- |
| [#31 spec][spec] / [latest summary][phase-result] and [profile policy][profiles] | Spec says “no lost content” on lossy networks; the summary also uses that shorthand. The implemented gate permits network loss within a statistical bound. | Exact zero loss only on lossless media paths; no measured excess over baseline on lossy paths. |
| [#31 spec][spec] and [ADR 0002][checkpoint-adr] / [ADR 0006][agent-adr] | Spec describes state staleness using checkpoint write age. ADRs distinguish snapshot-copy age, which can be older. | Use snapshot age for recovery safety and application rollback; report both ages. |
| [README][readme] and [frame-cache documentation][frame-cache] / [ADR 0005][buffer-adr] | Earlier README claims of process-local caching and no input replay are corrected. | Redis frame storage and agent input replay follow current ADR contracts and scoped result evidence. |
| [#31 agent spec][spec] and [ADR 0006][agent-adr] | Spec asks for decoded audio. The implemented interface accepts encoded Opus; a resumable decoded-audio adapter is deferred. | No decoded-audio or portable codec-state guarantee. |
| [#37 earlier result][old-network-result] and [final result][network-result] | Earlier lossy-DC moves failed; the final tuned main matrix passes. | Final result supersedes the earlier matrix for these conditions. Untuned private-route performance is not accepted by the final sample. |
| [PR #53][checkpoint-pr] and [ADR 0002][checkpoint-adr] | Earlier PR describes an RTP send limiter; current ADR measures caller source sequence rate without a send limiter. | Current ADR governs the caller input bound; no claim that a send limiter enforces it. |
| [#95 closing comment][stall-close] / [PR #101][expiry-pr] and [PR #103][outage-pr] / [ADR 0008][outage-adr] | Before the longer retention, a 5 s crash or live stall counted `ownership_expired`. With the budget, the crash counts `checkpoint_unsafe` and the live call survives. | Use the PR #103 sample for the new budget. Older results prove the earlier accounting fix. |
| [#31 store summary][store-result] and [PR #103][outage-pr] / [ADR 0008][outage-adr] / [PR #105][outage-followup-pr] | Summary still lists #102/#104 as follow-ups; both are now closed by PR #105. “Over 30 s” omits retention headroom; its “before” row for all outages under 30 s omits the old 3 s expiry boundary. | Follow the merged fixes and ADRs. Actual ownership expiry, not the budget boundary alone, triggers `ownership_expired`. |
| [Latest #51 comment][rollback-counts] and [#31 network summary][phase-result] | About 210 versus about 230 untraced rollbacks; both report one failing rollback and zero failures in 521 traced rollbacks. | Keep both sourced totals and their observation scope. No inferred failure rate. |
| [#45 title/older playbook][device-ticket] and its current scope / [#98][mobile] | The title and playbook still include Android. The current issue body defers mobile browsers. | Desktop Chrome only for phase 2; Android Chrome and iOS Safari are explicit non-guarantees. |
| [#45 result][device-result] / [#31 device summary][device-summary] and the [#45 records][device-records] | The summaries say the buffer removed the takeover audio gap in 6 of 10 kills. The per-kill list and records show 0 ms in 5 kills and a 40.875 ms gap in one. | Use the per-kill record values, rounded to the nearest millisecond: 5 zero-gap kills, one 41 ms gap and four 218–354 ms gaps. |
| [#48 browser result][cloud-browser] and the [#45 records][device-records] | #48 cites #45 takeover video as “about 390–506 ms”. The records span 330.8–506.3 ms; 390.75 ms is the buffer-on median. | Use the #45 record values, rounded to the nearest millisecond: 331–506 ms, buffer-on median 391 ms. |
| [#127][move-loss] and the [profile policy][profiles] / this matrix | #127 says the guarantee promises that a planned move loses no media. The profile policy does not gate moves on raw content loss, and the move guarantee here bounds the gap. | Lossless planned moves are a phase goal and a soak gate. The [passing soak][soak-pass] met it, and [PR #146][move-fix-pr] fixes the ordering race when the relay buffer is on. The move guarantee still bounds the gap; zero loss rests on the cited soak and regression tests, not on the profile policy. |
| [#41 verdict][soak-verdict] / [#31 soak summary][soak-summary] and [#85 first result][soak-fail] / [#127][move-loss] | The summaries say moves lost “one packet”; #85 says each lost “exactly one packet or frame”. The [loss rows][soak-gist] show two move recoveries that lost one audio packet **and** one video frame. | Nine move recoveries lost content; each affected track lost one packet or frame, and two recoveries lost both. |
| [#31 soak row][spec] and [soak policy][soak] / [#85 first result][soak-fail] | The first run met the row's listed checks for decryption, lost calls, memory and sessions. The soak policy also fails on event rows and saturation. | The soak policy governs. The first run failed it; the [passing run][soak-pass] meets both the row and the policy. |
| [#85 passing result][soak-pass] and [#138][report-fields] | The soak verdict is `pass`, but the report's raw nested `run.outcome` reads `inconclusive` because of one 20.1 ms generator sample. | The soak verdict governs; its judge tolerates an isolated sample under twice the limit. The nested field is a known report mismatch. |

## Explicit non-guarantees

| Excluded claim | Source |
| --- | --- |
| Cross-host relay failover, floating public IP, anycast, or multiple relays across hosts | [#31 out of scope][spec]; [ADR 0004][standby-adr] |
| Safari or data channels | [#31 out of scope and guarantee-matrix section][spec] |
| Mobile browsers (Android Chrome, iOS Safari) | [#45 scope change][device-ticket]; deferred to [#98][mobile] |
| Multi-region operation, Kubernetes or production orchestration | [#31 out of scope][spec] |
| Multi-party rooms, fan-out, simulcast or SVC | [#31 out of scope][spec] |
| TURN, TCP or TLS **media fallback** | [#31 out of scope][spec]. Private HTTP/Redis TCP tuning is a separate matter. |
| Production store durability, Redis failover/backup operations, or automatic relaunch of failed standbys | [#31][spec]; [session-store limits][session-store]; [ADR 0004][standby-adr] |
| Control/private API authentication beyond private addresses and firewalls; a proven public signaling front | [#31][spec]; [topology signaling limit][topology] |
| Unconditional route protection after failed/dropped persistence or partial restoration; protection from stolen session credentials or compromised workers | [ADR 0001][stickiness-adr]; [ADR 0003][routes-adr] |
| No-loss recovery after buffer loss, overflow, expiry, agent queue overflow or unsafe checkpoint loss; exactly-once application side effects | [ADR 0002][checkpoint-adr]; [ADR 0005][buffer-adr]; [ADR 0006][agent-adr] |
| Lossless planned moves with the relay buffer off, under worker→relay reordering, or after ring eviction | [ADR 0009][move-fix-adr] |
| Video input for agents, validated resumable PCM decoding, or speech/LLM-provider integration | [#31][spec]; [ADR 0006][agent-adr] |
| Recording, SIP, MoQ or GPU transforms | [#31 out of scope][spec] |
| Windows standby fencing, general mixed-version rollout compatibility, or fixed global Redis frame-cache memory | [ADR 0004][standby-adr]; [ADR 0006 compatibility rules][agent-adr]; [frame-cache limits][frame-cache] |

## Evidence to add when runs arrive

- **#127:** The ordering-race fix is proven by regression tests and CI. The
  next full soak should confirm zero move loss with it.
- **#45 buffered audio gaps:** The cause of the four 218–354 ms buffered audio
  gaps is not known, and no issue tracks it. A later browser crash run with
  verified kill timing can re-check it.
- **#48:** Both parts are closed. Add the settled cost figure when #48 posts it.
  The cost per call-hour above is an estimate from public list prices, not a bill.
- **Store outages:** Add full-budget transport-partition or Redis-failover
  evidence if those cases are run. Keep loaded/profile timing and the separate
  consent acceptance unmeasured until sourced results arrive.
- Keep #51 open until a trace identifies the cause and a regression covers the
  fix. #94 is resolved by PR #97. Publication belongs to the project lead and
  orchestrator.

[spec]: https://github.com/relaishq/relais/issues/31
[ticket]: https://github.com/relaishq/relais/issues/49
[main-run]: https://github.com/relaishq/relais/actions/runs/38197061923
[network-result]: https://github.com/relaishq/relais/issues/37#issuecomment-6117260128
[phase-result]: https://github.com/relaishq/relais/issues/31#issuecomment-6117260681
[old-network-result]: https://github.com/relaishq/relais/issues/37#issuecomment-6113305852
[buffer-job]: https://github.com/relaishq/relais/actions/runs/38197061923/job/114641974951
[harness-job]: https://github.com/relaishq/relais/actions/runs/38197061923/job/114641974831
[restart-job]: https://github.com/relaishq/relais/actions/runs/38197061923/job/114641974877
[standby-job]: https://github.com/relaishq/relais/actions/runs/38197061923/job/114641974908
[capacity-run]: https://github.com/relaishq/relais/actions/runs/38147888686
[soak-run]: https://github.com/relaishq/relais/actions/runs/38162635098
[agent-result]: https://github.com/relaishq/relais/issues/31#issuecomment-6108153828
[buffer-pr]: https://github.com/relaishq/relais/pull/77
[checkpoint-pr]: https://github.com/relaishq/relais/pull/53
[incomplete-ring]: https://github.com/relaishq/relais/issues/82
[recovery-flags]: https://github.com/relaishq/relais/issues/84
[rollback-hit]: https://github.com/relaishq/relais/issues/51#issuecomment-6114094148
[rollback-counts]: https://github.com/relaishq/relais/issues/51#issuecomment-6116570763
[diagnostics-pr]: https://github.com/relaishq/relais/pull/93
[agent-flake]: https://github.com/relaishq/relais/issues/94
[early-media-pr]: https://github.com/relaishq/relais/pull/97
[cloud]: https://github.com/relaishq/relais/issues/48
[cloud-result]: https://github.com/relaishq/relais/issues/48#issuecomment-6158341764
[cloud-browser]: https://github.com/relaishq/relais/issues/48#issuecomment-6166965901
[cloud-teardown]: https://github.com/relaishq/relais/issues/48#issuecomment-6166974978
[device-result]: https://github.com/relaishq/relais/issues/45#issuecomment-6162503619
[device-records]: https://gist.github.com/Derek-X-Wang/16b752e167d73b3b952ccf56605e50c9
[device-summary]: https://github.com/relaishq/relais/issues/31#issuecomment-6162504981
[soak-fail]: https://github.com/relaishq/relais/issues/85#issuecomment-6169232937
[soak-rerun]: https://github.com/relaishq/relais/issues/85#issuecomment-6178387175
[soak-pass]: https://github.com/relaishq/relais/issues/85#issuecomment-6181268134
[soak-close]: https://github.com/relaishq/relais/issues/41#issuecomment-6181270151
[cost]: https://github.com/relaishq/relais/issues/31#issuecomment-6181288660
[soak-gist]: https://gist.github.com/Derek-X-Wang/00e92ca2f8f9787bdc64c1d545a1c87c
[soak-mac]: https://github.com/relaishq/relais/issues/85#issuecomment-6167483622
[soak-verdict]: https://github.com/relaishq/relais/issues/41#issuecomment-6169235072
[soak-summary]: https://github.com/relaishq/relais/issues/31#issuecomment-6169235593
[move-loss]: https://github.com/relaishq/relais/issues/127
[move-loss-findings]: https://github.com/relaishq/relais/issues/127#issuecomment-6176025694
[second-crash-flake]: https://github.com/relaishq/relais/issues/114
[second-crash-class]: https://github.com/relaishq/relais/issues/114#issuecomment-6183382466
[second-crash-fix]: https://github.com/relaishq/relais/pull/143
[second-crash-during]: https://github.com/relaishq/relais/issues/144
[move-fix-pr]: https://github.com/relaishq/relais/pull/146
[move-fix-run]: https://github.com/relaishq/relais/actions/runs/38706006218
[judge-bugs]: https://github.com/relaishq/relais/issues/128
[judge-pr]: https://github.com/relaishq/relais/pull/129
[udp-buffer-pr]: https://github.com/relaishq/relais/pull/132
[report-fields]: https://github.com/relaishq/relais/issues/138#issuecomment-6181274730
[port-race]: https://github.com/relaishq/relais/issues/141
[history-watermark]: https://github.com/relaishq/relais/issues/125
[caller-ice]: https://github.com/relaishq/relais/issues/122
[soak-ticket]: https://github.com/relaishq/relais/issues/41
[full-soak]: https://github.com/relaishq/relais/issues/85
[device-ticket]: https://github.com/relaishq/relais/issues/45
[mobile]: https://github.com/relaishq/relais/issues/98
[profiles]: dev/network-profiles.md
[topology]: dev/netns-topology.md
[capacity]: dev/capacity.md
[capacity-results]: dev/capacity-results.md
[soak]: dev/soak.md
[soak-results]: dev/soak-results.md
[caller-memory]: dev/caller-memory.md
[frame-cache]: dev/frame-cache.md
[session-store]: dev/session-store.md
[readme]: ../README.md
[stickiness-adr]: adr/0001-route-stickiness.md
[checkpoint-adr]: adr/0002-checkpoint-age-envelope.md
[routes-adr]: adr/0003-persisted-relay-routes.md
[standby-adr]: adr/0004-standby-relay.md
[buffer-adr]: adr/0005-relay-buffer-replay.md
[agent-adr]: adr/0006-agent-interface.md
[tcp-adr]: adr/0007-private-route-tcp-recovery.md
[outage-adr]: adr/0008-store-outage-budget.md
[move-fix-adr]: adr/0009-planned-move-export-replay.md
[store-stall]: https://github.com/relaishq/relais/issues/95
[stall-close]: https://github.com/relaishq/relais/issues/95#issuecomment-6126635047
[expiry-pr]: https://github.com/relaishq/relais/pull/101
[outage-pr]: https://github.com/relaishq/relais/pull/103
[outage-followup-pr]: https://github.com/relaishq/relais/pull/105
[outage-job]: https://github.com/relaishq/relais/actions/runs/38268515100/job/114876708573
[store-result]: https://github.com/relaishq/relais/issues/31#issuecomment-6128574992
