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
independently testable. Standalone wiring and #35 acceptance are described below.

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
handle repeated content; an agent's duplicated-input callback belongs to #39.

The window is determined by the exact snapshot's inbound map and retained
packets, rather than an assumed checkpoint cadence. #34's store-clock age seam
has not landed on this base. Its age metadata and policy must be integrated and
the age-window acceptance rerun after the orchestrator merges it. Fixed phase-1
counter margins and the existing resume-attempt budget are retained here.

## Gate and private protocol

After lease transfer, the control plane calls `BeginReplay` with a null inbound
map to install the gate, then fences the old leg by switching the route. This
precedes snapshot I/O: a stalled store must not leave the old owner forwarding.
A dead source needs no drain acknowledgement. The plane then reads the snapshot,
extracts its inbound map, and calls `BeginReplay` again with that exact map before
resume. An empty map is valid; null means that the checkpoint is not ready yet.
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
receives media keys. The existing bounded hold timeout releases abandoned work;
timeout/drop/send counters must be checked before claiming complete replay.
Retried HTTP outcomes, failed adoption, a second immediate crash and frame-cache
coexistence have #35 failure-path coverage described below.

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

## Failure paths and standalone acceptance (#35)

The standalone `relais-relay` now enables the bounded caller buffer by default.
`-buffer-off` restores phase-1 recovery. `-buffer-window`,
`-buffer-session-bytes`, `-buffer-total-bytes` and `-buffer-ssrcs` expose the
retention and admission bounds. All bounds must be positive. The core
`relay.Config.Buffer == nil` behavior remains available to embedded callers.

A lost replay HTTP reply exposed a failure beyond a lost adoption reply:
replay had already drained the queue and released the gate, but the next
control-plane attempt rebuilt that gate from the original checkpoint. The
already adopted worker then received the same authenticated ciphertext again.
The new real-actor HTTP test reproduced **38 worker decryption failures** on
memory before the fix. Confirmation now reuses the configured checkpoint and
resume bytes without calling `BeginReplay` again for the same uncertain tenure.
A remaining queue resumes from its delivered floor; a missing gate returns
`ErrHoldExpired`, without rebuffering input already delivered to that tenure.
The result reports `HoldExpired` and does not claim confirmed complete replay
when the reply was uncertain. Relay counters still show exactly one replay;
caller packet return and unique-frame decode establish that it completed.

`TestRelayBufferFailurePaths` exercises real workers, caller SRTP, and the
control-to-worker/control-to-relay private HTTP APIs on both session stores.
The frame-cache coexistence tests also use Redis frame storage in the Redis
lane. Failure injection lives in the harness tests, not production flags.

| Path | Memory outcome | Redis outcome | Caller / adopted-worker decryption failures | Gate released |
| --- | --- | --- | --- | --- |
| Target dies after storing its counter reservation, before adoption | Continued on next target | Continued on next target | 0 / 0 | Yes |
| All targets reject adoption | Clean loss, `LostCount=1` | Clean loss, `LostCount=1` | 0 / no adopted target | Yes |
| Adopted target's resume reply is lost | Same tenure confirmed | Same tenure confirmed | 0 / 0 | Yes |
| Completed replay's reply is lost | Continued, one replay | Continued, one replay | 0 / 0 | Yes |
| Target dies during paced replay | Continued on next target | Continued on next target | 0 / 0 | Yes |
| Target dies just after replay, before the reply | Continued on next target | Continued on next target | 0 / 0 | Yes |
| Caller sequence wraps inside the ring window, audio and video | Continued across wrap | Continued across wrap | 0 / 0 | Yes |
| Complete buffer and frame cache both enabled | Outage content first; old cache replay suppressed | Same | 0 / 0 | Yes |
| Incomplete buffer and frame cache both replay | Cached old picture first; new content from requested keyframe | Same | 0 / 0 | Yes |
| Buffer capacity overflow | PLI fallback, overflow counted | Same | 0 / 0 | Yes |
| Buffer time expiry | PLI fallback, expiry counted | Same | 0 / 0 | Yes |
| Actual relay restart with restored routes and an empty ring | PLI fallback enabled; outage packets retained by new relay | Same | 0 / 0 | Yes |

The continued complete-buffer scenarios also check zero outage packet loss,
no outbound SRTP index reuse, no requested keyframe, and full ffmpeg decoding
of every unique original picture. Second-crash tests check the intermediate
adopted worker before killing it too. The lost-session test checks lease
removal, a single counted loss, and immediate release of held bytes. Capacity
and expiry deliberately lose retained content; they prove decryptable media
continues through the existing keyframe path, not no-loss recovery outside
the bounds. A restarted relay cannot restore historical ciphertext, even when
it restores authenticated routes.

### Real-process comparison

`make crash-run CRASH_FLAGS="-compare-buffer"` interleaves ten buffer-on and
ten buffer-off SIGKILL trials in the existing loopback (`lan`) topology. Both
modes use the same frame-cache setting, checkpoint cadence, kill phase and
60-second post-takeover observation. `-frame-cache-off` can select PLI-only in
both comparison modes. The original cache/PLI comparison explicitly disables
the relay buffer, so its phase-1 metrics still measure the same recovery paths.
`-buffer` selects a single buffered mode for developer smoke runs.

The comparison builds a caller test executable once from the source checkout
and launches only `TestRelayBufferProcessCaller` in each trial. This reuses
#27's exact ciphertext-header packet observations and unique-picture decode
inside new harness test files, without changing #32's recorder, report or
public caller APIs. Relay, workers and control plane are the ordinary separate
binaries. Each process group is owned, stopped and reaped by the runner; Redis
is throwaway on a free loopback port. This mode requires the source checkout
and the Go tool, as does `make crash-run`.

The table separates audio/video returned, sent and lost outage packets, first
new source-content attribution, first-content delay, keyframe requests, relay
replay count, gate release and caller media gap. Source timestamps plus packet
position identify input after each tenure's sequence offset; duplicates cannot
hide a missing source packet. Outage spans SIGKILL to terminal takeover. These
are packet-return and codec-continuity measurements. They are not the #32
caller playout/concealment/freshness yardstick; its columns must be added and
acceptance rerun after that merge. Media gap is reported against phase 1's
**416 ms median**, with the same SIGKILL origin for both modes.

One local Redis process smoke (`-buffer -runs=1 -after=5s`) returned **20/20
audio and 12/12 video outage packets**, with **zero decryption failures and
zero keyframe requests**. First new source content was outage media at
**389.0 ms**; the gap was **406.0 ms**, 10.0 ms below the phase-1 median.
A second bounded on/off comparison returned 19/19 audio and 12/12 video
outage packets with the buffer, versus 0/22 audio and 0/13 video without it.
Its buffered gap was 402.2 ms and first outage content 385.2 ms; the baseline
gap was 460.3 ms and first requested-keyframe content 450.8 ms. Both had zero
caller decryption failures. These are developer observations on the shared
Mac, not ten-run acceptance or browser playout claims.

A separate `crash-run-buffer` non-PR CI job runs the twenty interleaved trials
and uploads `bin/crash-run-*` logs on failure. Its 35-minute budget avoids
combining two roughly 22-minute comparisons in the existing job. PR checks
are unchanged. **NOT-RUN locally:** ten-run/60-second acceptance, the nightly
full matrix and timing matrix. Their current merged-head results remain CI
requirements; the pending #27/#34 integration and #32 yardstick also need a
merged-head rerun.
