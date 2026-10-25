# Bridge host output

The ADR 0014 host path is available to a v2 registration named `bridge` in a
split-checkpoint pool. It is an owner-side Go API, ready for the authenticated
bridge transport. This slice does not add a WebSocket server, Python runner,
provider integration, or a fallback agent.

## Admission and ownership

The owner binds an authenticated `BridgeOutputScope` through `BindBridgeOutput`.
`BeginBridgeOutput` reserves an idempotent host turn/stream marker before observer
tasks start. `SubmitBridgeOutput` copies, validates and hashes the packet outside
the session lock. Its dedicated ingress consumer prepares the actual metadata
encoding outside that lock, then rechecks the captured actor/host revision,
scope, deadline, floors and memory reservation at commit. Stale preparation
retries only inside the admission budget. Expiry settles a packet refusal and a
media gap; it never invokes Handle, Save or the actor failure path.

The separate priority lane supports cancellation and clear-audio. The playback
module owns the canonical cancel/clear floors. Actor commits project them, and
rollback cannot lower them. Task launch, task-result application and the pacer
check the live floor. Each retained packet carries its complete scope; the pacer
checks that scope at the existing locked send boundary.

`BridgeOutputCredit` returns absolute available slots/bytes with monotonically
increasing advertisement revisions. The producer must subtract its unresolved
identities and obey both dimensions. Credit waits occur before host ingress;
real-time encoded-ready latency measurements still include them. Burst grading uses each turn's first ready-to-wire sample and reports later credit wait separately. All in-flight copies,
retained digests, references, queued audio and export scratch count against the
existing session/worker move budgets. Pending ingress has its own bounded cap.
Actor data retains its existing eight-times export allowance. Bridge export
shares immutable payload references, reserves frozen records/references, and
charges the JSON encoder buffer (up to twice its used size while growing) plus
the returned exact bytes, including base64, and bounded
snapshot/history fields. Bridge-only wire validation checks the same encoded
length without allocating nested copies. Pressure refuses export before pause;
the source remains usable.

**Proposed actor reserve: 160 KiB**, configurable with
`ActorReserveBytes` / `bridge-actor-reserve-bytes`. Maximum live actor work is
63 KiB: 16 KiB aggregate observer requests with two owned copies, 20 KiB of
pending results plus outstanding terminal reservations, and 11 KiB of adapter
move-only state. The reserve also protects in-flight ingress headroom. The
validated `ObserverRequestBytes`, `ObserverMailboxBytes` and
`AdapterMoveOnlyBytes` profiles bound those terms at admission and restore.
The 20 KiB result allowance can carry a full 11 KiB state candidate after
JSON/base64 expansion, plus both observer terminal reservations. Larger chunks
backpressure before copying; oversize adapter state is typed retryable capacity.
Other v2 agents keep their existing limits.

`bridgeMoveCost` reserves the larger of current actor cost and the entire
allowed future actor envelope, including results that cannot drain while the
actor waits. Two live request copies receive only one request-copy export
expansion. Credit, packet admission, drain-to-fit and export all use this model.
Output cannot use space that a later in-bounds observer turn or candidate needs.
Profiles leaving no full-size output credit are refused during bridge init.

The unchanged 1 MiB move/mailbox budget preserves the 160 KiB reserve and
Proposed 209,715 B export headroom (20%). Absolute audio ceilings remain
256 KiB and 4 s. `TestBridgeOutputFullActorEnvelopeAfterCredit` fills the queue
before observers exist, grows every actor term to its bound and performs an
actual exact export. Local caps are 36 × 160 B (5,760 B / 0.72 s) and
14 × 1,200 B (16,800 B / 0.28 s), with full working/history state. The original
two-16-KiB observer growth scenario is also covered using an explicit 32 KiB
request / 8 KiB mailbox profile; its caps are 21 / 8 packets. These are measured
fixture counts, not hard-coded limits.

Planned export/drain holds only the session being moved. The actor waits for
its queue and in-flight admission to fit before pausing input or stopping tasks.
Playback and checkpoint settlement continue. Waiting ends at the existing
handoff deadline minus two callback timeouts, preserving the pause/save reserve.
A definite deadline refusal leaves the source usable and releases its hold.
Worker-wide scratch reservation still must succeed. Frozen records are never
pruned during the exact cut. `BeginDrain` refuses new placement; sessions not yet
selected by control keep output credit and playback until their own move hold.
`TestBridgeOutputWorkerDrainKeepsUnmovedSessionsLive` checks three such sessions
and their production exports. `TestBridgeOutputCreditAndIngressDuringMove`
checks concurrent stale-credit ingress, zero-credit requests during drain, and
non-consuming moving receipts during export.

The existing 2,048-byte host metadata cap is shared by two bounded regions.
Application/task metadata and bridge/playback metadata each receive half. The
host measures actual JSON outside the packet lock. Credit preserves room for
terminal refusals, end markers and interruption. Metadata pressure backpressures
new work. It does not enlarge the host cap or the four active task slots.

## Receipts, playback and durability

Sequences span turns within one lineage. An identical duplicate returns its
retained receipt; different retained bytes conflict. Status distinguishes settled,
proven-unadmitted and unknown identities. An absent crash receipt is unknown,
including identities above the checkpoint ceiling. Crash restore advertises no
new output credit until authenticated ceiling reconciliation is implemented.

The live ledger settles admission without a store write. The pacer retires sent
payloads immediately after its local terminal receipt, while retaining SHA-256,
identity and outcome. Frozen exports retain their own immutable references. Payload retirement does
not claim checkpoint durability. `BridgeOutputCheckpointed` reports only the
captured store-acknowledged revision, including uncertain-write settlement.
Receipts, credits, status and interruption replies carry their captured scope.
Exact moves preserve the monotonic credit advertisement counter.

Receipt exceptions coalesce adjacent matching outcomes within a turn. Both the
range count and encoded-byte cap apply. Digest pruning requires acknowledgement,
an explicit replay horizon, and no live queue reference. The owner advances that
horizon only when old input/work cannot return; elapsed time is not proof.
Either horizon advancement or the subsequent checkpoint acknowledgement triggers
pruning. The host cannot infer the remote producer's replay boundary from a
store acknowledgement. Until the authenticated transport supplies that proof,
retained records stop output credit at the bounded cap and leave the actor
reserve available. The measurement producer explicitly declares its consumed
receipt horizon; the store callback never invents one.

**Phase B prerequisite: receipt retention.** Without pruning, the current export
model costs about 2.75 KB per retained receipt. The former current-actor model
held 211 receipts (4.22 s); reserving the entire future actor envelope now holds
47 receipts (0.94 s at 50 packets/s), measured by
`TestBridgeOutputReceiptPressurePreservesActor`. The 29 s store-stall run needs
about 1,450 receipts, roughly 31 times this capacity. Phase A has no registered production
bridge agent and no production caller advancing `AdvanceBridgeReplayHorizon`.
Do not claim that it can sustain that stall yet. Candidate designs are compact
settled ranges plus a digest chain, or advancing the horizon on every bot
acknowledgement. A horizon still requires durable checkpoint acknowledgement
before pruning, so the acknowledgement design alone does not remove the
29 s no-checkpoint retention requirement. The no-pruning regression verifies
that the actor stays live and output resumes after acknowledgement plus horizon
advance. No retention redesign is included in phase A.

Every receipt includes `consumed`. Terminal admission/refusal/cancellation and
retained duplicates consume the identity. Ordering, scope, moving and capacity
refusals before commit do not. A retry below the pruned floor returns
`status=settled`, with no invented packet outcome. Moving, unbound, uncertain,
out-of-order and stale-scope reasons remain distinct.

A late-packet gap advances RTP time without adding to the wall send schedule.
The cursor records late-wake rebase coverage, consumes that coverage once, and
preserves the existing pacer behavior tracked in #265. Queue recovery and pacer
debt are different measurements.

A bridge turn stays busy until audio_end, settled identities through its final
sequence, and an empty turn queue. The two non-audio bridge observer tasks do not
block PlaybackFinished. `BridgePlaybackOutcome` joins the unchanged v2 playback
notice to complete/gapped/canceled/unknown and its gap count. Completion proves
local send, not caller hearing.

## bridge_output/v1

The #252 decoder accepts the optional `bridge_output` domain at version 1.
Its absence means no reads, writes or continuation capability. This phase does
not register a bridge agent, so generated release manifests omit both the domain
and the `bridge` worker profile. This preserves v0.3.0 target-tool validation,
which requires an exact domain set. A release that registers the bridge agent
must enable its split-checkpoint `bridge` profile and advertise the domain;
retained bridge state requires readers on workers and control. Legacy snapshot
bytes, public v2 types and golden fixtures stay unchanged. Missing/incompatible
capability is refused by preflight; restore also checks the actual bridge version
and encoded metadata.

The optional `bridge_output/v1` subtree in the host envelope records scope,
lineage-to-stream/turn markers, issued/settled/sent/pruned cursors, exception
ranges, cancel/clear floors, outcomes and rebase coverage. The normal playback
window uses compact tagged bridge/gap spans. An absent kind keeps the established
task-reference encoding.

The private exact-move section contains one owned cut of that state, the last
acknowledged state, credit advertisement counter, replay horizon, retained packet digests/receipts, unsent
payloads, and the full tagged queue. Bridge references contain kind, stream_id,
packet_seq, turn, offset=0, count=1. Gap references contain a sequence range and
its duration without payload. Restore checks complete ledger coverage, scope,
stream/turn bindings, queue order, identity uniqueness, payload length, Opus
duration, SHA-256, limits and reconstructed playback equality. Missing or
conflicting references are refused. Application SaveMoveOnlyLength remains the
application's exact length; the host section is separate and repeatable.

## Profile and validation

`AgentConfig.BridgeOutput` and the worker's `bridge-*` flags resolve the Proposed
values: 50 ms admission; 32 packets/38,400 pending payload bytes; 256 KiB/4 s
retained audio; eight ranges/512 encoded bytes; and the Proposed 160 KiB actor
reserve, 16 KiB of aggregate observer requests, 20 KiB of result/terminal space,
11 KiB of adapter move-only state, plus 209,715 B of export safety headroom. Test-only constants retain the 50 ms interruption target, 20 ms full /
10 ms partial fixture packets, 200 packets/s for 2 s, 150 ms raw latency and
500 ms recovery gates. Worker flags cannot change measurement targets.
Invalid profiles fail startup. Effective audio bounds also respect the registered
agent/mailbox ceilings. Working bytes reserve the existing worst-case host
metadata allowance from the bridge's 3,072-byte budget.

## Next slice

Phase B must drive fenced same-lineage epoch remapping, recovered bot-issued
ceilings, revocation/replacement, and the exact wire-boundary race using the same
session lock. A scope transfer must update retained packet scopes together with
the frozen lineage; a replacement must invalidate the old queue/receipts instead.
The current binding API deliberately cannot authorize either transition.
A restored output queue stays gated when its saved scope has not been bound to
the destination owner epoch. Phase B must authorize that remap before release. The
29-second store stall and move/rollback campaign remain separate acceptance
work. The Python transport must use the ready timestamp from before any credit
or IPC wait, then query uncertain identities before retrying identical bytes.

## Measurement

Build-tagged sustained qualification:

```sh
python3 scripts/dev/with-redis.py go test -race -tags bridge_measurement \
  ./pkg/mediaworker -run 'TestBridgeOutputSustainedMeasurement|TestBridgeOutputHostStallFailsLatencyQualification|TestBridgeLatencyQualificationRejectsControls' \
  -count=1 -timeout=4m -v
```

The continuous mixed phase runs for 61 seconds at 50 packets/s with full
checkpoints, controls, gaps, jitter, rapid turns and actual 10 ms final Opus
packets among the 20 ms packets. The host-stall control keeps the production
actor event loop live while holding its separate ingress consumer.

Real-time phases keep the independent source clock and ready timestamps before
credit/IPC wait. The 160 B 4× burst blocks the bounded 32-packet source queue,
modeling paused TTS reads. It tracks every one of the 400 IDs, reports credit and
producer waits separately, and grades first-packet ready-to-wire p95 over ten
interrupt/clear turns, exact move/restore and bounded export cost. A separate
single-turn sustained burst must block on credit near the cap, avoid starvation,
refusals and lateness, stay bounded and drain all 400 packets. Raw wire gaps,
scheduling debt and gaps beyond existing debt accounting remain visible. Each
phase reports raw ready-to-admission p95,
refusals, closures, drain/cancellation accounting, every stall's recovery, queue
slope, pacer debt, checkpoint bytes/age, process CPU and Save timing. Both the
host-admission stall and maximum-size overload must report `qualified=false`.
