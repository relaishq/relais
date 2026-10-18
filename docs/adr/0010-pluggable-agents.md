# ADR 0010: Pluggable agents and a bounded async host

Status: **Accepted** 2026-10-16 by the project lead after three design reviews; slices 1–9 implemented and released in v0.2.0, with the deferred items below still open. Part of #148.
Baseline: `f74798267771395caff9e4e6f2c73fd2918f1387`. “v1” is this proposed SDK slice; `agent/v2` distinguishes the existing interface.

## Owner summary

- A developer registers an agent and task handlers in a small Go main.
- The host runs slow work and plays queued replies independently of caller input.
- On shutdown it waits for handlers when the operator sets a measured move reserve.
- With no reserve set, shutdown uses urgent export and skips that wait.
- Moves carry buffered speech; unfinished generation can be interrupted.
- The default 1 KiB state budget cannot keep normal conversation history across a crash.
- A crash can forget earlier conversation, not just the current turn, and loses recordings/audio.
- Side effects need stable keys and a checkpoint before launch; retries can cost money.
- Store/shutdown budgets belong to the operator; move-only bytes have a separate memory budget.
- Pools use the same registration; new builds must restore prior state and pending work.
- The keyless example needs a manual gate; Demo A needs a larger budget and makes no capacity claim.
- A later store-schema ADR must land before claiming conversations survive crashes on defaults.

## Current contract and constraints

| Fact at the baseline | Evidence |
| --- | --- |
| Only echo/demo are selected by the CLI; lifecycle composition lives in main. | `cmd/relais-worker/agent.go:18-26`; `cmd/relais-worker/main.go:20-111` |
| Encoded Opus output replaces one input and keeps its timestamp; callbacks cannot mutate after return. | `pkg/agent/agent.go:36-52,80-97`; `pkg/mediaworker/agent.go:257-322,466-516` |
| Defaults are 8 KiB state, 32 queued packets, 100 ms callbacks and changed-state write interval. | `pkg/agent/agent.go:11`; `pkg/mediaworker/agent.go:18-43` |
| Whole JSON snapshots pair app state/progress with transport counters; Redis encrypts the full blob. | `pkg/mediaworker/snapshot.go:36-76,89-145`; `pkg/sessionstore/redis.go:696-720` |
| Planned moves preserve accepted state; takeover restores checkpoints and can repeat input/content. | `docs/guarantees.md:409-448`; `docs/adr/0005-relay-buffer-replay.md:89-146` |
| Identity is an operator factory convention; general mixed-version rollout is excluded. | `docs/adr/0006-agent-interface.md:165-177`; `docs/guarantees.md:797` |
| Archives use CGO_ENABLED=0; data channels and provider integration are not guaranteed. | `scripts/release/build-archives.sh:18-26`; `docs/guarantees.md:785,795` |

Use existing session/move/takeover/tenure terms. Checkpoint means a successful fenced store
write, not Save (`docs/adr/0002-checkpoint-age-envelope.md:3-26`). A task is slow work; a result
is a numbered message. The bounded process-memory export mailbox is **not a durable inbox**.

## A — Pluggability and supported API

Add `github.com/relaishq/relais/pkg/workerapp` for all worker flags/environment, stores, media,
relay attachment, control heartbeats/rejoin, private HTTP, readiness, metrics and signal drain.
Relay/control remain separate services. Rebuild
stock `relais-worker` on it, keeping `-agent=echo|demo`, defaults, optimized Echo, and demo
observation behavior. Custom names cannot shadow built-ins. New calls use the selected
agent; restoration uses the persisted name, never a fallback to Echo.
Keep existing `pkg/agent` signatures. Put async events in additive `pkg/agent/v2`.
Supported SDK: these packages, later `pkg/agent/opusio`, and `pkg/callharness` for tests. Pin
pre-1.0 releases: compatible signatures within a minor, deprecation before later-minor removal,
and migration notes for breaking minors. Snapshot compatibility is separately tested.

**Public Go declarations (bodies omitted):**

```go
// github.com/relaishq/relais/pkg/workerapp
package workerapp
import (
    "context"
    agent "github.com/relaishq/relais/pkg/agent/v2"
)
type Registration struct {
    Name string
    Factory agent.Factory
    Tasks map[string]agent.TaskHandler // versioned kinds, e.g. "stt/v1"
    Limits agent.Limits
}
type Options struct { Agents []Registration; DefaultAgent string }
func Main(Options) // os.Args, signals, bounded drain, error reporting/exit
func Run(context.Context, []string, Options) error // no signals or os.Exit
```

```go
// github.com/relaishq/relais/pkg/agent/v2
package agent
import (
    "context"
    "time"
    legacy "github.com/relaishq/relais/pkg/agent"
)
type State struct { Version uint32; Bytes []byte; HasMoveOnly bool; MoveOnlyLength int }
// Save returns small crash state and O(1) move-data metadata, never move-only bytes.
type MoveOnlyState interface { // optional; required if HasMoveOnly is true
    SaveMoveOnly(context.Context) ([]byte, error)
    RestoreMoveOnly(context.Context, []byte) error
}
type ResumeNotice struct { legacy.ResumeNotice; MoveOnly Outcome }
type FactoryArgs struct { SessionID string; AgentName string; MaxStateBytes int } // effective admitted budget
type Factory func(FactoryArgs) Agent
type Agent interface {
    Handle(context.Context, Event) (Effects, error)
    Save(context.Context) (State, error)
    Restore(context.Context, State) error
}
type EventID string
type TaskID string
type Turn uint64
type EventKind string
const (
    Started EventKind = "started"; AudioInput EventKind = "audio_input"
    TaskMessage EventKind = "task_message"; TimerFired EventKind = "timer_fired"
    Resumed EventKind = "resumed"
    Control EventKind = "control" // reserved now; routed support in Demo A slice
    PlaybackFinished EventKind = "playback_finished"
)
type Event struct {
    ID EventID; Kind EventKind
    Input *legacy.Input; Result *TaskResult; Resume *ResumeNotice
    Timer *TimerNotice; Control *ControlMessage; Playback *PlaybackNotice
}
type Effects struct {
    StartTasks []TaskSpec; CancelTasks []TaskID; CancelBefore *Turn
    SetTimers []TimerSpec; CancelTimers []string
    Audio []Audio; ClearAudioBefore *Turn
    NotifyPlaybackFinished *Turn // optional, one completion notice for this turn
}
type Audio struct { Turn Turn; Source AudioRef }
type AudioRef struct { TaskID TaskID; Sequence uint64; PacketOffset uint32 }
type PlaybackNotice struct { Turn Turn }
type TimerSpec struct { ID string; After time.Duration }
type TimerNotice struct { ID string; Generation uint64 }
type TaskClass string
type ResumePolicy string
const (
    ReadOnly TaskClass = "read_only"; SideEffecting TaskClass = "side_effecting"
    Retry ResumePolicy = "retry"; Abandon ResumePolicy = "abandon"
)
type TaskSpec struct {
    Step string; Kind string; Turn Turn; Class TaskClass; Resume ResumePolicy
    Request []byte; Timeout time.Duration
}
type Outcome string
const (
    OK Outcome = "ok"; Failed Outcome = "failed"
    Timeout Outcome = "timeout"; Canceled Outcome = "canceled"
    OutcomeUnknown Outcome = "outcome_unknown"; Capacity Outcome = "capacity"
)
type EncodedAudio struct { Format string; Packets [][]byte }
type Chunk struct { Data []byte; Audio *EncodedAudio }
type TaskResult struct {
    TaskID TaskID; Sequence uint64; Turn Turn; Outcome Outcome
    Chunk Chunk; Final bool; Detail string // bounded diagnostic, not machine semantics
}
type TaskRun struct { /* private host-owned fields */ }
func (*TaskRun) Spec() TaskSpec // owned copy
func (*TaskRun) ID() TaskID
func (*TaskRun) Key() string
func (*TaskRun) After() uint64
func (*TaskRun) Emit(context.Context, uint64, Chunk) error
// nil return publishes terminal OK; Fail supplies another typed terminal outcome.
func (*TaskRun) Fail(Outcome, string) error
// Context cancellation/deadline map to Canceled/Timeout; other errors to Failed.
type TaskHandler func(context.Context, *TaskRun) error
// A future TaskRun.Input method can be added without changing handlers.
type ControlMessage struct { ID string; Sequence uint64; Name string; Data []byte }
type Limits struct {
    MaxStateBytes int; MaxPendingBytes int; MaxMailboxBytes int; MaxActiveTasks int
    MaxAudioBytes int; MaxAudioDuration time.Duration
}
// Initialized sentinels; ErrVersion/ErrStateTooLarge preserve legacy identity.
var ErrVersion, ErrStateTooLarge, ErrIncompatibleAgent, ErrBackpressure, ErrCapacity error
```

Developer main passes a stable name, factory and task map to `workerapp.Main(Options)`;
no worker lifecycle is copied and no internal packages are imported.
The host derives TaskID/key from a versioned length-prefixed tuple of session, agent name,
causing EventID and Step. No public ID/key helper, caller-supplied task ID, random ID, worker
address, or tenure enters that identity. Require unique Step per cause; conflicting arguments
for the same ID fail. Input EventID uses caller SSRC/extended index; task events use task
ID/sequence; timers use ID/generation; Started has one stable session ID. Resumed identifies
an adoption attempt and **must not return StartTasks**. Pending work is the host's concern.
**Handle must ignore unknown EventKinds** without changing state or returning effects.

## B/D — Actor, retry-only tasks, results, and cancellation

Callbacks/Save/Restore stay serial, off the packet reader, under the existing 100 ms default
budget; no network I/O or mutation after return. Save follows accepted events. Publish app
state, consumed input, task descriptors, result cursors, timer generations, cancellation floor,
and audio references atomically. Non-input events never advance caller progress. Rejected
callbacks/effects restore the prior local state; quarantine late callbacks as today
(`pkg/mediaworker/agent.go:179-215,257-322`). Redeliver Started until a checkpoint records it,
including its pending task descriptors; otherwise a crash can lose an uncheckpointed greeting.

Choose **retry-only**, not a durable result journal, for v1. Results are **delivered at least
once, committed once** in the saved logical history. A callback can run again after rollback;
a network request or audible rendering can also repeat. Planned export carries the exact
bounded move-only mailbox. Crash checkpoints record presence/length of move-only recordings,
ReadOnly requests and audio, never their bytes. Resumed.MoveOnly is OutcomeUnknown when data was
present but lost; missing ReadOnly requests/results get typed OutcomeUnknown tasks. Keep the
phase in Bytes; clear unsent speech and restart on later input/Control/timer, not StartTasks from
Resumed. Never activate refs with absent payloads. Checksums needed for local task dedup/retry
are computed once at admission, not by repeatedly hashing recordings during Save.
At exact export, pause the actor and call optional SaveMoveOnly within the export deadline,
consistent with its last accepted Bytes/HasMoveOnly/MoveOnlyLength. Validate declared length and
memory/wire bounds; copy only then. On exact resume, Restore small State, then RestoreMoveOnly
before Resumed. On crash resume do not call RestoreMoveOnly; deliver OutcomeUnknown instead.
Missing optional methods with HasMoveOnly=true fail state acceptance, not the eventual move.

- ReadOnly (STT/LLM/TTS) launches immediately after local acceptance on an active route,
  without waiting for a checkpoint. Extra starts wait as never_launched when all four default
  slots are busy; keep deadlines and FIFO fairness, not a Capacity result for slot exhaustion.
  Its Request bytes are move-only, even for never_launched tasks. A crash cannot reconstruct
  their bytes: report OutcomeUnknown and restart the turn. Replayed fresh causes keep
  stable IDs; repeated computation/charges are allowed by this class, not deduplicated by Relais.
  Interrupted ReadOnly Retry needs a reproducible/resumable prefix; otherwise report
  OutcomeUnknown. Never regenerate different audio under an existing reference.
- SideEffecting requires explicit Retry/Abandon and a **forced, non-coalesced fenced
  checkpoint** containing intent/state/progress before launch. The dispatcher performs this
  outside callbacks/media locks. Persist its bounded Request bytes, unlike ReadOnly requests.
  Any acknowledged successful checkpoint containing the full launch intent permits launch;
  no confirmation read is needed during an outage. Retry requires provider dedup/query by
  TaskRun.Key with unchanged arguments and adequate retention. Slot exhaustion queues it as
  never_launched; do not pre-mark it as started while it is waiting for a slot.
  Abandon reports OutcomeUnknown if it may have started; it never repeats an uncertain effect.
- Persist phase `never_launched`, `may_have_started`, or terminal/canceled, kind, absolute
  deadline and cursors; ReadOnly stores request presence/length only. The forced launch checkpoint conservatively records
  `may_have_started`. Exact export can prove a handler never crossed local launch admission:
  mark it `never_launched`; the target launches it normally, including Abandon tasks. Recheck
  local fence/attempt/turn immediately before any launch, including after checkpoint I/O. A crash
  cannot prove that interval, so a prepared Abandon task can conservatively be unknown.
- Positive task timeout becomes an absolute store-clock deadline; resume never resets it.
  Recheck that deadline immediately before launch, including after store recovery or a slot
  wait; report Timeout rather than call an expired task. TaskRun.After remains a safe replay
  floor separate from committed result progress. Move resumes have exact payloads; a SideEffecting
  retry requires provider-stable result identity/sequence. Missing ReadOnly bytes yield
  OutcomeUnknown, not an automatic refill. This leaves room for a later durable inbox.
- The **first Emit chunk is immediate** if memory permits; later chunks publish at most once
  per 100 ms per task. Coalesce/backpressure with cancellation; merge or delay terminal
  publication within that interval. Emit never writes Redis. Bound chunks before copying and
  reserve a typed terminal slot per task. Queued starts share the move-memory budget; a request
  exceeding the byte bound is an explicit admission failure, not an unbounded waiting queue.
- Audio effects append `(TaskID, sequence, packet offset)` refs to the host's move-only backlog.
  The host stages only <=300 ms/4 KiB into the next-to-send window, advances the pacer, and
  backpressures Emit when the move mailbox is full. Callbacks never wait for playout, copy audio
  into app state, or negotiate credit. Remove AwaitAudio/AudioDemand/AudioCredit entirely.
  Optional NotifyPlaybackFinished emits PlaybackFinished after that turn's tasks/results finish
  and queued audio is sent; this is local send completion, not proof the caller heard it.
- Checkpoints retain only a bounded playback cursor/reference window and payload presence/length;
  exact exports also carry the full backlog/payloads. Audio.Format is unnecessary: the referenced
  EncodedAudio owns the format. Do not tick callbacks/Save on idle playback. Keep requests and
  payloads until their references drain and required acknowledgement/horizon passes.
- Cancel local contexts immediately on accepted CancelTasks/CancelBefore; ClearAudioBefore(t)
  removes queued audio tagged with Turn < t. Persist the task cancellation and monotonically
  increasing turn floor in the next snapshot; reject late/parked results by attempt token and
  turn before publication; only host-generated Canceled terminals bypass the old-turn data filter.
  No separate store-side marker in v1: cancellation becomes durable
  at that checkpoint. A prior-checkpoint takeover can retry old work until cancellation is
  replayed; cancellation cannot undo an external effect. Do not wait for storage to barge in.
- Receipt/payload retention ends after checkpoint acknowledgement **and** the duplicate-window
  horizon, with no live audio reference. No lifetime task counter/cap. Compact monotonic input,
  task-result, turn and control sequence floors survive pruning; conservative duplicate ranges
  do not justify discarding new work (`docs/adr/0005-relay-buffer-replay.md:127-146`). A horizon
  is the validated replay/retained-export horizon, not a guessed 550 ms receipt TTL. Bounded
  active-window pressure backpressures new tasks; it must not end an ordinary long conversation.

### Store outages (ADR 0008)

The default outage budget is 30 s, with 34 s retention and unchanged checkpoint cadence;
transient store errors retain forwarding/ownership, while authoritative lease loss fences
(`docs/adr/0008-store-outage-budget.md:3-22,26-50,112-133`). ReadOnly launches and Emit may
continue locally within all budgets. SideEffecting launches wait for a successful checkpoint
containing their intent. Failed forced writes consume **no snapshot byte tokens** and never
produce Capacity; uncertain outcomes do not grant launch or require reads during the outage.
Any later acknowledged successful checkpoint containing that intent grants launch, after a fresh
deadline/fence/cancellation check; expired intent gets Timeout. Emit/terminal results have no store
append in this version: a failed checkpoint leaves them in bounded local memory, not silently
acknowledged or dropped. When full, Emit backpressures with cancellation; reserved terminal
space records failure/timeout. On recovery, checkpoint the current state/cursors; do not reload
old state. On a worker crash, missing move-only data gets OutcomeUnknown, while SideEffecting
work follows Retry/Abandon. Stale checkpoints
still pass ADR 0002's age/margin policy. Thirty seconds of retained ownership does not promise
thirty seconds of replay, service output, or recoverable agent state. New moves/adoptions can
wait/fail during the outage. No app-capacity event may masquerade as a transient store failure.

### Move, drain, and pacer

Urgent planned moves stop launches, cancel unfinished handlers, freeze mailbox admission and
export accepted state within the existing flush budget. Playout already buffered is **preserved**:
pause at the export fence, transfer backlog/cursor/payloads, then continue paced playout on target.
Only unfinished generation is interrupted/retried/abandoned. Migration cancels an attempt, not
the logical task/turn: fence its late Canceled result; do not mark the task Canceled or clear audio.
No drop/clear just because a move is urgent. Preserve ADR 0009's pre-auth pause, exact window, output drain marker, exclusivity and
ReleaseAfterExport (`docs/adr/0009-planned-move-export-replay.md:52-106,151-173,235-245,303-329`).

Quiescence is **worker-local at SIGTERM, before calling /workers/{name}/drain**. Start one
-drain-timeout deadline (currently 5 s: `cmd/relais-worker/main.go:32,105-108`). First acknowledge
an idempotent mark-draining operation in control, so placement excludes this registration; also
reject raced local creates. This new mark-only endpoint must not move calls. Today only Drain
marks draining (`pkg/controlplane/controlplane.go:798-837`); factor that step for the worker.
Then stop launches and wait for in-flight handlers, **not playout**, concurrently across all
sessions while consuming results and pacing audio. Never hold a control call lock or drain slot:
today Drain holds both around p.move, limited to min(GOMAXPROCS,16)
(`pkg/controlplane/controlplane.go:866-903,1005-1009`), and cannot infer the worker's timeout.
Use max(0, min(2 s, remaining drain-timeout - measured move rounds)) for this single local phase;
move-round reserve comes from validated loaded drain measurements for the session count and
control concurrency, including placement-mark time/headroom, supplied as a validated worker flag
-agent-drain-move-reserve. With no valid reserve, skip the wait. A smaller operator
-agent-drain-task-budget may tighten it, never extend the deadline.
Then call /workers/{name}/drain with the remaining deadline. Never-launched requests move exactly;
if the wait expires, unfinished handlers use urgent export. Heartbeats/private API stay live
until moves finish (`cmd/relais-worker/main.go:94-111`; `cmd/relais-worker/drain.go:14-36`).

Use a monotonic output pacer independent of ingress, a 48 kHz RTP clock, and host-validated
packet duration/MTU, SSRC, sequence and encryption counters. Encoded output type stays generic;
v1 accepts `audio/opus` only. Parse duration in the host; no public Samples field. Initialize
outbound timing before first caller audio without inventing consumed input. The persisted
next-to-send window is <=300 ms/4 KiB payload equivalent, not a cap on a full move-only reply.
Send packet starts at most 20 ms early; an indivisible Opus packet can span up to 120 ms.
Persist logical time/next timestamp/queue cursor with crypto counters; restore from store-clock
age, never compared worker clocks. Rebase overdue sends; no burst catch-up or indexes when empty.
Keep existing margins; validate at least the generated 400 packets/s maximum (2.5 ms packets)
as well as caller rate (`docs/adr/0002-checkpoint-age-envelope.md:28-77`). Store-clock timers
carry generations, fire overdue generations once in committed history, and honor cancellation.

DTX can remove the regular inbound clock. Inspected WebRTC revision `9a49bcc60f4b22293dc9451bdbc679c18e5b3a9e`:
[usedtx enables DTX, lines 256-258](https://webrtc.googlesource.com/src/+/9a49bcc60f4b22293dc9451bdbc679c18e5b3a9e/modules/audio_coding/codecs/opus/audio_encoder_opus.cc#256),
[default disabled, line 64](https://webrtc.googlesource.com/src/+/9a49bcc60f4b22293dc9451bdbc679c18e5b3a9e/api/audio_codecs/opus/audio_encoder_opus_config.h#64),
[sparse silence packets, lines 324-351](https://webrtc.googlesource.com/src/+/9a49bcc60f4b22293dc9451bdbc679c18e5b3a9e/modules/audio_coding/codecs/opus/opus_unittest.cc#324).
Source evidence is not installed-Chrome capture; today's demo uses an oscillator (`cmd/relais-demo/web/demo.js:61-78`).
Separate output timing changes ADR 0006 only for v2 (`docs/adr/0006-agent-interface.md:12-19`).

## C — Measured defaults, separate move memory, and approval gate

Reviewer measurement: established v7 plaintext is **3,474 B before media-counter growth**;
DTLS cert/key/connection contributes **2,072 B (59.6%)**, constant after handshake. Source: scratch
`wagent/snapsize/pkg/mediaworker/zz_size_test.go:9-15` (reviewer-provided measurement, not capacity
proof). Redis envelope overhead is budgeted at 42 B (30 B for the shown 2-byte header + 12-byte
nonce + 16-byte tag: `pkg/sessionstore/redis.go:709-715`); v8 metadata/counters are additional.
The previous 6 KiB cap left 6,144-3,474-42 = 2,628 encoded bytes, before host metadata; an 8 KiB
app alone needs 10,924 encoded bytes. That default could not work.

New **v2** app-state default: **1 KiB** raw Bytes (configurable maximum 64 KiB); serialized
crash-persistent host metadata/request/ref allowance: **2 KiB**, including v8/counter growth.
ReadOnly requests/recordings stay move-only: checkpoint only presence/length and recovery phase;
ordinary Save never serializes/copies/hashes those bytes, even for a 1 MiB recording. SideEffecting request bytes stay inside the 2 KiB host
allowance (Limits.MaxPendingBytes counts that encoded host subtree; MaxStateBytes counts Bytes
only). MaxStateBytes in FactoryArgs exposes the effective admitted budget. Reject startup if a
registration's declared MaxStateBytes cannot fit the operator snapshot cap plus validated host
reserves; never silently shrink it. Resolve default limits before this check; recheck actual size
at runtime. Limits.MaxMailboxBytes bounds separate move memory. Default active tasks **4**; queue
additional starts as never_launched without Capacity
for slot exhaustion. Other limits respect operator caps; legacy 8 KiB defaults stay unchanged.

Worker-only flags, not Limits fields (registrations cannot raise store/shutdown budgets):
-agent-max-snapshot-bytes=7168; -agent-snapshot-bytes-per-second=81920;
-agent-snapshot-burst-bytes=28672. Slice 9 adds -agent-drain-task-budget=2s and
-agent-drain-move-reserve with quiescence; slice 6 does not expose these flags.
The reserve has **no default**: unset means no quiescence and urgent export for every v2 session.
Snapshot arithmetic: 3,474 + 42 + 1,368 (base64 of 1,024) + 2,048 = **6,932 B**, leaving 236 B under
7,168. This is a derived allocation; validate actual v8/counters, never assume the base is fixed
for all calls. At 10/s, 7,168*10 = **71,680 B/s**. The 81,920 B/s payload budget leaves 10,240 B/s
for forced writes; plus measured frame/owners 31,371+3,256 gives **116,547 B/s**, leaving **14,525**
under 131,072 for session protocol/reads/retries. This is **not measured capacity approval**
(`docs/dev/capacity-results.md:61-70`; `docs/dev/capacity.md:180-225`). An explicit 8 KiB app needs
at least 3,474+42+10,924+2,048 = **16,488 B/checkpoint**, already 164,880 B/s before other clients.
Demo A uses an explicit larger **operator** budget: e.g. 24 KiB snapshots, 264 KiB/s and
96 KiB burst for an 8 KiB app. At 10 checkpoints/s, 24,576*10 = 245,760 B/s;
264 KiB/s = 270,336 B/s adds one maximum forced snapshot/s. Burst is four maximum snapshots. It makes **no capacity claim** and does not raise the approval ceilings.

Preflight actual whole ciphertext size before state acceptance and again at capture; reserve
periodic/required transport work before new side-effect intent. Successful checkpoints consume
byte tokens; a failed write releases its reservation without debit/Capacity, and transport/store
failures remain distinct from admission exhaustion. Count all attempted wire bytes/commands in
metrics/gates, including failed writes; keep bounded retry/backoff. Queue intent while success is pending;
any later successful checkpoint containing it suffices. Do not coalesce its initial forced write,
slow cadence, delay crypto-required writes, or relax snapshot age to save bytes. Genuine size/
healthy-store budget refusal stays typed; unavoidable transport overflow refuses adoption/ends
with explicit v2 capacity evidence. Store I/O/flow-control waits never hold actor/media locks.

**Move-only budget:** default **1 MiB raw per session**, **64 MiB total per worker**, operator
flags -agent-move-bytes-per-session/-agent-move-bytes-total. It includes recordings, ReadOnly
requests, full reply audio/backlog, mailbox and pending-start payloads, outside snapshot accounting.
A 30 s reply at 64 kbit/s is 240,000 B before metadata, so this can carry a full bounded reply,
not merely 300 ms. Raw byte limits are insufficient: preflight the actual serialized export AND
resume request, including nested base64 and envelope, below **4 MiB**. The baseline limit is
`internal/privateapi/http.go:22,121,174` (not pkg/privateapi); byte-slice wrapping is visible at
`pkg/mediaworker/http.go:32-46`. Backpressure producers at memory/wire headroom; reject a request
that can never fit before acceptance. No emitted result may make an already accepted move
unexportable. Keep one immutable payload copy with refs and bounded export serialization scratch;
worker-global accounting includes that scratch and concurrently held exports. Quiescence only
waits for handlers: playout continues but an entire buffered reply can move without draining it.

Choose a **manual pre-approval gate**, not an automatic release claim: run the relais-load
capacity step against the custom example binary, with 100 commands/128 KiB request+reply per
call-second, checkpoint-age AND snapshot-age p99 <=550 ms on default envelope, and Save CPU/time
per event/call-second. Include forced writes/retries/all store clients; report export HTTP/memory
separately. Add worker-binary/agent overrides (`cmd/relais-load/topology.go:172` hardcodes stock).
The load workflow actually has a schedule (`.github/workflows/load-calibration.yml:3-4,67-68`),
but the new custom-worker gate is manual until implemented; green main CI alone does not satisfy
it (`.github/workflows/release.yml:12-30`). Before owner approval to enable v2, attach the exact
build/config/run evidence. Manual review binds approval to the exact build/operator budgets while
the representative custom workload remains unvalidated. Keep ceilings; over-budget blocks approval.

## E — Identity and safe refusal; same-registration pools

Use envelope v8 for custom agents, containing agent name, contract, State.Version, required task
kinds and declared Limits, with the exact state/progress/cursors. Zero limit fields remain
zero in identity and mean host defaults; effective bounds are resolved and capped locally,
never persisted as identity. Stock echo/demo retain their legacy profiles. A custom target
checks name/kinds/declared limits and validates restored content against local effective state,
host metadata, mailbox, active-task and audio bounds before adoption, output or task launch.
Identity mismatch returns ErrIncompatibleAgent; content exceeding local capacity returns
ErrCapacity without truncation. Never substitute Echo. Main f6428f2 experimental v8 envelopes
retain their previous resolved-bounds compatibility check.
Identity-free custom snapshots need a declared profile or drain; version 1 does not identify an app (`docs/adr/0006-agent-interface.md:165-177`).
Pools are **same-registration only**; compatible names alone do not authorize mixing task code.
New builds must Restore prior state/pending task schemas and retain compatible recovery capacity.
Incompatible rollout needs another pool or ended calls.
No capability placement/bridge promise. Failed targets retain the transferred lease; control owns
source rollback, alternate targets or typed loss cleanup
(`docs/adr/0006-agent-interface.md:109-125`). Upgrade platform support before enabling v8; old
readers reject it. Preserve ADR 0009's worker-first ordering and strict private API compatibility
(`docs/adr/0009-planned-move-export-replay.md:247-264`; `docs/adr/0002-checkpoint-age-envelope.md:99-100`).

## F/G — Encoded audio, keyless example, Control and the guide

Keep CGO_ENABLED=0. Use raw encoded Opus for v1. Source-verified pinned pure-Go libraries:
Pion [oggwriter v4.2.20:7-20,354,475,1186-1201](https://github.com/pion/webrtc/blob/v4.2.20/pkg/media/oggwriter/oggwriter.go)
wraps encoded payloads and computes granules; [rtp v1.10.5 OpusPacket:28-38](https://github.com/pion/rtp/blob/v1.10.5/codecs/opus_packet.go)
only rejects empty input, not invalid codec framing. Neither encodes PCM. Pion
[oggreader v4.2.20:268-340](https://github.com/pion/webrtc/blob/v4.2.20/pkg/media/oggreader/oggreader.go)
returns page payloads, not complete Opus packets; its lacing/continuation fields are not exposed.
Demo A needs bounded pure-Go `pkg/agent/opusio` declarations (using context, io and agent/v2):
```go
type Reader struct { /* private bounded packet assembler */ }
func NewReader(io.Reader) (*Reader, error)
func (*Reader) Next(context.Context) (agent.EncodedAudio, error)
```
Assemble real packet boundaries, CRC, continuation,
headers, and duration per [RFC 3533 section 6](https://www.rfc-editor.org/rfc/rfc3533.html#section-6)
and [RFC 7845 sections 3-5](https://www.rfc-editor.org/rfc/rfc7845.html#section-3); reject unsupported
chaining/mappings/gain explicitly. Provider Ogg/Opus format, pre-skip, final trim and padding need
fixture/provider validation; packet forwarding is not sample-exact PCM conversion. Pion writer
pre-skip defaults to 3,840 samples (`oggwriter.go:28,598,912`): test preroll/short recordings before
STT upload. Do not add a public WriteOgg/trim/gain abstraction in this slice.

Keyless `examples/delayed-loopback` records bounded packets with a sparse seen window, ends by
byte/time without more input, then replaces recording state with a task request. A local ReadOnly
handler waits 300 ms, checksums the move-only recording and emits stable
100 ms playback chunks. Exact moves carry request/recording/full reply; crash resumes get
OutcomeUnknown, clear unsent refs and record a new turn. Never copy audio into durable Bytes. Move during recording/processing/playout, kill at each cut, and
show turn/cursors, pending bytes, snapshot age and replay notice through private diagnostics.
No VAD, codec install or API key. A local idempotent ledger separately tests side effects.

Control for Demo A push-to-talk: client sends `POST /calls/{id}/agent/control` with
`{id,sequence,name,data}`; signal -> control -> current worker private API
`POST /sessions/{id}/agent/control`. ControlSender/ControlReceipt belong to that later slice,
not the first public interface. No media data channel.
Resolve the live route under existing call ownership; private targets never come from the client.
That later HTTP shape returns 202 for local acceptance and 200 only after a checkpoint
records it. Client retries with the same ID/sequence/body across routing changes. Per-call ordered
sequences and a sparse dedup window checkpoint like input; compact old prefixes to reject late
duplicates after receipts expire. Reject conflicts; bound control data to 1 KiB within pending bytes.
Emit Control with stable EventID; no public control-authentication claim beyond existing deployment
constraints (`docs/guarantees.md:791`). Slice 8a implements these endpoints and
`agent.ControlReceipt`; `controlplane.ControlSender` is an optional worker capability.
There is no additional SDK sender abstraction; Demo A uses the HTTP route.
New sequences start at 1 and must be contiguous; future sequences are refused
without consumption. The retained sparse receipt window compacts acknowledged
prefixes after four newer sequence positions, with no wall-clock TTL.

Outline `docs/build-an-agent.md`: build keyless example; register main/factory/tasks; Save/Restore
and IDs; read-only versus side-effect work; lost move-only data/outages; refs, turns and cancellation;
state/byte budgets and capacity gate; move/kill/drain evidence; same-registration upgrades;
then Control push-to-talk, STT Ogg upload and TTS packet reader. Write it with implementation.
Realtime/live STT are **out of scope and unverified**. They need streaming task input, a PCM
path or validated pure-Go Opus decoder, and a provider connection that survives/re-attaches
after worker restart. This ADR does not design it or claim HTTP retry restores that conversation.

## H — Tests and implementation order

Preserve existing host tests (`pkg/mediaworker/agent_test.go:85-158,234-305,357-427`), checkpoint
pairing tests (`pkg/mediaworker/agent_checkpoint_test.go:110-155,170-209`), call continuity/replay
(`pkg/callharness/agent_internal_test.go:237-275,319-421`; `pkg/callharness/relay_buffer_agent_test.go:29-64`),
and stock/demo/drain behavior (`cmd/relais-worker/agent_test.go:99-103,222-318`; `cmd/relais-worker/drain_test.go:16-53`).

| Seam | Required new proof (all NOT-RUN) |
| --- | --- |
| External module | Public imports only; CGO_ENABLED=0 custom/stock build; effective FactoryArgs budget; oversized declaration fails startup without clamping; preserve flags/readiness/heartbeat/metrics/drain. |
| Host/Memory/Redis | Started redelivery; ignore unknown kinds; reject StartTasks from Resumed; immediate ReadOnly versus forced SideEffecting launch; uncertain writes; stable IDs; typed outcomes; never-launched Abandon export. |
| Call harness | Move exact recordings/requests/reply/cursor; SaveMoveOnly runs only at paused export, once; checkpoints use presence/length without copying/hashing recording; crash OutcomeUnknown; delivered at least once, committed once; duplicate windows/sparse inputs/wraps. |
| Flow/cancel | Host appends refs and backpressures Emit; first chunk immediate; optional PlaybackFinished is send-only evidence; turn-tagged clear/cancel and late-result drop; no pacer/checkpoint deadlock; >150 turns without lifetime cap. |
| Clocks/limits | Packetless and DTX input; variable packet duration, queue/lead bounds, no catch-up; timer generation/reset; size at limit and limit+1; ciphertext/token budget including forced writes; no lock cycle or skipped required checkpoint. |
| Process | Worker-local SIGTERM mark-only placement fence; unset reserve skips wait; configured reserve enables concurrent handler-only wait without call locks/drain slots; urgent full-reply move; kills/lost replies/mismatch cleanup/zombies; ADR 0009 regressions. |
| Outage gate | Extend store-stall (`.github/workflows/ci.yml:290-315`): 5/15/29 s, no token debit/Capacity on failed forced writes, launch on later successful intent checkpoint, deadline Timeout before launch, local Emit/terminals, unknown move-only data on stale takeover and 35 s expiry. |
| Capacity/audio | Custom-worker relais-load gate in C, Save CPU, checkpoint-age p99; known Ogg lacing/CRC/pre-skip/trim decode fixtures; real Chrome DTX capture; Demo A provider-format tests. Source inspection is not runtime proof. |

Implement after owner approval, in this order:
1. workerapp; stock worker rebuilt on it; external-module CGO_ENABLED=0 build test.
2. Envelope v8 with name/task-kind refusal and existing adoption cleanup.
3. agent/v2 events, including reserved Control.
4. *TaskRun, two task classes, export mailbox, typed outcomes and host-derived IDs.
5. Pacer/store-clock timers, <=300 ms persisted window, move-only backlog, host flow, Clear/Cancel.
6. Limits, whole-snapshot byte admission and Emit coalescing budget.
7. Keyless example, guide, manual custom-worker capacity/Save-CPU and store-stall gates.
8. Demo A: routed Control push-to-talk, Pion oggwriter STT upload, opusio.Reader TTS.
9. Worker-local SIGTERM quiescence before control Drain; loaded move-round/playback measurements.
10. Deferred work below; none is implied by completion of the first nine slices.

## Deferred / later ADR

Durable inbox and result blobs with fencing/retention (stronger recovery, extra store cost);
LegacyFactory; public ReadVersions/WriteVersion, heartbeat capabilities and bridge-rollout recipe;
256 KiB state; public WriteOgg trim/gain handling; public IdempotencyKey and OpusPacket.Samples;
streaming task input/Realtime, PCM codecs and resumable decoder state. The Demo A
HTTP Control sender and receipt plumbing arrived in slice 8a; it is no longer deferred.
Keep TaskRun.After/Emit compatible with future durable results. No single-render promise.
One **store-schema ADR** must cover both constant DTLS/transport setup state written once in a
separate same-slot key, and app state written only on change in the **same fenced EVAL**, with
the changing transport blob carrying the exact app-state revision and digest. Do not design it here.
Reviewer estimates: splitting DTLS alone frees 2,072 B/checkpoint, giving only about **2.7 KiB**
raw app room: (7,168-(3,474-2,072)-42-2,048)*3/4 = 2,757 B. It does not make 8 KiB affordable.
Writing 8 KiB history once per turn is about **2 KB/s** at roughly one update per 5-6 s, versus
**109 KB/s** at 10 Hz (10,924 B/update). Today's constant DTLS bytes cost **20.7 KB/s/call**,
about **41%** of 50,277 B/s session traffic and **24%** of 84,904 B/s total
(`docs/dev/capacity-results.md:61-70`). These are arithmetic projections, not new measurements.
Slices **1-7 do not wait** for this ADR. Demo A needs an explicit operator budget, with no capacity
claim. The store-schema ADR must land **before any default-budget conversation crash-survival claim**.

## Guarantee impact and approval

Legacy guarantees and ADR 0009 remain gates. V2 planned continuation includes the exact accepted
mailbox/references. Takeover loses move-only recordings/ReadOnly requests/buffered speech,
reports OutcomeUnknown and restarts the turn; side effects retain Retry/Abandon. Speech can pause
or repeat and restarted turns can incur new charges. Default 1 KiB cannot preserve normal history;
this is not merely current-turn loss. Worker-local drain waits for handlers only **when the operator
sets a measured move reserve**; unset skips waiting. Then buffered playout moves; urgent exports
preserve buffered audio but interrupt unfinished generation.
During ADR 0008 outages, media continues but side-effect launches wait and local mailboxes can
backpressure. Snapshot budgets add explicit v2 capacity refusal/end conditions; they must not be
hidden as loss-free continuation. No general mixed-version, side-effect transaction, or seamless
crash promise. On implementation, update `docs/guarantees.md:795` for the newly tested unprompted
output/provider scope, plus agent conditions at `docs/guarantees.md:409-448`; preserve other exclusions.
Owner approval: supported API; retry-only recovery/charge limits; task-class trust and forced writes;
measured-base defaults/operator-only budgets; manual approval gate; move-only crash loss;
same-registration deployment and worker-local drain time reserve.
