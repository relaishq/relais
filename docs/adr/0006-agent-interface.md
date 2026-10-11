# ADR 0006: Per-call audio agents in the session snapshot

## Context

Ticket #46 and phase-2 spec #31 section 7 require small application state to
survive the same planned moves and crash takeovers as media. Echo remains the
default. A slow application must not hold the UDP reader or video processing.
No system libraries or machine-wide installs are permitted.

## Audio decision and codec research

Agents receive owned, encoded Opus RTP payloads and caller inbound SRTP progress.
They can emit one encoded payload for each input, with the input's RTP timing,
or suppress that output. Custom output has its own continuous RTP sequence,
using the existing persisted counters and takeover margins. Silent input never
spends output indexes or the unsent stream's pre-wrap runway. Echo keeps the
original source-sequence rewrite. This supports playing pre-encoded clips;
unprompted output and a separate output clock are deferred. Video keeps its
existing echo and frame-cache path.

Research checked upstream sources on 2026-10-10:

- [Pion Opus v0.1.0](https://github.com/pion/opus/tree/v0.1.0) is MIT, pure Go,
  and includes SILK, CELT and hybrid decode paths. It is no longer correct to
  describe Pion as SILK-only. Its public decoder keeps codec and resampler state
  private and has no snapshot/restore API. Its current main branch also exposes
  packet-loss concealment; do not assume that API exists in v0.1.0.
- [tphakala/go-opus](https://github.com/tphakala/go-opus) is BSD-3-Clause, a
  libopus derivative. Upstream claims a complete decoder and all 12 official
  conformance vectors. Its current go.mod requires Go 1.27, above this project's
  Go 1.26 toolchain. These are upstream claims, not local codec validation.
- [hraban/opus](https://github.com/hraban/opus) is MIT but wraps native libopus
  through cgo, so it does not meet the installation constraint.

The pure-Go options are credible, but neither was validated here as a portable
codec with bounded, serializable continuation state for this ticket. Adopt no
codec dependency. `agent.Decoder` and `agent.PCM` define a later decode adapter
seam. An adapter must include codec state in the application snapshot or declare
that it resets at resume. Applications that require PCM should validate a pinned
adapter separately. No PCM completeness or bit-exact decoder migration is claimed.

## Hosting and deadlines

Each session has an independent agent from `AgentConfig.Factory`. The default is
`agent.Echo`. Built-in Echo uses the original synchronous packet path: no
payload copy, channel round trip or per-packet deadline. Its consumed progress is recorded
under the existing packet lock. Export fences Echo atomically without pausing
audio first, so no input is dropped between a pause and the fence.
All custom callbacks run off the media path. A bounded queue (32 packets by
default) accepts audio without waiting.
Overflow and callback timeout drop input and count it. Callbacks run serially,
outside the media lock, with a 100 ms deadline by default. Process plus state
encoding share a deadline. The host copies custom payloads and returned state
bytes. Planned export budgets queued work at half the relay default hold timeout
(1.5 s), reserving a full callback deadline before starting another queued input.
At budget exhaustion it drops the remaining queue, counts each input drop, and
publishes the already-consumed state/progress pair. An in-flight callback can
finish within its own deadline; budget exhaustion alone never ends the call.
Export ends a session only if its in-flight callback is actually quarantined
past its deadline, or another terminal agent error occurs. Idle agents never
request checkpoints; the existing transport ticker supplies the base cadence.

A callback that finishes after its deadline cannot publish state or output. Its
instance is quarantined while it runs; further input is dropped. On completion,
Restore rolls back to the last accepted local state before another callback.
Failed rollback is terminal. Go cannot forcibly stop an arbitrary function: an
agent must honor its context and must not mutate after returning. A permanently
blocked callback can retain one goroutine/instance until it returns, but does not
block worker shutdown or media. Factories must be quick and nonblocking.

## State and persistence

Save encodes versioned application bytes after each changed callback. This is
local encoding, not a store write. The payload has a hard 8 KiB default limit.
Save and Restore must be inverses; version zero is reserved. State includes any
application-owned codec state and all logic needed to continue.

A successful callback pairs its encoded state with the exact last consumed caller
SSRC, extended input index, RTP timestamp and consumed count. State and progress
publish together with outbound media under the packet lock.
Changed-state store writes coalesce at most once per `SaveInterval` (100 ms
default). Transport-only snapshots of an unchanged stateless echo keep the
existing cadence. A checkpoint always copies the newest complete state/progress
pair; a rate-limited attempt skips the entire write, rather than misdating an
older agent pair with a newer checkpoint timestamp. A skip, including one found
after capture, records no checkpoint attempt, success or failure. A planned move
stops enqueue, drains the accepted queue within its budget, drops any remainder,
and publishes the final consumed pair before fencing/export, bypassing the rate
limit. Periodic copies include owned agent
bytes under the same media lock as the transport counters. Encoding and fenced storage run outside that lock.
Successful writes reuse the exact pair captured for encoding, without decoding
the snapshot again. Required resume and replay-reservation checkpoints always
write; only optional live checkpoints can return an explicit skipped result.
The restored pair seeds durable state before callbacks, since its checkpoint
or held export is already available to rollback.
The bytes ride in the existing encrypted snapshot for Memory and Redis and count
in the existing store byte accounting. No store schema changes are needed.

Oversize, Save failure, unsupported agent version, Restore failure and callback
failure are counted on the worker and exposed by `AgentStats` and private status.
Store write failures follow the existing transport checkpoint policy for all
agents: count failed writes, retain ownership and keep retrying. The checkpoint
ages until a successful retry, and a takeover reports its measured age. A store
stall does not become an agent Save failure or end every application call.
Failures from the agent's own Save, invalid state and live callbacks remain
terminal and counted. Worker counters are available through both /status and
the worker's /metrics registry, without session labels.

A failed target restore never adopts a session or emits resumed media. It returns
a typed agent cause and closes without releasing the transferred lease. The
control plane owns cleanup: planned moves can restore a compatible source;
takeovers can try another compatible target. If none works, the loss result
preserves the agent cause and removes the lease and snapshot immediately.
Resume checkpoint errors also close without release, preserving rollback and
retry. A committed store write cannot become a failed write due to a subsequent
snapshot decode.

## Resume and replay

Restore and Resume callback timeouts are typed adoption failures (`ErrRestore`),
even when they wrap a context deadline. Both direct and HTTP control-plane paths
try another target and retain the agent failure cause if no target can adopt.
Restore validates bytes before transport adoption, followed by a deadline-bounded
Resume notice. Resume must have no external side effects: failed transport
adoption can retry both callbacks on a fresh instance. Internal state changes
from Resume are encoded before the required adoption checkpoint. The notice
includes planned move or takeover, measured checkpoint write age, snapshot-copy age, and the saved consumed-input progress. The separate
snapshot age also covers copy-to-commit storage delay; checkpoint age alone does
not bound application state staleness. Rate limiting ages the checkpoint itself
and is therefore included in those observations. Tests measure the saved pair
and these ages explicitly.

Relay takeover now populates `ResumeOptions.InputMayBeDuplicated` and
`DuplicateWindows` from the prepared relay plan before Restore/Resume. Direct
and private HTTP paths carry identical caller inbound SRTP ranges. A resume
notice describes possible replay, not exactly-once processing: gaps and input
whose prior processing is unknown can occur within each range. After drain,
the takeover result narrows those ranges to successful sends within the original
plan. Live input arriving after preparation cannot have been processed by the
fenced source and does not enlarge that result. No selected media or a planned
move without replay leaves the fields false/empty.

Custom-agent consumed audio progress can trail `SRTP.Inbound` because callbacks
are queued. Ticket #27 uses the consumed `Progress.Index` as the audio replay
floor when it is below transport. The agent state/progress pair and transport
counters must come from the same validated checkpoint; mismatched consumed SSRC
or progress ahead of transport is rejected. A never-consumed stream has no audio
floor, including index zero. Synchronous built-in/legacy Echo and video keep
the transport floor.

The resumed SRTP receive context uses the same consumed floor. With no consumed
progress, it starts just before the first selected replay index, or unprimed at
index zero. Cold-relay range indexes are aligned to the authenticated transport
checkpoint's ROC before notifying the worker, including never-consumed audio.
Replay protection stays enabled; queues larger than its 64-packet
window and caller ROC wraps are tested. Persisted transport high water marks
and outbound reservation/margins stay intact. Memory and Redis HTTP takeover
tests recover inputs queued at the source checkpoint and consume each once on
the target, with zero decrypt failures and target queue drops. Buffer capacity,
agent queue overflow/deadlines and the cross-tenure floor limitation assigned
to #35 still apply. No durable or universal no-loss application claim is made.
Applications must tolerate replay after a checkpoint and make external side
effects idempotent themselves.

Transport snapshots now use version 7, so old workers cannot silently resume a
custom application's snapshot as echo. Version-six snapshots with no agent
extension remain readable only by default Echo, using the distinct Echo state
marker (version 0x4543484f, empty bytes).
Echo rejects custom version-1 state, including empty stateless snapshots, to
prevent an incompatible factory from silently switching output sequences.
Unmarked Echo snapshots from the initial version-seven implementation are
ambiguous and must be drained before upgrading; they are rejected rather than
interpreted as Echo. No public agent interface signatures changed.
Missing or zero-version state in a version-seven snapshot is rejected, including
by Echo. Workers must use the same configured application factory across moves;
application versions are validated by Restore. Application identity remains an
operator configuration contract, not something inferred from its bytes.
