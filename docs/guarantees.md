# Phase-2 guarantee matrix — draft

This document records the evidence for [phase 2 (#31)][spec] and
[ticket #49][ticket]. It describes the tested conditions, not a production
service-level agreement. It does not make a phase-2 go/no-go decision.

The main reference is commit `765db53`, [CI run 38197061923][main-run].
Capacity, soak and browser evidence have their own sources below. A passing
sample applies only to its stated workload and topology. Independent machines,
real cameras and sustained load cannot be inferred from namespace or loopback
tests.

The full soak is **pending [#41][soak-ticket], run by Derek through [#85][full-soak]**. Real devices are
**pending [#45][device-ticket]**. Cloud confirmation is **pending [#48][cloud]**. The checkpoint-envelope
row is **pending [#95][store-stall]**, a real-process store-stall run. The unresolved rollback failure in **#51** and
agent-test flake in **#94** remain visible in this draft.

## How to read the matrix

- **Pass** means the cited sample met its acceptance rules. It does not mean
  that every deployment or every failure path has passed.
- **Pending** means the named run has not supplied its required evidence.
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
These are short load floors, not maximum capacities or an operating recommendation.
See [capacity-results.md][capacity-results].

The one-call profile results and the LAN load results are separate experiments.
Moves and takeovers under `region`, `far-caller` or `lossy-dc` **at the validated
LAN load are not measured**. Relay continuity and agent browser trials also
do not establish loaded N+1 timing.

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
has separate short LAN evidence at eight calls. A confirmed export transfers
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

**Status:** **Pass for the cited profile matrix and short LAN load sample.**
Real-camera and Android results are pending #45; real-internet confirmation is
pending #48. **#51 remains a known exception on aborted/rolled-back moves**;
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
validated on short LAN steps at eight calls with N+1. Browser and cloud scope
remain pending.

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

**Status:** **Pass within the cited buffer, checkpoint, profile and load
conditions.** Pending #45 for real encoders and phone playback; pending #48 for
the real internet. Overflow, expiry, relay-buffer loss and terminal adoption
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

## Agent continuity

**Guarantee:** A compatible audio agent restores its accepted state and consumed
input position exactly on a successful planned move, and restores its checkpoint
on takeover with measured state age and a warning when input may repeat.

**Conditions:** Matching agent factory and supported state version on every
worker; bounded queues; callbacks honor their deadlines; valid checkpoint and
relay replay conditions. The demo is deterministic, audio-only and uses encoded
Opus clips without an external service. Profile-by-load or N+1 agent timing is
**not measured**. The browser evidence is a single call in the local demo.

[ADR 0006][agent-adr] defines **8 KiB** default state size, a **32-packet** input
queue, **100 ms** callback deadlines and **100 ms** minimum changed-state write
interval. Queue overflow and callback timeout can drop input and count it. A
planned export drains accepted work within its budget and can count dropped
remaining input. Exact continuation means the accepted state/progress pair
continues; it is not a promise to process every sent packet.

The [#31 browser result comment][agent-result] reports **seven passing events**:
**three moves and one drain** restored exactly; **three kills** restored
checkpoints **372–460 ms** old; the **60-second hold** passed. The reported
recording includes a takeover from a **445 ms** checkpoint. This draft cites
Derek's result comment; it does not independently inspect the private recording
or claim that it is publicly available.

State rollback uses **snapshot age**, not write age alone. The demo verdict
allows snapshot age plus **one 20 ms input packet**, as documented in the
[README agent-continuity rules][readme]. Replay can repeat work after the saved
input floor. [ADR 0005][buffer-adr] supplies duplicate input ranges and the
conservative duplication window before callbacks start. Applications must
make external side effects safe to repeat themselves.

**Status:** **Pass for the reported local Chrome run and cited hosting/replay
contract tests.** #94 remains an unresolved pre-move agent-test flake. Android,
real-camera and cloud runs remain pending their tickets. No universal no-loss
application or exactly-once side-effect guarantee is established.

## Capacity and soak

**Capacity guarantee:** The cited short LAN sample recovered after one worker
death and completed a subsequent planned move at its validated eight-call load
without exceeding the demonstrated worker floor.

**Conditions:** The workload, shared runner and N+1 formula in the load section;
buffer on; safe checkpoint envelope; unsaturated generator. Sustained capacity
and other network profiles at that load are **not measured**.

| Final capacity observation | Result | Source |
| --- | --- | --- |
| Worker floor | 8 calls; 16-call step inconclusive due to generator saturation | [Capacity results][capacity-results] |
| Relay floor | 8 calls; 1,370.7 forwarded datagrams/s; 16-call step inconclusive | [Capacity results][capacity-results] |
| N+1 death, then move | Pass at 8 calls across 3 workers | [Capacity results][capacity-results] |
| Loaded drain | 44 ms at 8 total calls | [Capacity results][capacity-results] |
| Store request/reply use | About 67 client commands and 85 KB per call-second | [Capacity results][capacity-results] |
| Store budget | 100 commands and 128 KiB per call-second, window averages | [Capacity policy][capacity] |
| Maximum relay packet rate; monetary cost/call-hour | Not measured; generator limits the ramp and no prices were supplied | [Capacity results][capacity-results] |

**Status:** **Pass for the short LAN sample.** The store budget is based on
planning assumptions; it does not prove Redis sizing at the larger target.
Agent-state workload cost is not isolated by this echo/fixture capacity sample.

**Soak guarantee sought:** Continuous calls remain within the move and takeover
rules through repeated moves, drains and kills, with no unintended lost calls,
decryption failures or leaked resources and no sustained memory growth.

**Conditions still to prove:** At least **50 calls for one hour**, an event
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

**Status:** **Pending #41/#85.** Full-run memory trends, event coverage and
cleanup results are not measured until that run arrives.

## Every threshold in the phase-2 spec

This table preserves all rows of [#31's threshold table][spec]. A pass is
limited to the cited conditions. The residuals below still require the project
lead's later disposition.

| Spec threshold | Status | Evidence or what the blocking run must show |
| --- | --- | --- |
| Takeover with relay buffer (`lan`): no lost content or keyframe wait; pause reported against phase 1; freshness back to baseline within 2 s | **Pass** | [Main buffered process job][buffer-job]: 10/10, median 413.4 ms versus 416 ms; zero loss; outage media first; freshness recovered. [Strict harness controls][harness-job] pass on both stores. |
| Planned move under `region` and `far-caller`: each track p95 ≤ baseline + 50 ms and ≤150 ms; zero decryption failures; no reconnect | **Pass in final sample** | [Final #37 matrix][network-result]. #51 remains a known integrity failure on an uncertain-reply rollback outside a successful planned move. |
| Takeover under `region` and `far-caller`: pause <2 s; no lost content with buffer; zero decryption failures; no reconnect | **Pass under the implemented lossy-link policy** | [Final #37 matrix][network-result]. [Profile rules][profiles] judge loss against the network baseline and burst guard; exact zero loss applies only to lossless media paths. |
| Relay restart or standby: gap <1 s; no reconnect; pause → takeover → resume never creates two forwarders | **Pass** | [Main restart][restart-job] restore-on and [standby][standby-job] SIGKILL/SIGSTOP tables pass; old process dead before new readiness; zero duplicate media. Single-call loopback scope. |
| Planned load with N+1, including after forced worker death: move/takeover rules hold; drain time reported | **Pass for short LAN sample** | [Final capacity evidence][capacity-results]: validated T=8, death then move pass, drain 44 ms. Shaped-profile load and full consent at that load are not measured. |
| Checkpoint-age envelope: a store stall beyond it never produces undecryptable takeover; policy counted and documented | **Pending [#95][store-stall]: real-process run** | [Main harness][harness-job] `TestRelayCheckpointAgePolicy` passes failed-write, stalled-write and delayed-success cases on both stores with zero decryption failures. [ADR 0002][checkpoint-adr] defines scaled margins or counted definitive loss. [PR #53][checkpoint-pr] reports normal real-process takeovers, but a separate-process store-stall trial beyond the envelope was not found: NOT-RUN. No product failure is inferred from this missing evidence. |
| Soak: ≥50 calls for ≥1 h with chaos; zero decryption failures/unintended lost calls; flat memory; no leaked sessions, keys or processes | **Pending #41/#85** | Full hardware-specific N+1 run must meet [soak acceptance][soak], including `outcome=pass`, `acceptance=true`, conclusive required memory rows and bounded cleanup. [Short run][soak-results] is supporting evidence only. |
| Real camera at 720p in desktop Chrome: planned-move and takeover rows hold | **Pending #45** | Save buffer-on and buffer-off records using the [device checklist][devices]. Verify actual camera dimensions, per-track move/takeover verdicts, first-live-frame times, visibility and the completed consent hold. Results not measured. |
| Android Chrome on local network: planned-move and takeover rows hold | **Pending #45** | The same device record and buffer comparison on Android; retain device identity and any blocked/invalid/inconclusive result. Results not measured. |
| Agent continuity: exact planned continuation; bounded checkpoint rollback on takeover; duplicated input flagged | **Pass for cited local demo and harness contract** | [Browser report][agent-result], [ADR 0006 hosting/replay tests][agent-adr] and [ADR 0005 notices][buffer-adr]. State-age interpretation uses snapshot age. #94 remains unresolved and does not prove a continuity failure after a move. |
| Cloud confirmation: move/takeover rows hold over real internet | **Pending #48** | Separate VMs in one region; external Chrome plus callers on another network; both driver and browser evidence. Approved provider/budget before spend, total ≤$25 including egress; final cost and teardown evidence. See [#48][cloud]. Results not measured. |

## Open residuals and source differences

### #51: caller decryption failure after rollback

[The reproduced failure][rollback-hit] occurred in run **38174220164**,
`lossy-dc` move trial **15**, after an injected lost resume reply and rollback.
The caller counted **three authentication-tag failures and zero replay
failures**; workers counted **zero decryption failures**. Media continued and
later moves were clean. The cause remains unknown; no root-cause regression or
fix is established.

[The latest #37 result][network-result] updates the observed frequency to
**one failing rollback in about 230 untraced rollbacks**, including the final
matrix, versus **zero in 521 traced rollbacks**. These are observed counts, not
a stable rate or reliability bound. The [investigation][rollback-doc] retains
the earlier untraced denominator of about **210**. Tracing may change timing.
The nightly lossy-DC move lane alternates traced and untraced runs; failures
retain caller records and worker logs. Monitoring remains active. A passing
matrix does not remove this exception to a blanket zero-decryption-failure claim.

### #94: agent test stops short before the planned move

[Issue #94][agent-flake] records one Redis test failure in run **38179543910**:
the agent consumed **33 of 34** successfully written inputs, with **zero input
drops and callback deadlines**. The assertion failed before the move. Its
frequency and cause are **not measured**. It may reflect loss or late input;
the source does not establish which. It is separate from #51. The related older
[#76][old-agent-flake] is closed; that does not close this new failure.

### Differences that affect interpretation

| Sources | Difference | Interpretation used here |
| --- | --- | --- |
| [#31 spec][spec] / [latest summary][phase-result] and [profile policy][profiles] | Spec says “no lost content” on lossy networks; the summary also uses that shorthand. The implemented gate permits network loss within a statistical bound. | Exact zero loss only on lossless media paths; no measured excess over baseline on lossy paths. |
| [#31 spec][spec] and [ADR 0002][checkpoint-adr] / [ADR 0006][agent-adr] | Spec describes state staleness using checkpoint write age. ADRs distinguish snapshot-copy age, which can be older. | Use snapshot age for recovery safety and application rollback; report both ages. |
| [README][readme] and [frame-cache documentation][frame-cache] / [ADR 0005][buffer-adr] | README retains older statements that the frame cache is process-local and the agent base does not replay input. Current docs describe Redis frame storage and agent input replay. | Use the current ADR contracts and scoped result evidence. The older README passages need separate cleanup. |
| [#31 agent spec][spec] and [ADR 0006][agent-adr] | Spec asks for decoded audio. The implemented interface accepts encoded Opus; a resumable decoded-audio adapter is deferred. | No decoded-audio or portable codec-state guarantee. |
| [#37 earlier result][old-network-result] and [final result][network-result] | Earlier lossy-DC moves failed; the final tuned main matrix passes. | Final result supersedes the earlier matrix for these conditions. Untuned private-route performance is not accepted by the final sample. |
| [PR #53][checkpoint-pr] and [ADR 0002][checkpoint-adr] | Earlier PR describes an RTP send limiter; current ADR measures caller source sequence rate without a send limiter. | Current ADR governs the caller input bound; no claim that a send limiter enforces it. |

## Explicit non-guarantees

| Excluded claim | Source |
| --- | --- |
| Cross-host relay failover, floating public IP, anycast, or multiple relays across hosts | [#31 out of scope][spec]; [ADR 0004][standby-adr] |
| Safari or data channels | [#31 out of scope and guarantee-matrix section][spec] |
| Multi-region operation, Kubernetes or production orchestration | [#31 out of scope][spec] |
| Multi-party rooms, fan-out, simulcast or SVC | [#31 out of scope][spec] |
| TURN, TCP or TLS **media fallback** | [#31 out of scope][spec]. Private HTTP/Redis TCP tuning is a separate matter. |
| Production store durability, Redis failover/backup operations, or automatic relaunch of failed standbys | [#31][spec]; [session-store limits][session-store]; [ADR 0004][standby-adr] |
| Control/private API authentication beyond private addresses and firewalls; a proven public signaling front | [#31][spec]; [topology signaling limit][topology] |
| Unconditional route protection after failed/dropped persistence or partial restoration; protection from stolen session credentials or compromised workers | [ADR 0001][stickiness-adr]; [ADR 0003][routes-adr] |
| No-loss recovery after buffer loss, overflow, expiry, agent queue overflow or unsafe checkpoint loss; exactly-once application side effects | [ADR 0002][checkpoint-adr]; [ADR 0005][buffer-adr]; [ADR 0006][agent-adr] |
| Video input for agents, validated resumable PCM decoding, unprompted agent output, or speech/LLM-provider integration | [#31][spec]; [ADR 0006][agent-adr] |
| Recording, SIP, MoQ or GPU transforms | [#31 out of scope][spec] |
| Windows standby fencing, general mixed-version rollout compatibility, or fixed global Redis frame-cache memory | [ADR 0004][standby-adr]; [ADR 0006 compatibility rules][agent-adr]; [frame-cache limits][frame-cache] |

## Evidence to add when runs arrive

- **#41/#85:** Record code revision, machine and capacity calibration, chosen
  N+1 floors, full duration/load, chaos coverage, generator exclusions,
  per-process memory verdicts and cleanup inventory. Replace the pending row
  only with a sourced full-run result.
- **#45:** Analyse all device/buffer records. Keep media gap, first new content
  and first live frame separate. Add actual device/camera settings and every
  failure or inconclusive cause.
- **#48:** Add driver and browser records, topology, workload, observed network
  conditions, checkpoint ages, final cost and teardown proof.
- **Checkpoint envelope:** Add the real-process store-stall result from
  [#95][store-stall]. Normal takeovers do not exercise that fault.
- Keep #51 and #94 open in the evidence until their causes and dispositions
  are recorded. Publication and the later phase decision belong to the project
  lead and orchestrator.

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
[agent-flake]: https://github.com/relaishq/relais/issues/94
[old-agent-flake]: https://github.com/relaishq/relais/issues/76
[cloud]: https://github.com/relaishq/relais/issues/48
[soak-ticket]: https://github.com/relaishq/relais/issues/41
[full-soak]: https://github.com/relaishq/relais/issues/85
[device-ticket]: https://github.com/relaishq/relais/issues/45
[profiles]: dev/network-profiles.md
[topology]: dev/netns-topology.md
[capacity]: dev/capacity.md
[capacity-results]: dev/capacity-results.md
[soak]: dev/soak.md
[soak-results]: dev/soak-results.md
[caller-memory]: dev/caller-memory.md
[devices]: dev/real-device-checklist.md
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
[rollback-doc]: investigations/51-rollback-srtp.md
[store-stall]: https://github.com/relaishq/relais/issues/95
