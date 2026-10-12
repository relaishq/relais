# ADR 0005: Replay caller media behind a live-traffic gate

A dead worker cannot receive the caller's outage media. The relay can retain
that ciphertext and send it to a resumed worker. We retain caller SRTP in a
bounded cache, deduplicate against the exact resumed checkpoint's inbound
indexes, and hold live input until paced replay finishes. The pause still
includes failure detection; this does not make a crash a planned move.

## Cache bounds and admission

`relay.Config.Buffer != nil` enables this core protocol. `&relay.BufferConfig{}`
selects a 1.5-second window, 1 MiB per session, 64 MiB in total, and 16 tracked
SSRCs per session. The default time bound is the configured snapshot envelope
(550 ms), dead-worker window (400 ms), detector tick (50 ms), and transfer
allowance (500 ms), summed to 1.5 s. `BufferConfig.SnapshotMaxAge`, `DeadAfter`,
`CheckInterval`, and `TransferAllowance` derive that default; set them to match
any changed control-plane envelope/detector configuration, or set `Window`
explicitly. The byte bound can still evict media before the time bound.
Cache-session metadata is capped by the relay's `MaxFlows`.
Nil retains phase-1 recovery, so its existing frame-cache/PLI scenarios remain
independently testable. Standalone process wiring and acceptance are #35.

Only confirmed callers may allocate cache or hold memory, as in #6. A pending
STUN candidate is insufficient. The cache contains caller-to-worker SRTP only,
including packets sent while the worker is dead. The clear RTP header gives
SSRC, sequence and timestamp; the payload and authentication tag remain opaque.
Historical STUN, DTLS and SRTCP do not have the checkpoint's inbound SRTP index
contract and are not cached. Newly arriving control packets use the existing
hold along with media, so authenticated consent still reaches the new owner.

Two linked lists permit constant-time oldest eviction, per session and across
the relay. Capacity overflow evicts oldest packets and increments `BufferDrops`;
normal time expiry increments `BufferExpired`. Oversized packets are counted
and discarded. `BufferUntracked` counts SSRC/session metadata admission limits.
No expiry resets a live session's rollover counter. Forget/route cleanup removes
metadata; a relay restart discards the cache. Empty caches retain the worker's
ordinary frame-cache/PLI path. An incomplete
ring with selected caller media prefers input replay, followed by a keyframe
request, so cached-picture replay cannot discard retained outage video. This
cache is not durable storage.

Byte accounting includes ciphertext, relay-header room, and 256 bytes per
packet for packet/list/queue and allocator overhead. A local GC measurement of
40,000 tiny RTP arrivals retained 3,640 packets under a 1 MiB account, with about
1 MB additional heap; the old account retained 32,768 packets and 9.36 MB.
These are local retained-heap measurements, not a bound on process RSS or
transient allocation. Metadata for streams is separately capped.

Replay pins retained datagrams in the existing hold. Its per-session byte
cap is `MaxHeldBytes`. With buffering enabled, that defaults to at least
`2 × MaxSessionBytes`: 2 MiB for a 1 MiB ring. One ring budget pins history;
the second leaves room for live traffic gated during resume and replay.
This is bounded headroom, not a guarantee against a stalled or slower drain.
The default `MaxTotalHeldBytes` is at least
`TakeoverParallelism × MaxHeldBytes`: 32 MiB for 16 parallel takeovers with
2 MiB holds. Set `BufferConfig.TakeoverParallelism` to match the plane's
configured concurrency. Explicit hold limits override these defaults.
Phase-1 holds retain their 1 MiB per-session and 8 MiB total defaults.
`MaxHeldSessions` caps concurrent holds at 1,024. Pinned references remain
accounted even when the cache evicts their packets. New held traffic also
spends the hold budget.

Hold overflow retains #6's counted drop-newest policy and makes replay
incomplete, including live drops after the complete plan was handed to resume.
No-loss acceptance therefore requires both cache and hold capacity, and
successful coordination before the backstop.

## Two SRTP index spaces

For caller SSRC `C`, the relay index is `(caller ROC << 16) | caller sequence`.
Its half-space ROC estimate follows RFC 3711 section 3.3.1. The worker snapshot's
`SRTP.Inbound[C]` is the highest authenticated caller index. Replay drops every
packet at or below that value. A relay restored mid-call aligns its observed
ROC to the checkpoint before filtering; it cannot infer unknown historical
wraps from a clear 16-bit sequence alone. If the first packet of an SSRC arrives
only after preparation, its index is seeded from that checkpoint before the
live filter runs. The existing half-space/rate envelope
still applies. Replay across wraps will get end-to-end failure coverage in #35.

The worker has a different outbound SSRC and sequence space. For an anchored
echo track, outbound sequence is `caller sequence + SeqOffset` modulo 65,536;
the outbound SRTP context tracks its own ROC. Resume adds #8's checked sequence
margin to `SeqOffset` and its outbound high water mark. This maps an input
processed again to a fresh outbound encryption index. #9's `ReplayFloor` is an
outbound floor protecting reserved frame-cache indexes. Neither that floor nor
the worker's outbound SSRC is a relay checkpoint filter. `SnapshotInboundIndexes`
returns the caller transport map and checks the snapshot version.

For custom audio agents, `SnapshotReplayIndexes` instead uses the last consumed
`Agent.Progress.Index` when it trails audio's transport high water mark. Both
the application state/progress pair and transport counters come from the same
metadata-validated snapshot, so no older, separately saved application state is
substituted. A consumed SSRC mismatch or progress ahead of transport is rejected.
If the custom agent has consumed no audio, the relay has no audio floor and
retains all available audio, including caller index zero. Built-in and legacy
Echo remain synchronous and use the transport floor. Video always uses transport.

The resumed custom audio SRTP context is primed at that same consumed floor,
rather than the transport high water mark. For a never-consumed stream it starts
just before the first selected replay index (or stays unprimed at index zero).
For a cold relay with a never-consumed stream, the transport checkpoint also
aligns the selected ranges to the caller's actual ROC before the notice reaches
the worker. This admits queues larger than the 64-packet receive window and across ROC wraps
without disabling replay protection. Persisted transport high water marks and
outbound margins remain intact. Source-rate observation uses each authenticated
input index and original RTP timestamp, including queued input below the saved
transport high water mark; a compressed 2.4-second replay still measures 50
Opus packets/s. Cache loss and agent queue overflow are still
bounded, counted loss; this rule does not make the ring or agent queue durable.

Packets above the checkpoint are delivered at least once across worker tenures,
including input the dead worker already processed after its last checkpoint.
Within a target replay, repeated ciphertext indexes are filtered and per-SSRC
input is ordered, so the new worker never encrypts the same input twice. The
returned content can repeat even though outbound SRTP indexes never do.
Timestamps and frame PictureIDs retain their source identity. A receiver must
handle repeated content; agent notification integration belongs to #46.

The window is determined by the exact snapshot's inbound map and retained
packets, rather than an assumed checkpoint cadence. ADR 0002's checkpoint
metadata is validated against those same bytes before fixing the replay filter.
Takeover performs one blob read per attempt; a concurrent replacement retries
with fresh bytes while leaving the gate unprepared. The validated counter copy
supplies both caller inbound indexes and the scaled margin/reserve decision.
An unsafe age or counter envelope produces definitive loss without resuming.

The relay plan and result expose per-SSRC `Windows` in caller inbound index
space. Before any resumed processing, the control plane supplies the selected
ranges as `ResumeOptions.InputMayBeDuplicated` and `DuplicateWindows`, with the
same fields on direct and private HTTP paths. No selected media leaves these
false/empty; planned moves without replay also leave them false/empty. Bounds
can include gaps and conservatively include input whose prior processing is
unknown. After drain, the takeover result intersects successful-send ranges
with the pre-resume plan, so newly gated live input does not extend the duplicate
warning. The notice cannot be narrowed retroactively after callbacks have begun.

The takeover resume result exposes `InputMayBeDuplicated`, `DuplicateWindows`
and `InputDuplicationWindow`. The latter retains the largest conservative
`SnapshotAge` validated in this recovery, bounding the prior owner's post-copy
processing span. A failed target can persist a newer counter reservation;
that write cannot shrink the warning while the original input queue survives.
It includes counter-copy-to-write delay and metadata reply latency; write age
alone would understate the window after a delayed successful put. This is a time
span in which replayed input may have been processed, not a count of duplicates
or a bound on source RTP timestamp age. Newly gated input was never delivered
to the prior owner. Agents can use this warning without learning media keys.
The original write time and both ages remain available on the takeover event
and in the resumed worker's checkpoint observations.

## Gate and private protocol

After lease transfer, the control plane calls `BeginReplay` with a null inbound
map to install the gate, then fences the old leg by switching the route. This
precedes snapshot I/O: a stalled store must not leave the old owner forwarding.
A dead source needs no drain acknowledgement. The plane then reads the snapshot,
validates its bound age metadata and counter envelope, extracts its inbound map,
and calls `BeginReplay` again with that exact map before resume. An empty map is
valid; null means that the checkpoint is not ready yet.
Preparation filters the bounded, pinned queue against the checkpoint. A repeated
request for the same checkpoint reuses the queue. A newer map is accepted only
when every previous SSRC floor is present and at least as high; preparation
re-filters against those new floors. A regressed or missing floor is refused.
Completed replay retains one receipt per buffered session, keyed by source,
target, receiver tenure and checkpoint. Per-SSRC `replayedThrough` floors are
keyed to that same target and tenure. A lost HTTP reply returns the original
receipt without installing another gate or sending ciphertext again. The
control plane sends the transferred owner lease epoch in the private move
request. A changed target or epoch starts a fresh receiver tenure; an idempotent
move confirmation preserves it. Legacy embedded callers that omit the epoch
still distinguish target transitions, including a return to a previous address.

Preparation filters against the checkpoint. Only after the final route is
known does the drain apply `max(checkpoint, replayedThrough)`, and only for the
same receiver tenure. A stale copy for that same receiver keeps replay
incomplete and uses PLI recovery. A new target replays everything after its
own checkpoint: if A→B delivers caller indexes 11–12 and B crashes before
checkpointing past 10, B→A replays 11–12 again. A new lease at the same address
also receives that uncheckpointed input. Flow removal or explicit forget
discards the bounded receipt and floor metadata.

The delivery floor and delivered windows advance only after a successful
private UDP send. A failed send, including a self-fenced send, counts as loss
without recording delivery. Each paced batch verifies that its target and
receiver tenure still match the live gate. A mid-replay move stops the old drain
with `ErrReplayTargetChanged`; it leaves the unsent queue gated and does not
save an old-target result as the new target's receipt. Preparing for the next
receiver keeps the gate, resets delivery to its checkpoint, and rebuilds the
queue from the retained ring plus still-gated live input. Context cancellation
also permits this preparation when the receiver tenure changes.

Replay refuses a gate whose
checkpoint has not been supplied. A trimmed index above
the checkpoint, unavailable ring, metadata limit, or hold overflow marks the
plan incomplete. A complete plan tells resume to skip old frame-cache pictures
and proactive PLI, preserving the caller's codec chain. An incomplete plan with
selected media also skips cached pictures and resume PLI, then requests a
keyframe after drain to repair any missing reference. This optimization is
used only with a worker that can receive a recovery keyframe request. The
existing outbound margin/budget and frame-cache floor remain in force.

After the target resumes,
`ReplaySession` drains selected input and new held traffic on the normal private
UDP leg. The last empty-queue check and hold release share `forwardMu`, so a live
packet cannot overtake replay and push the 64-packet SRTP receive window past it.
Other sessions can forward between replay batches. SSRC streams are ordered
independently while retaining their interleaving slots. Preparation sorts once;
late arrivals insert into their SSRC's order, while ordinary arrivals append.
Each drain batch holds the forwarding lock for O(batch) work and never sorts
the remaining queue. Empty-session cleanup consumes flow-removal events, so it
does not scan all buffered sessions under the forwarding lock. Held binding
requests are admitted again to preserve authenticated consent evidence.

Both operations are available through private HTTP:
`POST /sessions/{id}/begin-replay` takes source and nullable inbound indexes;
`POST /sessions/{id}/replay` takes the resumed target. Worker resume carries the
`RelayReplay` complete-plan flag and the separate `RelayInputReplay` flag
through its existing private API (`relay_replay` and `relay_input_replay`).
The latter means incomplete caller media is pending, without promising codec
continuity. Complete plans use only `relay_replay`; incomplete plans with no
selected media leave both recovery overrides false. `relay_input_replay` is
omitted when false, so normal
resumes and complete-ring takeovers do not send an unknown field to old workers.
Workers and the control plane must upgrade together, as required by
[ADR 0002](0002-checkpoint-age-envelope.md): private HTTP readers reject unknown
fields with `DisallowUnknownFields`. Incomplete-ring takeovers require the new
field on both sides. Both flags preserve the existing outbound sequence margins
and cached-picture index reservations. Neither relay
endpoint receives media keys. The bounded hold timeout ends an abandoned replay gate by
discarding queued ciphertext; it cannot identify an adopted live recipient
safely. Normal planned-move timeout behavior is unchanged. Every terminal
takeover loss forgets the session: this atomically discards the gate, held packets
and ring before any legacy release can flush input to a failed target.
`ReplayResult` reports `Complete`, `Dropped`, `SendFailures`, and `Expired`,
including loss after resume. Expiry or any send/drop failure clears completion.
Context cancellation keeps the queued packets and completeness intact; retrying
the same lossless hold still tells resume to skip frame-cache/PLI recovery.
When resume deferred recovery for selected input and the drain is incomplete,
or a complete plan later loses packets or its hold vanishes (including restart),
the plane calls
`POST /sessions/{id}/keyframe` on the worker. This uses its existing encrypted,
rate-bounded PLI path. The plane retains the first possibly adopted tenure's
deferred-recovery decision and resends that attempt's recovery flags across
confirmation retries, even if the first request never reached the worker.
An empty expiry receipt cannot cancel it, and newly gated input cannot replace
the first attempt's ordinary resume recovery with deferred recovery. A
confirmation that resends deferred recovery also skips cached-picture replay,
as a landed first adoption does, so the first picture waits for the post-drain
keyframe.
A new target records a new recovery decision. On first adoption, an incomplete
plan with no selected media uses the worker's ordinary resume recovery and receives no
second control-plane keyframe request. Overflow or
expiry can leave a partial ring; any retained media is delivered before PLI.
When nothing remains, or buffering is off or the relay restarts before preparing
input, normal frame-cache/PLI recovery applies. On a badly incomplete ring,
the first fresh picture may wait for PLI instead of showing an old cached
picture. Unknown video SSRCs keep the request pending until video
arrives. An uncertain recovery request keeps takeover retryable. The in-process expiry test on both stores observes a worker PLI and
no receiver PLI, with zero decryption failures. Full real-process restarts,
failed adoption, a second immediate crash and frame-cache coexistence remain
#35's acceptance scope.

## Pacing and spike evidence

Default replay uses a shared bucket per target worker, allowing at most 16
packets or 16 KiB per millisecond across every session replaying to that worker.
Tokens refill with elapsed time; starting another session cannot reset the
target's burst allowance. Filtering stale ciphertext happens before token
admission, so discarded duplicates spend no sender capacity. A single oversized
datagram may exceed the batch byte target,
but still spends the bounded cache/hold budget. This restores freshness quickly
without dumping the entire ring into the worker socket at once. All sender
timers, HTTP servers, worker actors and test Redis instances are task-owned.

The first spike used the existing hold before implementing the rolling cache:
kill, resume from stored state with RTP margin 8,192 and SRTCP margin 128, release
real ciphertext. It returned 40 held packets and decoded 90/90 VP8 frames with
ffmpeg; caller and target decryption failures were zero. The core overlap test
then replayed 200 packets against the actual 64-packet receive window and returned
all 250 sent audio packets. Bypassing the production live gate made that test
fail with 103 target decryption failures in the final preparation protocol;
restoring the gate passed.

These are single-call, in-process loopback observations on the shared Mac, with
race detection, HTTP private APIs, and cache-off/PLI-on comparison. They do not
establish emulated-network, multi-call capacity or real-process results. The
outage packet window runs from kill to control-plane completion; packets already
sent before kill are counted separately as repeated content.

| Store | Buffer | Audio returned / sent / lost in outage | Video packets returned / sent / lost | First new decoded content | Audio / video gap | Replay duration | Caller + target decrypt failures |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Memory | Off | 0 / 17 / 17 | 0 / 10 / 10 | Requested keyframe | 361 / 367 ms | 0 | 0 + 0 |
| Memory | On | 17 / 17 / 0 | 10 / 10 / 0 | Outage media | 351 / 356 ms | 1.49 ms | 0 + 0 |
| Redis | Off | 0 / 17 / 17 | 0 / 11 / 11 | Requested keyframe | 368 / 403 ms | 0 | 0 + 0 |
| Redis | On | 18 / 18 / 0 | 11 / 11 / 0 | Outage media | 366 / 378 ms | 4.64 ms | 0 + 0 |

Normal takeover replay returned 32 packets (memory) and 35 (Redis), including
post-checkpoint repeated input. With deliberately stalled checkpoints, memory
returned all 21 audio and 15 video outage packets; Redis all 20 audio and 14 video
packets. Repeated pre-crash content was 15 audio and 9 video packets in each stale
trial, under fresh outbound indexes. Stale replay took 5.27 and 8.13 ms. New video
content came from the outage, with zero keyframe requests. Caller frame freshness
returned below 100 ms after replay within 0.4-41.1 ms in these four buffer trials.

The phase-1 offline recorder treats retransmitted PictureIDs as extra frames and
passes their older timestamps into ffmpeg's output mux, which rejects them as
non-monotonic DTS. The new tests decode exact caller-observed pictures once,
using their existing source PictureID identity, and require every original frame
to return and ffmpeg to decode the full unique frame sequence. They do not modify
the recorder/report/recovery files owned by #32. This proves packet return and
codec continuity, not a browser jitter buffer's audio playout/concealment policy.
The merged #32 yardstick is now used by the strict acceptance test below.
Its decoded-first-content gate remains failing; these spike measurements do
not replace that verdict.

The #34 integration tests exercise metadata replacement between the blob read
and validation, terminal loss with an allocated gate/ring on both stores, and
compressed replay through the production SRTP decrypt/echo path. That path
measures caller index advance against original RTP timestamps, not arrival time
or the outbound margin. Delivering 2.4 source seconds in a burst measured about
50 packets/s for Opus and 500 packets/s for VP8. No rate estimate is produced
before two media seconds. Replay therefore does not manufacture an arrival-rate
peak that would inflate the next checkpoint's safety margin.

## Failure detection measurement

Keep the 400 ms default. Two real workers per threshold sent their normal
100 ms heartbeats to three actual control planes concurrently for 120 seconds,
under the Mac's current shared load. These were idle in-process workers, so the
measurement undercounts false takeovers under active media, process isolation,
and transport load. No worker was intentionally stopped. The
observer counted actual rejoin/fence challenges, not predicted timer gaps.

| Dead window | Worker-minutes observed | False takeovers | Per worker-hour equivalent | Largest observed heartbeat gap |
| --- | --- | --- | --- | --- |
| 150 ms | 4 | 2 | 30 | 155.9 ms |
| 200 ms | 4 | 0 | 0 observed | 157.5 ms |
| 400 ms | 4 | 0 | 0 observed | 153.3 ms |

150 ms is unsafe on this loaded host. Zero events in four worker-minutes does
not prove 200 ms safe: a Poisson zero-event 95% upper bound is about 45 events
per worker-hour. The same observation limit applies to the 400 ms sample.
No faster-detection policy is adopted. Reproduce with
`RELAIS_DETECTION_SECONDS=120 go test -count=1 -v -run
'^TestHeartbeatDetectionMeasurement$' ./pkg/controlplane/`.

## Main integration and strict acceptance

The private relay `/metrics` registry exports ring sessions, packets, accounted
bytes, capacity drops, normal expiry and metadata admission failures. It also
exports completed drains, replayed/filtered packets, replay admission/expiry
drops, UDP send failures and replay gate expiry. These have no session labels.
Move and replay holds share the existing hold gauges; replay-specific loss
counters exclude planned moves. Scrape tests drive actual UDP forwarding,
capacity loss, send failure and hold expiry.

`TestAcceptanceRelayBufferTakeover` uses the merged caller yardstick, both
stores, and `StrictAcceptancePolicy()` without race. Buffer-off controls must
show positive trusted loss and a requested keyframe. Buffer-on must show zero
trusted loss, first new decoded outage media, zero decrypt failures and recovered
freshness over the full stability window. The test retains every strict gate.

The earlier #27 local integration was partial: both stores show zero buffer-on loss,
zero decrypt failures and recovered freshness, but the yardstick labels first
new decoded content `natural keyframe`. Its decoder-chain recorder treats
repeated pre-crash pictures as a broken chain before fresh outage pictures.
The unfiltered full decode also stops at a non-monotonic DTS from those repeated
pictures. The strict first-new-content assertion therefore fails. Recorder
changes belong to #32 and were not made here; the earlier packet/unique-decode
evidence above cannot substitute for this gate or prove actual caller decode
continuity. The table and logs in the rework report record the failed
classification explicitly. No timing threshold was loosened. The #35 process
acceptance below now consumes the current #32 yardstick directly.

## Standalone wiring and merge verification (#35)

The standalone `relais-relay` enables the bounded caller buffer by default.
`-buffer-off` restores phase-1 recovery. `-buffer-window`,
`-buffer-session-bytes`, `-buffer-total-bytes` and `-buffer-ssrcs` expose the
retention and admission bounds. All bounds must be positive. The total byte
limit is at most 1 GiB; both the per-session ring and the derived per-session
and global replay holds must fit it. The core `relay.Config.Buffer == nil`
behavior remains available to embedded callers.
The standalone relay retains the fenced lease, standby flags, bind retries
and private-listener validation from #43.

A lost replay HTTP reply on the original #35 base reproduced **38 worker
decryption failures** when confirmation recreated a drained gate. Main's #27
replay receipts now cover this regression. Confirmation returns the saved
result for the same source, target and checkpoint, without replaying ciphertext
again. The #35 test checks confirmed completion, no expired hold, exactly one
drain and zero decryption failures. Main's receipt implementation supersedes
the earlier #35 control-plane guard and completion check.

`TestRelayBufferFailurePaths` exercises real workers, caller SRTP, and the
control-to-worker/control-to-relay private HTTP APIs. Frame-cache coexistence
uses Redis frame storage in the Redis lane. Failure injection lives in the
harness tests, not production flags. Failed adoption, clean loss after all
targets reject adoption, lost resume/replay replies, caller sequence wrap,
frame-cache coexistence, overflow, expiry and actual relay restart have passing
memory and Redis merge checks. Continued complete-buffer cases check zero outage packet
loss, no outbound SRTP index reuse, no requested keyframe and ffmpeg decoding
of every unique original picture. Clean loss checks lease removal, one counted
loss and release of held bytes. Overflow and expiry check decryptable media
through the existing keyframe path.

The second-crash regressions retain their no-loss, no-PLI, no-index-reuse and
unique-frame decode assertions. Both now pass on memory and Redis. During paced replay,
117/117 outage packets returned on each store. After replay, memory returned
122/122 in its targeted run and Redis returned 118/118 in the full matrix.
Both had zero caller/adopted-worker decryption failures and released the gate.
Unit regressions cover A→B→A→B address reuse, a fresh epoch at the same address,
self-fenced sends, and a route move between paced batches. The send and move
regressions failed before their corresponding fixes.

Interrupted HTTP replay is also canceled after B decrypts its first packet,
then B is killed before checkpointing. The gate is rebuilt for C; an opt-in
probe requires C to authenticate every uncheckpointed index processed by B.
The same direct check covers a second crash after B completes its drain.
Removing tenure re-preparation makes the cancellation test fail to finish
recovery. An uncertain export with a planned hold now releases all observed
held input to the adopted worker before the backstop, with no hold timeout.
These regressions pass with memory and a dedicated throwaway Redis.

### Real-process comparison

`make crash-run CRASH_FLAGS="-compare-buffer"` interleaves ten buffer-on and
ten buffer-off SIGKILL trials in the loopback (`lan`) topology. Both modes use
the same frame-cache setting, checkpoint cadence, kill phase and 60-second
post-takeover observation. `-frame-cache-off` selects PLI-only in both modes.
The original cache/PLI comparison and relay restart/standby continuity modes
explicitly pass `-buffer-off`. `relais-load` and `relais-demo` keep the product
default buffer on. An uncertain planned export retains its existing hold and
releases it to the adopted target through the planned-hold path.
`-buffer` selects the buffered acceptance mode.
`make crash-run CRASH_FLAGS="-buffer -runs=10 -after=60s"` runs ten consecutive
SIGKILL takeovers, each with 60 seconds of post-takeover observation. Buffer
comparison refuses relay-restart, relay-standby, sigterm and netns combinations;
main's existing modes and topology validation remain available.

The comparison builds a caller test executable once from the source checkout
and launches only `TestAcceptanceRelayBufferProcessCaller` in each trial. This reuses
exact ciphertext-header packet observations and unique-picture decode inside
new harness test files. Relay, workers and control plane are ordinary separate
binaries. Each process group is owned, stopped and reaped by the runner; Redis
is throwaway on a free loopback port. This mode requires the source checkout
and Go.

The table separates audio/video returned, sent and lost outage packets, first
new source-content attribution, first-content delay, keyframe requests, relay
replay count, gate release and caller media gap. Source timestamps plus packet
position identify input after each tenure's sequence offset; duplicates cannot
hide missing source packets. Outage spans SIGKILL to terminal takeover. These
include #32's caller loss, first-new-content and freshness yardstick. Buffered
acceptance requires trusted zero audio/video loss, first new decoded outage
media and `RequireFreshnessRecovered(StrictAcceptancePolicy())`, in addition
to zero packet loss, no PLI, unique-frame decode and zero decryption failures. Media gap is reported against phase 1's
**416 ms median**, using the same SIGKILL origin for both modes.

One merge smoke (`-compare-buffer -runs=1 -after=5s`) returned **18/18 audio
and 11/11 video outage packets** with buffering, with zero decryption failures
and no keyframe request. First new source content was outage media at
**360.1 ms**; gap was **375.9 ms**, 40.1 ms below the phase-1 median. Buffer-off
returned 0/20 audio and 0/12 video outage packets; its gap was 424.3 ms and
first requested-keyframe content was 551.0 ms. Both modes released the gate
and passed the packet-return smoke. This single shared-Mac run does not prove
ten-run acceptance or browser playout.

The process driver now samples each real private `/metrics` endpoint and
writes `metrics.json`. Main's collector already exports ring sessions, packets,
bytes, capacity/age/metadata loss and completed, sent, filtered, dropped, failed
and expired replay counters. The buffered driver also requires a successful
surviving-worker scrape with `relais_worker_decryption_failures_total` equal to
zero. It requires those relay samples,
occupied-ring gauge peaks and successful replay counters. No extra collector
names or session labels are needed.

`crash-run -buffer-lossy -runs=1 -after=5s` starts the same separate service
binaries. A driver-owned HTTP proxy delays real resume beyond a 150 ms gate
lifetime, after preparation said the plan was complete. Thus the control plane
must call the worker's real `/sessions/{id}/keyframe` endpoint over HTTP. The
probe counts that request, checks gate expiry/drop metrics, continued media,
zero decryption failures and first new decoded requested-keyframe content.
The first local run observed one HTTP recovery request and one caller PLI,
counted loss, released hold and zero decryption failures. Fault injection is
confined to the driver; worker and relay APIs are unchanged apart from the
trusted move epoch.

The separate `crash-run-buffer` non-PR CI job runs exactly ten consecutive
buffered SIGKILL trials with 60-second observation. Its failure artifact keeps
`bin/crash-run-*`, including caller and service logs and metrics. Other main
jobs, including relay continuity and PR checks, are unchanged. The nightly full
both-store matrix and timing matrix still require hosted current-head results.

### Ten consecutive local process takeovers

`make crash-run CRASH_FLAGS="-buffer -runs=10 -after=60s"` passed on the
shared Mac, using its owned throwaway Redis. Each trial observed 60 seconds
after takeover. Current #32 trust and freshness gates were enforced; no timing
threshold was loosened. Logs: `bin/w35-acceptance.log` and
`bin/crash-run-1689269490/`, including per-process metrics.

| Run | Pause ms | Delta vs 416 ms | Audio returned/sent | Video returned/sent |
| --- | ---: | ---: | ---: | ---: |
| 1 | 364.6 | -51.4 | 18/18 | 11/11 |
| 2 | 440.4 | +24.4 | 21/21 | 13/13 |
| 3 | 422.9 | +6.9 | 21/21 | 12/12 |
| 4 | 415.9 | -0.1 | 21/21 | 12/12 |
| 5 | 409.7 | -6.3 | 19/19 | 12/12 |
| 6 | 408.1 | -7.9 | 19/19 | 12/12 |
| 7 | 411.3 | -4.7 | 20/20 | 12/12 |
| 8 | 376.4 | -39.6 | 18/18 | 11/11 |
| 9 | 374.8 | -41.2 | 18/18 | 11/11 |
| 10 | 377.6 | -38.4 | 18/18 | 11/11 |

Median pause: **408.9 ms**, **-7.1 ms** against phase 1.
All ten: zero trusted audio/video loss, zero decryption failures, no PLI,
first new decoded outage media, strict freshness recovered, gate released.
