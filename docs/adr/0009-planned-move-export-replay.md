# ADR 0009: Replay unconsumed pre-marker input after a planned export

Status: proposed for #127 (revision 2, after design review). Evidence is
added when the implementation lands.

## Context

A planned move holds new caller input at the relay, then sends a drain marker
to the old worker on the private UDP leg. The worker acknowledges the marker
from its single read loop, so the ACK proves that the worker has read every
datagram that arrived before the marker. It does not prove that every datagram
the relay sent before the marker has arrived. UDP can reorder a caller datagram
behind the marker. If it reaches the old worker after the export fence, the
session is gone and the datagram is discarded. The new owner never receives
it, and the planned-move release sends only the held queue.

`TestPlannedMoveReorderedPrivateInput` shows exactly one lost audio unit per
delayed datagram with a move, and none without a move. A delayed datagram can
also be overtaken by later pre-marker datagrams. The export then has a high
water mark above the missing index, so a filter that keeps only indexes above
the high water mark is not enough.

The export snapshot, not the ACK, is the record of what the old worker
consumed. Export fences the session under the packet lock, so every datagram
handled before the fence is in the snapshot and every later one is discarded.
Datagrams that arrive between the ACK and the export are consumed and must not
be sent again.

One gap exists today between "authenticated" and "consumed". For a custom
audio agent (ADR 0006), `flushAgent` sets `paused` and releases the packet
lock before the fence. Audio that arrives in that interval is authenticated
(the inbound high water mark advances) and then dropped by `enqueueAudio`.
`TestAgentPlannedFlushDrainsAcceptedQueue` exercises this drop.

## Decision

The relay replays retained caller media that the final export did not consume.
It does this once, when it releases the planned hold to the owner that resumed
from that exact export.

1. **Authenticated input is consumed input.** While a custom agent is paused
   for export, the worker does not authenticate caller RTP: `handleSRTPPacket`
   returns before decryption, as it does after the fence. Such input stays
   outside the snapshot, so the relay recovers it and the target's agent
   receives it after restoring the exported state and progress. Echo already
   fences without a pause. SRTCP processing is unchanged. If export fails and
   the session continues, the unpause reopens input; it is extended to cover a
   failed flush too. These drops,
   which are outside this decision, stay as they are today and are not
   recovered:
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
3. **The control plane passes the export's consumed input to the relay.**
   After a successful export it reads `{High, Window, Exact}` per SSRC from the
   exact export bytes. It releases the hold with `ReleaseAfterExport(id, to,
   input)` only when the new owner resumes from those bytes with margin zero:
   - a successful move;
   - a rollback that resumes the source from the same export;
   - the retained uncertain-transfer path when it resumes from the retained
     export without crash margins.

   A transition to crash margins resumes from a store checkpoint that may
   predate the export, so it keeps `ReleaseSession`.
4. **The relay selects only unconsumed media.** Under `forwardMu` the relay
   does the following:
   - **Aligns its ring.** It aligns the ring's caller index space to the
     export. The translation anchors the relay's locally observed high index
     in the export's rollover-counter (ROC) space,
     `delta = inboundIndex(H, uint16(localHigh)) - localHigh`. The current
     `indexTranslation` instead extends `H`'s sequence number in the relay's
     space. That cannot go below ROC 0, so a relay that restarted mid-call and
     first observed a sequence just after a wrap mislabels the ring by one ROC.
     For example, `H = 65535` with first observed sequence 0 keeps index 0
     instead of 65536. The fix replaces the shared helper, so ADR 0005's crash
     replay gains the same correction.
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
   - **Reports.** The reply carries `Packets`, `Recovered`, `Filtered` and
     `Complete`. `Complete` is false when ring eviction or SSRC admission
     limits covered an unconsumed window index. A counter
     (`relais_relay_planned_recovered_total`) and a histogram of the release's
     `forwardMu` hold time (`relais_relay_planned_release_lock_seconds`) are
     exported.
5. **The new owner restores the receive window.** On resume, when the primed
   index is the transport high water mark and the mask is exact, the inbound
   SRTP context is primed at every authenticated index of the window. Priming
   runs in ascending order, ending at `H`, with one primer context per SSRC.
   The receiver ROC is set once, before the first primer, so later primers
   cross a wrap by normal estimation. The restored replay window then matches
   the old owner's, so SRTP rejects a duplicate of consumed input after the
   move, as it would without a move. ADR 0006's custom-agent replay floors still
   prime only their lower floor.
6. **Replay rejection is not a decryption failure.** The worker counts SRTP and
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
7. **ReplayFloor after an exact planned resume (#9).** A planned resume with
   margin zero, whose video inbound window was restored exactly, leaves a
   positive `ReplayFloor` unchanged. Every other resume still advances it to
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
     possibility.

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
`HighestSentIndex`, and audio has no floor. Item 7 removes the effect for
exact planned resumes. It remains when a resume without an exact window
happened within the last 64 video indexes.

### Behavior with the relay buffer disabled

With `relay.Config.Buffer == nil` (`relais-relay -buffer-off`, and the default
in-process harness relay) the relay keeps no copy of forwarded media.
`ReleaseAfterExport` then still filters held media against an exact window, and
otherwise releases like `ReleaseSession`. A datagram that reaches the old worker
after its export is still lost: the #127 limit remains in that mode. This is an
accepted scope exception; no always-on journal is added. The standalone relay
enables the buffer by default.

### Compatibility and upgrade order

**Upgrade every worker first, then the relay and the control plane in either
order.** Every intermediate state is no worse than today. The full guarantee
needs all three upgraded.

| Mixed state | Behavior |
| --- | --- |
| New control plane, old relay | `POST /sessions/{id}/release-after-export` returns 404 or 405 from the relay's mux. The control plane retries the existing `/release` only for those two statuses. Every relay domain error is 409; uncertain and other errors are returned and never retried. The #127 limit remains until the relay upgrades. |
| Old control plane, new relay | The old `/release` is unchanged. |
| Old source worker | No mask in the export, so input is inexact. Only ring input above `H` is recovered; held media stays unfiltered, as today. Nothing is sent twice by the release. |
| New source, old target worker | The relay sends only unconsumed media, including held media. The old target ignores the mask: it primes only `H`, so a later live duplicate is admitted as today. It also advances a positive `ReplayFloor`, so recovered video gaps on such sessions are still dropped. |
| New workers, old relay or control plane | Agent input that arrives during the export flush is no longer authenticated. Without recovery it is lost, as it is dropped today, but it is no longer counted as an agent input drop. |

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

## Test plan

Phase B harness tests for the fix run with the relay buffer enabled. Each
end-to-end case requires the following:

- zero lost content units;
- zero worker and caller decryption failures;
- zero worker replay rejections, unless the case asserts an exact count;
- each audio unit and video packet returned exactly once (no repeated returns
  or duplicate packets);
- one authenticated decryption per caller index across workers, checked with
  the `workerprobe` decrypt hook;
- connected throughout, with no ICE restart or renegotiation.

**Index cases** (relay unit tests on `ReleaseAfterExport`, worker unit tests on
the mask and priming, and harness reorder injection):

- tail and below-`H` gaps for audio and video;
- sparse masks;
- index 0, and `H < 63` without underflow;
- `H-63` recovered and accepted by the target, and `H-64` never sent;
- rollover, including the cold-ring cases on both sides of 65535/65536 for
  planned release and for `BeginReplay`;
- independent audio and video SSRCs with different masks;
- held duplicates of consumed media filtered;
- an SSRC unknown to the export.

**Custom-agent cases:**

- arrivals during the export flush are not authenticated, are recovered, and
  are processed once by the target's agent (`TestAgentPlannedFlushDrainsAcceptedQueue`
  is updated to expect no authentication and no agent drop);
- a session with a prior positive `ReplayFloor`. A recovered gap video packet
  is echoed at a fresh outbound index, and caller-side decryption shows no
  reuse. A consumed duplicate is rejected. Burst indexes are never echoed
  again. Without an exact window the floor still advances (the existing
  `TestReplayLatePacketsNeverReuseIndex/true` stays);
- rollback with the original late datagram arriving before and after the
  recovered copy: exactly one echo, one replay rejection and zero decryption
  failures;
- exact duplicate-delivery counts in every case.

**Move-path cases:**

- a retained uncertain transfer resumed from the retained export;
- an uncertain resume reply followed by confirmation: one release, a second
  release returns `ErrHoldExpired`, nothing is sent twice;
- a lost `release-after-export` reply;
- the transition to crash margins uses `ReleaseSession`;
- hold expiry before release;
- ring eviction reports `Complete=false`;
- a relay restart and a standby activation before a planned move, with an
  aligned cold ring;
- a relay restart during a hold loses it, as stated.

**Compatibility cases**, through the real private HTTP handlers:

- an export with its mask removed (inexact path);
- an old relay simulated by a handler without the new route: 404 falls back,
  while 409 and uncertain errors do not;
- an old control plane on `/release`;
- a target resumed without window priming: no duplicate from the release.

**Regressions:**

- `TestPlannedMoveReorderedPrivateInput` becomes a regular test with ordered,
  tail, gap, video, no-move and agent subtests: 20/20 with `-race`;
- a 20-call drain plus repeated move rounds through per-worker private-leg
  reorder proxies. The proxies delay in-flight datagrams past each marker,
  overtake them with later pre-marker datagrams, and swap adjacent datagrams.
  The test checks zero lost units and a positive recovered count, repeated;
- the existing PR checks (`make lint`, `make vet`, `make test-unit` with Redis,
  `make test-harness-short`, the `TestRelayUncertainTransferSurvives` race run
  and `make test-harness-smoke`) and the relay-buffer and crash suites.

**Measurements**, recorded in this ADR:

- concurrent drain move duration and caller audio and video gap p50/p99/max,
  for this branch against main, on the same machine and topology;
- kernel UDP receive-buffer drops before and after each stress run (macOS
  `netstat -s -p udp`, Linux `/proc/net/snmp`);
- `forwardMu` hold time per release (histogram p50/p99/max) during the
  20-call drain;
- resume priming cost per SSRC.

Acceptance requires the existing move gap budget (soak audio pause p99
about 29 ms), no added socket drops, and release lock hold times well below
one audio frame interval.
