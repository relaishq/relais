# ADR 0009: Replay unconsumed pre-marker input after a planned export

Status: implemented for #127 after two design reviews (revision 3), awaiting
the implementation review.

## Context

A planned move holds new caller input at the relay, then sends a drain marker
to the old worker on the private UDP leg. The worker acknowledges the marker
from its single read loop, so the ACK proves that the worker has read every
datagram that arrived before the marker. It does not prove that every datagram
the relay sent before the marker has arrived. UDP can reorder a caller datagram
behind the marker. If it reaches the old worker after the export fence, the
session is gone and the datagram is discarded. The new owner never receives
it, and the planned-move release sends only the held queue.

The opt-in reproduction (`TestPlannedMoveReorderedPrivateInput` at `7c86b62`)
lost exactly one audio unit per delayed datagram with a move, and none without
a move. A delayed datagram can
also be overtaken by later pre-marker datagrams. The export then has a high
water mark above the missing index, so a filter that keeps only indexes above
the high water mark is not enough.

The export snapshot, not the ACK, is the record of what the old worker
consumed. Export fences the session under the packet lock, so every datagram
handled before the fence is in the snapshot and every later one is discarded.
Datagrams that arrive between the ACK and the export are consumed and must not
be sent again.

The same reordering also affects the return path. A delayed datagram that
reaches the old worker after the ACK but before the export is consumed and
echoed. That echo follows the ACK on the worker leg. If the relay reads it
only after the planned route moved to the new owner, the relay drops it as
unrouted (`WorkerNoFlow`). The 20-call reorder stress reproduced this: two
echoes were dropped and one audio unit was lost although the inbound fix
recovered every late datagram.

Before this decision there was one gap between "authenticated" and
"consumed". For a custom
audio agent (ADR 0006), `flushAgent` sets `paused` and releases the packet
lock before the fence. Audio that arrives in that interval is authenticated
(the inbound high water mark advances) and then dropped by `enqueueAudio`.
`TestAgentPlannedFlushDrainsAcceptedQueue` exercises this drop.

## Decision

The relay replays retained caller media that the final export did not consume.
It does this once, when it releases the planned hold to the owner that resumed
from that exact export. Before it moves the route, it forwards the old owner's
output from before the export fence.

1. **Authenticated input is consumed input.** While a custom audio agent is
   paused for export, the worker does not authenticate that agent's audio:
   `handleSRTPPacket` returns before decryption for RTP whose clear payload
   type is the audio track's. Such audio stays outside the snapshot, so the
   relay recovers it and the target's agent receives it after restoring the
   exported state and progress. Video and SRTCP keep their unchanged paths
   (ADR 0006): they are authenticated, echoed or acted on, and recorded, as
   before. Echo already fences without a pause. If export fails and the
   session continues, the unpause reopens input; it now also covers a failed
   flush. These drops, which are outside this decision, stay as they are
   today and are not recovered:
   - agent queue overflow, quarantine and export-budget exhaustion
     (ADR 0006), which are counted;
   - the cache-burst video gate after a crash resume (#9).
2. **The worker records a receive window.** Next to each caller SSRC's
   inbound high water mark `H` (`SRTP.Inbound`), the session state keeps a
   64-bit mask `SRTP.Received`: bit `i` means index `H-i` was authenticated.
   It is updated with `noteInbound` after each successful decryption, under
   the packet lock. 64 matches the worker's SRTP replay window (`replayWindow`):
   Pion admits an unseen `H-63` and rejects `H-64`. An export without a mask
   (from an older worker) is inexact. A new worker that resumes such an export
   starts its window with every index at or below `H` marked consumed. This is
   conservative: it can only skip a recovery, never send input twice.
3. **The route moves only after the old owner's output is forwarded.** After
   a successful export the control plane switches the route with
   `MoveSessionAfterExport` (`POST /sessions/{id}/move-after-export`). Under
   the acknowledged planned hold, the relay first sends a second drain marker
   to the exported source and waits for its acknowledgement. The source's read
   loop answers it after the fence, so on the worker leg it follows every echo
   the source produced from input it consumed before the fence. The relay
   reads and forwards that output while the route still names the source,
   then moves the route. A lost marker or acknowledgement is resent every
   quarter of `OutputDrainTimeout` (100 ms by default). After the timeout the
   route moves as before, so a source that died after its export costs at most
   that bound. This uses the existing marker format, so every worker version
   acknowledges it. ADR 0008's exclusivity is unchanged: the old owner's output
   is accepted only while the route still names it. The retained
   uncertain-transfer path uses the same call when it resumes from the
   retained export without crash margins. Crash takeovers keep
   `MoveSessionAtEpoch`. A retry whose earlier route call failed no longer
   knows the routed source and cannot drain it. It uses the existing route
   repair: `MoveSessionAtEpoch` with an empty source, or `/move` with
   `repair`. It never sends an empty source to `move-after-export`, which
   rejects one with 400.
4. **The control plane passes the export's consumed input to the relay.**
   After a successful export it reads `{High, Window, Exact}` per SSRC from the
   exact export bytes. It releases the hold with `ReleaseAfterExport(id, to,
   input)` only when the new owner resumes from those bytes with margin zero:
   - a successful move;
   - a rollback that resumes the source from the same export;
   - the retained uncertain-transfer path when it resumes from the retained
     export without crash margins.

   A transition to crash margins resumes from a store checkpoint that may
   predate the export, so it keeps `ReleaseSession`.
5. **The relay selects only unconsumed media.** Under `forwardMu` the relay
   does the following:
   - **Aligns its ring.** It rebuilds every retained index of an SSRC around
     the export's anchor: `inboundIndex(H, uint16(index))`, within the RFC
     3711 half-space envelope. The ring's high water mark, trims and lowest
     index are recomputed the same way. A relay restored mid-call starts its
     own rollover counter (ROC) at zero and cannot represent a sequence
     observed before a wrap it never saw. No single offset can then align a
     reordered cold ring: at `H = 65535`, sequences 0 and then 65534 are stored
     as indexes 0 and 65534, but their true indexes are 65536 and 65534. The
     former shared `indexTranslation` (a single offset) kept index 0 and
     filtered it as too old. ADR 0005's crash replay used the same helper and
     gains the same correction. Retained packets span at most the ring window,
     far inside the envelope, so a warm ring keeps its indexes.
   - **Builds candidates.** Candidates are retained ring media and held media,
     deduplicated by `(SSRC, index)`. When an index is in both, the held copy
     wins so it keeps its slot next to held consent checks.
   - **Keeps a candidate** when one of these holds:
     - the export has an exact window for its SSRC and the index is above
       `H`, or in `H-63..H-1` with its bit clear;
     - the export has an inexact window and the index is above `H`; held media
       at or below `H` is then kept unfiltered, as today, because it cannot be
       classified;
     - the export does not name the SSRC, because the source never
       authenticated any of it.

     Indexes at or below `H-64` are dropped, because SRTP would reject them as
     too old.
   - **Sends and releases.** Non-media held packets keep their slots. Kept
     media is ordered per SSRC by `orderReplay` and sent before the hold is
     removed, so live input cannot overtake it.
   - **Reports.** The reply carries `Packets`, `Recovered`, `Filtered`,
     `Complete` and `LockHeld` (how long the release held `forwardMu`).
     `Complete` is false when ring eviction or SSRC admission limits covered an
     unconsumed window index that the ring had observed: indexes below an SSRC's
     lowest observed index are never counted. That lowest index is lifetime
     metadata and may be rollovers old. Alignment shifts it by the high water
     mark's correction instead of rebuilding it from 16 bits, which would move
     it into the anchor's epoch and hide eviction after a wrap. The relay exports
     `relais_relay_planned_releases_total`, `_recovered_total`,
     `_filtered_total`, `_incomplete_total`,
     `relais_relay_planned_release_lock_nanoseconds_total` and the
     `relais_relay_planned_release_lock_max_nanoseconds` gauge. The move
     result reports `RecoveredPackets` and `ReleaseLockHeld`.
6. **The new owner restores the receive window.** On resume, when the primed
   index is the transport high water mark and the mask is exact, the inbound
   SRTP context is primed at every authenticated index of the window. Priming
   runs in ascending order, ending at `H`, with one primer context per SSRC.
   The receiver ROC is set once, before the first primer, so later primers
   cross a wrap by normal estimation. The restored replay window then matches
   the old owner's, so SRTP rejects a duplicate of consumed input after the
   move, as it would without a move. ADR 0006's custom-agent replay floors still
   prime only their lower floor.
7. **Replay rejection is not a decryption failure.** The worker counts SRTP and
   SRTCP replay-window rejections (Pion's `duplicated packet`) in a separate
   session counter and `relais_worker_replay_rejections_total`.
   `SessionDecryptFailures` and `relais_worker_decryption_failures_total` then
   count authentication and decryption failures only. A rollback can make the
   original late datagram arrive after its recovered copy; that is then one
   replay rejection.

   This split must not let an existing gate miss duplicates. Every existing test
   or gate that requires zero worker decryption failures also requires zero
   replay rejections, unless its scenario deliberately delivers a duplicate,
   and then it asserts the exact count. The caller's own counts are unchanged.
   A caller-side replay rejection means reused outbound SRTP indexes and stays
   a decryption failure.
8. **ReplayFloor after an exact planned resume (#9).** A resume with margin
   zero and no duplicate notice leaves a positive `ReplayFloor` unchanged only
   when it actually restored the video track's inbound window from an exact
   export (`exactPlannedVideo`). Restoration sets a per-session flag for the
   track's inbound SSRC only after `restoreInboundWindow` succeeds. Mask
   presence alone, margin zero alone, or a window restored for another SSRC
   is not enough. Every other resume still advances it to
   `HighestSentIndex`, as today. The rule "never echo a rewritten source index
   at or below `ReplayFloor`" is unchanged. Proof that no outbound SRTP index
   is reused:
   - **Burst indexes stay protected.** The floor still covers every
     cached-picture burst index, because it is never lowered.
   - **Above the floor, the mapping is one-to-one.** The outbound sequence is
     the caller index plus `SeqOffset`. A margin-zero resume does not change
     `SeqOffset`, so above the floor each outbound index belongs to exactly
     one caller index.
   - **Each caller index is echoed at most once.** With the window restored,
     the target rejects every caller index at or below `H` that any earlier
     owner in this chain accepted: in-window ones are duplicates and older
     ones are too old.
   - **Earlier inexact resumes are covered.** A crash resume, or a resume by a
     worker without this decision, advanced the floor at that time, and a crash
     margin moved `SeqOffset` past the window.
   - **Why the old rule advanced.** The floor advanced on planned resumes
     because a fresh replay window could accept consumed input again.
     `continueAfterReplay` can change its timestamp, so re-echoing at the same
     index would encrypt a different plaintext. Item 5 removes that
     possibility. That item makes the old reason obsolete only for this exact
   case.

This reuses ADR 0005's retained ring, caller index space, corrected ROC
alignment and per-SSRC ordering. It does not use the crash replay gate. Crash
replay is at-least-once from a possibly stale checkpoint. A planned export is
exact, so the planned path sends each missing index once, with no duplicate
notice to agents, no paced drain and no receipts. Planned resume flags and
keyframe recovery do not change.

### How often ReplayFloor is positive

`ReplayFloor` becomes positive only in `reserveReplay` (`framecache.go`). That
needs a crash-margin resume with neither `RelayReplay` nor `RelayInputReplay`,
a worker-owned socket, negotiated video, an enabled frame cache, and an
admitted cached group. Once positive, it stays positive for the rest of the
call. In the default process configuration, the worker frame cache is on and
the relay buffer is on. A crash takeover then normally has a complete or
partial ring with selected media and skips cached pictures. The floor
therefore becomes positive in these cases:

- **Buffer on, empty or unavailable ring:** a crash takeover of a video call
  whose relay ring is empty or unavailable. This happens after a relay restart
  or standby activation, after ring eviction, or when the relay lacks the
  buffer.
- **Buffer on, uncertain export:** a takeover after an uncertain planned export
  (`BeginReplay` returns `ErrHeld`).
- **Buffer off:** every crash takeover of a video call with a cached group.

Only video gaps below `H` are affected. A tail index above `H` maps above
`HighestSentIndex`, and audio has no floor. Item 8 removes the effect for
exact planned resumes. It remains when a resume without an exact window
happened within the last 64 video indexes.

### Behavior with the relay buffer disabled

With `relay.Config.Buffer == nil` (`relais-relay -buffer-off`, and the default
in-process harness relay) the relay keeps no copy of forwarded media.
`ReleaseAfterExport` then still filters held media against an exact window, and
otherwise releases like `ReleaseSession`. The output drain marker (item 3)
needs no buffer, so the return-path fix applies in this mode too. A datagram
that reaches the old worker after its export is still lost: the #127 limit
remains in that mode. This is an
accepted scope exception; no always-on journal is added. The standalone relay
enables the buffer by default.

### Compatibility and upgrade order

**Upgrade every worker first, then the relay and the control plane in either
order.** Every intermediate state is no worse than today. The full guarantee
needs all three upgraded.

| Mixed state | Behavior |
| --- | --- |
| New control plane, old relay | `POST /sessions/{id}/move-after-export` and `/release-after-export` return 404 or 405 from the relay's mux. The control plane retries the existing `/move` and `/release` only for those two statuses. Every relay domain error is 409; uncertain and other errors are returned and never retried. The #127 limit remains until the relay upgrades. |
| Old control plane, new relay | The old `/move` and `/release` are unchanged. |
| Old source worker | No mask in the export, so input is inexact. Only ring input above `H` is recovered; held media stays unfiltered, as today. Nothing is sent twice by the release. |
| New source, old target worker | The relay sends only unconsumed media, including held media. The old target ignores the mask: it primes only `H`, so a later live duplicate is admitted as today. It also advances a positive `ReplayFloor`, so recovered video gaps on such sessions are still dropped. |
| New workers, old relay or control plane | Custom-agent audio that arrives during the export flush is no longer authenticated. Without recovery it is lost, as it is dropped today, but it is no longer counted as an agent input drop. Video and SRTCP are unchanged. |
| Any worker, new relay | The output drain marker uses the existing marker format, so every worker acknowledges it. |

The snapshot version stays 7. The new field is optional, old readers ignore it,
and new readers treat its absence as inexact. A version bump would make old
workers reject every new snapshot during a rolling deploy.

## Rejected alternatives

- **Sequence-aware marker.** The relay names what it forwarded and the worker
  delays its ACK until it has consumed that input or a bound passes. This needs
  a session-scoped marker format, so old workers never ACK, and moves abort
  after the one-second barrier timeout during a rolling deploy. Every private-leg
  loss would wait for the full bound, and input delayed beyond the bound would
  still be lost. It would work with the buffer off.
- **The ACK reports consumed input.** Datagrams that arrive between the ACK and
  the export are consumed, so an ACK-time report would send them twice. It also
  needs a new UDP format.
- **The old worker hands late datagrams back to the relay.** This needs a new
  worker-to-relay message, a per-caller tombstone after export and loop
  protection. A datagram that is lost on the private leg, or that arrives
  after the tombstone expires, is still lost. If it returns after the new owner
  has advanced 64 indexes, SRTP rejects it.
- **Reuse the crash gate (`BeginReplay`/`ReplaySession`) with the export's
  high water mark.** Its filter keeps only indexes above the checkpoint and
  misses an overtaken datagram. It also adds a second relay round trip before
  release, and its paced drain, receipts and duplicate notices are not needed
  for an exact export.
- **Accept the old owner's output for a grace period after the route moves.**
  This would weaken ADR 0008's exclusivity. The zombie-fencing test
  (`TestRelayZombieFencing`) rejects any output from the old leg after a move,
  and an uncertain export cannot prove the old owner is fenced. The output
  drain marker keeps exclusivity and costs one private-leg round trip.
- **Pause all caller RTP during an agent flush.** Revision 2 proposed this. It
  would stop video and SRTCP for the flush, so with the buffer off, or before
  the relay is upgraded, a delayed video packet would be lost where it is
  echoed today.
- **Clear window bits for input the agent drops**, instead of item 1. Overflow
  and quarantine drops happen without a move too. The relay would then deliver
  that old input late, and bit 0 could describe an unconsumed `H`, which would
  need a separate SRTP priming point.
- **Wait after the ACK before export.** This adds pause to every move and gives
  no bound on reordering.

## Limits

- Recovery depends on the ring: 1.5 s, 1 MiB per session and 64 MiB in total
  by default, and 16 SSRCs per session. Eviction before release, or an
  untracked SSRC, leaves the loss, and the reply reports `Complete=false`. A
  hold that expires before release keeps ADR 0005's counted-loss behavior and
  recovers nothing. Relay shutdown or replacement during a move discards the
  hold and ring, so that move keeps today's loss.
- Crash-margin transitions in the planned path keep `ReleaseSession` and the
  #127 limit.
- Reordering on the worker-to-relay leg is not covered. An echo reordered
  behind the output drain acknowledgement can still arrive after the route
  moved; this race exists today for any move. The reproduced and fixed race is
  relay-to-worker reordering.
- If the output drain marker times out (`OutputDrainTimeout`), the route
  moves without it; a pre-fence echo still in flight can then be dropped, as
  today.
- Caller SRTCP, STUN and DTLS are not retained. A late keyframe request or
  consent check reaching the old worker after export is still dropped. The
  caller retries both.
- Ring media from a confirmed caller address that failed authentication at the
  old worker is not consumed. If it is inside the window, it is sent once to
  the new owner and fails authentication there too. Crash replay has the same
  property.
- Reordering deeper than 63 indexes is out of scope; SRTP would reject that
  input without a move as well.
- The `ReplayFloor` residual described above.

## Tests

End-to-end cases run with the relay buffer on, through per-worker private-leg
UDP proxies and the relay's real private HTTP API. Each requires zero lost
content units, zero worker and caller decryption failures, zero worker replay
rejections unless a case asserts an exact count, each unit and packet returned
once, one authenticated decryption per caller index across workers (the
`workerprobe` decrypt hook), and a call connected throughout with no ICE
restart or renegotiation.

| Plan item | Test |
| --- | --- |
| Tail and below-`H` gaps, audio and video | `TestPlannedMoveReorderedPrivateInput` (ordered, audio tail, audio gap, video gap, no move); relay `TestReleaseAfterExportRecoversUnconsumedInput` |
| Sparse masks, independent SSRCs, index 0, `H < 63`, `H-63` sent and `H-64` not, unknown SSRC, held duplicates, inexact export | `TestReleaseAfterExportRecoversUnconsumedInput` |
| Window tracking, priming across a wrap and below 63 | `TestReceiveWindowTracksAuthenticatedInput`, `TestRestoreInboundWindowMatchesExporter` |
| Cold-ring rollover, `[0, 65534]` at `H=65535` and `[1, 65535]` at `H=65536`, for planned release and crash replay | `TestColdRingRolloverAlignment` (fails on the old single-offset alignment in all four cases) |
| Eviction after a rollover, with a lifetime lowest index from an earlier epoch, for planned release and crash replay | `TestRolloverEvictionIsReported` (the planned case reported `Complete=true` before the fix) |
| Combined rollover and reordering | `TestPlannedDrainReorderedPrivateInput` starts each call's audio and video sequences just below 65535, staggered so they wrap during the drain and move rounds under reorder injection; the cold-ring unit cases pin the boundary |
| Agent arrivals during the export flush | `TestPlannedMoveAgentFlushRecoversLateAudio`, `TestAgentPlannedFlushDrainsAcceptedQueue` |
| Agent pause leaves video and SRTCP active | `TestPausedAgentLeavesVideoAndRTCPActive` |
| Prior positive `ReplayFloor`; the floor condition | `TestExactPlannedResumeKeepsReplayFloor` (restored window keeps it; mask without restore, crash margin, or another SSRC's window advances it); `TestReplayLatePacketsNeverReuseIndex` unchanged |
| Rollback with the original before and after the replay | `TestPlannedMoveRollbackRecoversLateInput` |
| Replay rejection counted apart | `TestReplayRejectionIsNotDecryptFailure` |
| Return-path output drain, its bound and resend | `TestMoveSessionAfterExportForwardsPreFenceOutput`, `TestMoveSessionAfterExportBounds` |
| Success, rollback, crash-margin transition | `TestMoveReleasesAfterExactExport` |
| Retained uncertain transfer | `TestRetainedPlannedMoveReleasesAfterExport`, `TestRelayUncertainTransferSurvives/deferred-move` |
| Retained retry after a failed route call, through the real relay HTTP handler | `TestRetainedPlannedMoveRetriesAfterRouteFailure` (never adopted in three attempts before the fix); `TestRemoteMoveAfterExportFallback` covers the empty-source repair |
| Lost reply on the route or release call | `TestHTTPMoveLostReplies/forward-route`, `/release-after-export` |
| Hold expiry, eviction, buffer off | `TestReleaseAfterExportEdges`, `TestReleaseAfterExportReportsEviction`, `TestReleaseAfterExportWithoutBuffer`, the buffer-off subtest |
| Relay restart and standby activation | Cold-ring cases above; `TestRelayBufferFailurePaths/*/relay-restart` and the `relay-continuity` CI jobs |
| Older relay, older control plane, mask removed | `TestRemoteReleaseAfterExportFallback`, `TestRemoteMoveAfterExportFallback`, the older-relay subtest, `TestSnapshotConsumedInputReportsExactness` |
| Many concurrent moves under reordering | `TestPlannedDrainReorderedPrivateInput`: 20 calls (10 under the race detector), a drain plus six move rounds, every leg holding, overtaking and delaying datagrams past drain markers; requires zero loss, zero `WorkerNoFlow`, a drain marker per move and positive recovery |

## Evidence

All local runs were on the shared development Mac (12 cores) while other
sessions kept its load average between 7 and 75, so local timings are noisy;
A/B pairs were interleaved.

**Before the fix.** At `7c86b62`, the opt-in reproduction lost exactly one
audio unit with a move and none without one.

**Regression tests.** Each figure is with the race detector unless marked.

| Test | Result |
| --- | --- |
| `TestPlannedMoveReorderedPrivateInput` (7 subtests) | 20/20 |
| `TestPlannedDrainReorderedPrivateInput`, 10 calls, 70 moves per run | 6/6 (3 with wrapping sequences, the last asserting all 20 streams wrapped); 105–136 late datagrams recovered per run |
| Same, 20 calls without the race detector, 140 moves per run | 8/8 (3 with all 40 streams wrapping); 191–273 recovered per run; zero `WorkerNoFlow`, zero loss, zero decryption failures and replay rejections, a drain marker acknowledged per move, zero kernel UDP receive-buffer drops |
| Return path before the output drain marker existed | The 20-call stress dropped two old-owner echoes (`WorkerNoFlow=2`) and lost one audio unit, with 86 late datagrams recovered |
| Cold-ring rollover cases | Fail on the old single-offset alignment in all four cases (planned and crash), pass now |

**Cost on the common path.**

- **Release.** A release with a 120-packet ring and ten held packets takes
  59 µs plain and 74 µs after export, so the scan, filter and sort add
  about 15 µs under `forwardMu`. In the 20-call reorder stress without
  the race detector, the lock hold per release was p50 35–56 µs, p99
  0.34–4.6 ms and at most 7 ms. That includes the sends of held packets,
  which a plain release also makes, and host preemption.
- **Priming.** Restoring a full 64-index window costs 25.5 µs per SSRC,
  against 4.3 µs for priming only the high water mark.
- **In-process A/B, no reordering.** 20 buffered calls, a drain plus six
  rounds, 140 moves per run, eight interleaved pairs with `7c86b62`:
  - median of run medians for move duration: 5.85 ms on main, 6.6 ms here;
  - caller audio gap p99: 26.4 ms on main, 27.1 ms here;
  - one run here had a load outlier, with 50.8 ms audio p99.
- **Release lock under CI stress.** The CI runner's 20-call stress held
  `forwardMu` for at most 24 ms per release, p99 12 ms. That was with every
  call proxied and the runner saturated (move p50 67 ms); it bounds a
  saturated host, not the added work.
- **CI timing gates** (default harness topology without the buffer, no race
  detector; main run 38677108873 at `7c86b62` against run 38698125116):
  - 20 single-call moves, median gap: 33.19 and 33.59 ms on main, 33.34
    and 33.37 ms here; maximum: 33.98 and 34.13 ms on main, 33.99 and
    34.00 ms here;
  - 10-call drain total: 5.1 and 12.7 ms on main, 8.6 and 19.7 ms here
    (one output drain round trip per move); largest call gap: 37.7 and
    33.2 ms on main, 40.6 and 34.1 ms here.
- **netns `lossy-dc` move profile, 20 trials, first run (38693196625).**
  Compared with main run 38570305475:
  - move duration median: 23.5 ms on main, 27.5 ms here;
  - the route call: `/move` 1.75 ms, against 3.63 ms for
    `/move-after-export`, which adds one private round trip of about 1.5 ms
    in this profile;
  - resume: 9.3 ms on main, 10.3 ms here;
  - caller audio gap median: about 32 ms in both;
  - video gap p95: 40 ms on main, 58 ms here. At this profile's move phase,
    the longer pause holds one more video frame. It stays inside the gate:
    baseline plus 50 ms, and at most 150 ms.
- **netns `lossy-dc` second run (38698125116).** It passed:
  - move duration median: 25.0 ms here, 23.7 ms on main;
  - resume: 9.3 ms in both;
  - audio p95: 37.2 ms here, 39.9 ms on main;
  - video p95: 50.3 ms here, 51.9 ms on main;
  - audio and video median gaps: 34.1 and 34.6 ms here, 33.2 and 34.1 ms
    on main.

  The first run's larger shift was partly runner variance. Its three
  recovered packets were real reordering from the profile's netem.

**CI.**

- **Run 38698125116 at `75d646f`** (manual dispatch on this branch): every
  job passed. That covers:
  - quality, unit tests with Redis, benchmarks;
  - the full race harness on both stores, including the 10-call stress with
    every stream wrapping and 70 packets recovered;
  - the non-race timing gates, including the 20-call stress with 246
    recovered, zero kernel UDP drops and every stream wrapping;
  - crash-run, crash-run-buffer, relay restart and standby continuity,
    store-stall, the netns crash run and all eight network-profile cells.
- **The earlier run 38693196625** failed for two reasons:
  - The race stress used 20 calls through proxies, and a drain marker timed
    out at the one-second barrier bound. The race variant now uses 10 calls.
  - `TestRelayBufferFailurePaths/redis/failed-adoption-loss` failed. It also
    fails on main runs 38675977770, 38570305475 and 38422671729 with the same
    "no terminal path retains a gate" message.

  Its `lossy-dc` barrier-resend probe failed one trial with a 316 ms gap. The
  move trace shows the control plane's `/resume` call took 214 ms, while the
  proxied worker answered in 9 ms. Control and driver each counted one TCP
  loss probe: the tail-loss class of ADR 0007, not this change. Main run
  38570305475 had a 1,047 ms resume outlier.

