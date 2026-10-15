# ADR 0009: Replay unauthenticated pre-marker input after a planned export

Status: proposed for #127; evidence is added when the implementation lands.

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

The export snapshot, not the ACK, is the exact record of what the old worker
consumed. Export fences the session under the packet lock, so every datagram
handled before the fence is in the snapshot and every later one is discarded.
Datagrams that arrive between the ACK and the export are consumed and must not
be sent again.

## Decision

The relay replays retained caller media that the final export did not
authenticate. It does this once, when it releases the planned hold to the owner
that resumed from that export.

1. **The worker records a receive window.** Next to each caller SSRC's
   inbound high water mark `H` (`SRTP.Inbound`), the session state keeps a
   64-bit mask `SRTP.Received`: bit `i` means index `H-i` was authenticated.
   It is updated with `noteInbound`, after each successful decryption, under
   the packet lock. 64 matches the worker's SRTP replay window, so indexes at
   or below `H-64` can never be accepted again by any owner. An absent mask
   means unknown and is read as all ones: everything at or below `H` counts as
   consumed.
2. **The control plane passes the export's authenticated input to the relay.**
   After a successful export it reads `{High, Window}` per SSRC from the exact
   export bytes. The hold release becomes `ReleaseAfterExport(id, to, input)`.
   Rollback releases to the source with the same input. The retained
   uncertain-transfer path does the same when it resumes from the retained
   export without crash margins.
3. **The relay sends what the export missed, then the held queue.** Under
   `forwardMu`, the relay aligns the session ring's index space to the export
   (the ADR 0005 `align` step for a relay that restarted mid-call). It selects
   ring media whose index is above `High`, or within `High-63..High-1` with
   its window bit clear. For an SSRC that the export does not name, it selects
   all retained media of that SSRC, because the source never authenticated any
   of it. Indexes at or below `High-64` are skipped: SRTP would reject them as
   too old. Selected media and held input are deduplicated by `(SSRC, index)`,
   ordered per SSRC by `orderReplay`, and sent before the hold is removed. Live
   input cannot overtake them. A counter
   (`relais_relay_planned_recovered_total`) and the move result report
   recovered packets.
4. **The new owner restores the receive window.** On resume, the inbound SRTP
   context is primed at every authenticated index of the window, in ascending
   order, ending at `H`. This uses one primer context per SSRC. The restored
   replay window then matches the old owner's window, so SRTP replay
   protection rejects a duplicate of consumed input after the move, as it
   does without a move. This applies only when the primed index is the
   transport high water mark. The custom-agent replay floors of ADR 0006 still
   prime only their lower floor.

This reuses ADR 0005's retained ring, caller index space, ROC alignment and
per-SSRC ordering. It does not use the crash replay gate. Crash replay is
at-least-once from a possibly stale checkpoint. A planned export is exact, so
the planned path sends each missing index once, with no duplicate notice to
agents, no paced drain and no receipts. Planned resume flags and recovery do
not change.

### Behavior with the relay buffer disabled

With `relay.Config.Buffer == nil` (`relais-relay -buffer-off`, and the default
in-process harness relay) the relay keeps no copy of forwarded media.
`ReleaseAfterExport` then behaves exactly like `ReleaseSession`. A datagram
that reaches the old worker after its export is still lost: the #127 limit
remains in that mode. The standalone relay enables the buffer by default.

### Compatibility and upgrade order

Any upgrade order is safe; full recovery needs a new relay, control plane and
source worker.

| Mixed pair | Behavior |
| --- | --- |
| New control plane, old relay | `POST /sessions/{id}/release-after-export` returns 404 or 405. The control plane retries the existing `/release`. The limit remains until the relay upgrades. |
| Old control plane, new relay | The old `/release` is unchanged. |
| New source worker, old target worker | The extra JSON field is ignored by the old decoder. Recovery still works, but the old target primes only `H`. |
| Old source worker | No mask in the export. The control plane reads it as all ones, so only input above `H` is recovered. Nothing is sent twice. |

The snapshot version stays 7. The new field is optional, old readers ignore it,
and new readers treat its absence conservatively. A version bump would make old
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
- **Wait after the ACK before export.** This adds pause to every move and gives
  no bound on reordering.

## Limits

- Recovery depends on the ring: 1.5 s, 1 MiB per session and 64 MiB in total
  by default, and 16 SSRCs per session. Eviction before release, or an
  untracked SSRC, leaves the old loss. A hold that expires before release keeps
  ADR 0005's counted-loss behavior and recovers nothing.
- Caller SRTCP, STUN and DTLS are not retained. A late keyframe request or
  consent check reaching the old worker after export is still dropped. The
  caller retries both.
- Ring media from a confirmed caller address that failed authentication at the
  old worker counts as not consumed. If it is inside the window, it is sent
  once to the new owner and fails authentication there too. Crash replay has
  the same property.
- On a rollback to the source, the original late datagram can still arrive
  after the source resumes. SRTP replay protection rejects the second copy, so
  nothing is echoed twice. Today's accounting counts that rejection as one
  decryption failure.
- A session that once used cached-picture replay keeps a positive
  `ReplayFloor` (#9). Recovered video below the export's high water mark maps
  under that floor and is still dropped, as the existing floor rule states.
  Audio is unaffected.
- Recovery of reordering more than 63 indexes deep is out of scope; SRTP would
  reject that input without a move as well.
