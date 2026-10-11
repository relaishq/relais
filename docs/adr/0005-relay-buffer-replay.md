# ADR 0005: Replay caller media behind a live-traffic gate

A dead worker cannot receive the caller's outage media. The relay can retain
that ciphertext and send it to a resumed worker. We retain caller SRTP in a
bounded cache, deduplicate against the exact resumed checkpoint's inbound
indexes, and hold live input until paced replay finishes. The pause still
includes failure detection; this does not make a crash a planned move.

## Cache bounds and admission

`relay.Config.Buffer != nil` enables this core protocol. `&relay.BufferConfig{}`
selects a one-second window, 1 MiB per session, 64 MiB in total, and 16 tracked
SSRCs per session. Cache-session metadata is capped by the relay's `MaxFlows`.
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
metadata; a relay restart discards the cache. Empty or incomplete caches retain
the worker's ordinary frame-cache/PLI path. This cache is not durable storage.

Byte accounting includes ciphertext and relay-header room. Packet/list metadata
is bounded by that byte budget and the minimum RTP header size. Metadata for
streams is separately capped. Replay pins retained datagrams in the existing
hold: per-session bytes are capped by both cache and hold limits, all holds by
`MaxTotalHeldBytes` (8 MiB by default), and concurrent holds by
`MaxHeldSessions` (1,024). Pinned references remain accounted even when the cache
evicts their packets. New held traffic also spends the hold budget. Hold overflow
retains #6's counted drop-newest policy. Therefore no-loss acceptance requires
both cache and hold capacity, and successful coordination before the backstop.

## Two SRTP index spaces

For caller SSRC `C`, the relay index is `(caller ROC << 16) | caller sequence`.
Its half-space ROC estimate follows RFC 3711 section 3.3.1. The worker snapshot's
`SRTP.Inbound[C]` is the highest authenticated caller index. Replay drops every
packet at or below that value. A relay restored mid-call aligns its observed
ROC to the checkpoint before filtering; it cannot infer unknown historical
wraps from a clear 16-bit sequence alone. The existing half-space/rate envelope
still applies. Replay across wraps will get end-to-end failure coverage in #35.

The worker has a different outbound SSRC and sequence space. For an anchored
echo track, outbound sequence is `caller sequence + SeqOffset` modulo 65,536;
the outbound SRTP context tracks its own ROC. Resume adds #8's checked sequence
margin to `SeqOffset` and its outbound high water mark. This maps an input
processed again to a fresh outbound encryption index. #9's `ReplayFloor` is an
outbound floor protecting reserved frame-cache indexes. Neither that floor nor
the worker's outbound SSRC is a relay checkpoint filter. `SnapshotInboundIndexes`
returns only the caller map and checks the snapshot version.

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

The takeover resume result exposes `InputMayBeDuplicated` and
`InputDuplicationWindow`. The latter retains the largest conservative
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
request for the same checkpoint reuses the queue. Replay refuses a gate whose
checkpoint has not been supplied. A trimmed index above
the checkpoint, unavailable ring, metadata limit, or hold overflow marks the
plan incomplete. A complete plan tells resume to skip old frame-cache pictures
and proactive PLI, preserving the caller's codec chain. The existing outbound
margin/budget and frame-cache floor remain in force.

After the target resumes,
`ReplaySession` drains selected input and new held traffic on the normal private
UDP leg. The last empty-queue check and hold release share `forwardMu`, so a live
packet cannot overtake replay and push the 64-packet SRTP receive window past it.
Other sessions can forward between replay batches. SSRC streams are ordered
independently while retaining their interleaving slots. Held binding requests
are admitted again to preserve authenticated consent evidence.

Both operations are available through private HTTP:
`POST /sessions/{id}/begin-replay` takes source and nullable inbound indexes;
`POST /sessions/{id}/replay` takes the resumed target. Worker resume carries the
complete-plan flag through its existing private API. Neither relay endpoint
receives media keys. The bounded hold timeout ends an abandoned replay gate by
discarding queued ciphertext; it cannot identify an adopted live recipient
safely. Normal planned-move timeout behavior is unchanged. Every terminal
takeover loss forgets the session: this atomically discards the gate, held packets
and ring before any legacy release can flush input to a failed target.
Timeout/drop/send counters must be checked before claiming complete replay.
Retried HTTP outcomes, failed adoption, a second immediate crash and frame-cache
coexistence are intentionally #35's acceptance scope, not proven by this build.

## Pacing and spike evidence

Default replay batches contain at most 16 packets or 16 KiB, with one millisecond
between batches. A single oversized datagram may exceed the batch byte target,
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
The new #32 yardstick's first-content, late-delivery, concealment and freshness
measurements still need a run after its merge.

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
under the Mac's current shared load. No worker was intentionally stopped. The
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
