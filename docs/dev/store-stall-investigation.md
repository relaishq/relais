# Ticket 95: expiry during real-process store stalls

Investigation only. Product code and ADR policy are unchanged. Checkout:
`feat/95-store-stall`, base `765db53`. Each trial uses the driver's throwaway
Redis and separate relay/control/two-worker processes, with one loopback caller.

## Live workers, no SIGKILL

Three five-second and three 3.5-second store stalls all stopped caller media.
No call survived. All six callers had zero decryption failures, ICE restarts,
and renegotiations. They later transitioned from connected to disconnected to
failed within the 35-second post-resume observation. The worker processes
remained alive and healthy, with zero sessions. Control reported no live calls,
no takeover, no loss, and no detection event. Both envelope policy counters
remained zero. The relay's active route count was zero.

The three 3.5-second trials sampled this state immediately after SIGCONT as
well as at the end. Their first samples already showed zero worker sessions,
zero relay routes, and missing lease/snapshot. The five-second trials sampled
at the end; their caller records also show media ending near SIGCONT, rather
than a later consent timeout. These are observed failures of availability and
terminal accounting, not decryption failures.

The worker does not re-create an expired session:

- `pkg/mediaworker/worker.go:292`: default ownership lease TTL is three seconds.
- `pkg/sessionstore/redis.go:218`: lease and snapshot use the same absolute
  expiry deadline. `redis.go:269` rejects an unmatched renewal instead of
  creating ownership; `redis.go:410` maps that rejection to `ErrLeaseLost`.
- `pkg/mediaworker/worker.go:742`: renewal uses the original lease token.
  `worker.go:749` sets `fenced` on `ErrLeaseLost`; `worker.go:756` closes the
  session. Transient store errors merely log and retain the session.
- `pkg/mediaworker/snapshot.go:152`: a checkpoint write that finds lease loss
  also fences and closes the session. Either write or renewal can win this
  race; the observations do not identify which one ran first.
- `pkg/mediaworker/session.go:434`: fenced sessions stop handling RTP.
  `pkg/mediaworker/demux.go:73` suppresses DTLS writes after fencing, including
  close notification. `pkg/mediaworker/session.go:691` removes local session
  state. Consequently, the caller initially has a connected but silent
  transport and later detects failure through ICE.

Control's `/status` forgets a call when `store.Get` returns `ErrNotFound`
(`pkg/controlplane/controlplane.go:827`). Its `forget` only removes the call
from the map (`controlplane.go:243`); it does not run terminal loss accounting
or relay cleanup. The worker remains healthy because heartbeats are separate
from session ownership.

The relay can keep forwarding established media while the store is unavailable.
An answered-route STUN request schedules an owner refresh
(`pkg/relay/relay.go:732`). When that lookup definitively returns `ErrNotFound`,
the relay drops the cached route (`relay.go:858`). Thus the route is not intact
after the stall; its removal is independently triggered by owner lookup,
rather than a counted control-plane terminal loss.

## SIGKILL while Redis remains stopped

The sequence is:

1. A checkpoint is present before the fault (`cmd/crash-run/store_stall.go`,
   `prepare`). SIGSTOP pauses the owned Redis child while audio/video continue.
2. The three-second lease deadline passes during the five-second pause.
   Lease and snapshot have the same expiry (`pkg/sessionstore/redis.go:218`).
3. The driver SIGKILLs and reaps the owner before SIGCONT
   (`cmd/crash-run/main.go:370`). After SIGCONT, Redis treats the old lease and
   snapshot as expired. An authoritative get also prunes the expired snapshot
   and persisted route hash (`pkg/sessionstore/redis.go:245`).
4. Control detects the dead worker, then enumerates `ListByWorker`
   (`pkg/controlplane/takeover.go:160`). The Redis index either expired or
   returns stale membership. Stale non-live leases are omitted
   (`pkg/sessionstore/redis.go:826`). No session job reaches checkpoint policy.
5. Concurrent `/status` reads drop the call on missing ownership
   (`pkg/controlplane/controlplane.go:827`), without counting a loss.
6. Relay owner refresh removes the cached route on authoritative absence
   (`pkg/relay/relay.go:858`). No media reaches a successor worker.
7. The caller receives no undecryptable media. It becomes silent, then ICE
   disconnected, then failed. There is no counted definitive loss or terminal
   takeover event, even though ADR 0002 calls for one.

The driver now marks the three-second takeover wait as timed out but proceeds
to observe and Hangup (`cmd/crash-run/main.go:403`, `main.go:483`). Missing
terminal takeover remains a failing result. It still writes and judges the
caller report, including decryption failures and reconnects. A pure regression
test checks that this missing-event report is retained and that a decryption
failure cannot be hidden by the timeout. The real-process rerun verifies the
production-called timeout path as well.

At five seconds, ADR 0002 requires RTP `ceil(5*5000*1.25)+65 = 31315`, beyond
the default cap `32768-10064-2 = 22702`. Snapshot retention would therefore
permit a counted definitive loss, not a safe resume. Current expiry prevents
that decision from being reached. Actual takeover age/margins are unavailable,
not zero-age checkpoint evidence.

## Product options: proposals, not changes

### Crash plus long store stall

| Option | Safety argument | Cost / tradeoff | ADR impact |
|---|---|---|---|
| Retain placed-call metadata until terminal reconciliation; count vanished ownership as definitive loss and explicitly clear route/cache | Recheck session/epoch before cleanup; never erase a successor's lease or route. With no valid owner/snapshot, send no resumed media. Cleanup and accounting must be idempotent | Smallest change; keeps current availability limit. A control restart needs durable terminal metadata if the guarantee must survive that restart | Clarify ADR 0002 terminal loss for expired/missing checkpoints; ADR 0003 if generation-conditional route cleanup is added |
| Decouple lease expiry from retained encrypted snapshot, checkpoint timestamps and recovery inventory | Retained bytes grant no ownership. Acquire a new fenced tenure atomically, then apply snapshot-age margins, caller reserve and caps. Five-second default snapshots still end in definitive loss | More store lifecycle/index work, bounded retention, and stronger race tests; enables recovery or precise accounting after ownership expires | ADR 0002 recovery/retention contract and ADR 0003 expiry-pruning lifecycle |
| Increase matching worker/control lease TTL beyond the tested stall | Atomic epoch transfer and relay fencing remain mandatory. ADR 0002 must still reject snapshots whose required margin exceeds the cap | Quick bounded mitigation, but longer stale leases and a larger failure window. A longer store outage repeats the silent-accounting bug | ADR 0002 should document the changed lease/outage contract; margin policy unchanged; ADR 0003 lifecycle must remain consistent |

Recommend retained placed-call metadata plus explicit counted terminal
reconciliation first. Control already has a call map; do not let status pruning
silently bypass the loss path. Reconcile against authoritative ownership and
epoch, and clean up only that tenure. Decoupled retention can follow if recovery
across lease expiry is a desired product guarantee. A TTL increase alone is
not a complete fix.

### Live worker plus long store stall

| Option | Safety argument | Cost / tradeoff | ADR impact |
|---|---|---|---|
| Keep worker fencing; make session loss visible to control and reconcile/count/clean it | Never resurrect or reuse stale encryption counters. Worker fencing and epoch-aware terminal cleanup preserve single ownership | Smallest safe completion of current behavior; a temporary store outage still ends the call. Caller notification/status semantics need a defined terminal reason | ADR 0002 should cover live lease-loss accounting as distinct from checkpoint-policy takeovers; ADR 0003 for conditional cleanup |
| Use a longer matching lease TTL for a documented maximum store stall | Worker keeps its current cryptographic state without loading stale counters. A valid lease remains exclusive; any transferred tenure must fence the old relay leg | Preserves calls for bounded stalls, at the cost of longer ownership retention. Still needs counted cleanup for stalls beyond the new TTL | ADR 0002 availability and live-outage contract; ADR 0003 expiry lifecycle stays aligned |
| Add bounded live-session suspension and atomic reauthorization after expiry | Fence output first. Resume only if an atomic new epoch belongs to that worker, no successor owns it, and its current transport/counter state plus source-advance bounds are safe. Otherwise definitive loss | Most complex; requires retaining local state instead of closing it, a new store operation, conditional route restoration, and explicit source-index budget tests | ADR 0002 confirmed-live/recovery distinction; ADR 0003 new-epoch routes. Preserve ADR 0001's original authenticated address/consent evidence |

Recommend explicit counted termination with existing worker fencing first.
It closes the observability/cleanup gap without weakening ownership or sequence
safety. If surviving these stalls is a product requirement, design bounded
reauthorization as a separate change; do not silently re-claim the same session
or reload an expired checkpoint. A longer TTL is an optional bounded
availability policy, not a substitute for terminal reconciliation.

## Measured results

Live silence includes trailing silence to the observation end; it is not a
finite recovery gap. Each live row had zero caller decryption failures,
reconnects and renegotiations, no counted terminal event, and caller
connection/ICE state `failed` at the end.

| Stall | Trial | Actual stall (s) | Observed media silence (s) | Outcome |
|---|---:|---:|---:|---|
| 5 s | 1 | 5.000361 | 35.024821 | Media stopped; caller failed |
| 5 s | 2 | 5.000576 | 35.011956 | Media stopped; caller failed |
| 5 s | 3 | 5.000093 | 35.011658 | Media stopped; caller failed |
| 3.5 s | 1 | 3.500262 | 35.026049 | Media stopped; caller failed |
| 3.5 s | 2 | 3.500412 | 35.032336 | Media stopped; caller failed |
| 3.5 s | 3 | 3.500161 | 35.012425 | Media stopped; caller failed |

Crash rerun: actual stall 5.005747 s; observed trailing
media silence 38.030349 s; decryption/reconnect/renegotiation counts all
zero. No terminal takeover, no loss or envelope count; one detection. The caller
was disconnected 5.642 s after SIGKILL and failed
31.649 s after SIGKILL. Hangup returned 404 after observation
and report collection. Snapshot age and takeover margins were unavailable.

| Moderate trial | Actual stall (s) | Snapshot age (s) | Checkpoint age (s) | RTP/SRTCP margins | Largest media gap (ms) | Result |
|---|---:|---:|---:|---:|---:|---|
| 1 | 1.502969 | 1.928310 | 1.928310 | 12117/259 | 399.795 | Scaled resume, PASS |
| 2 | 1.507417 | 1.938736 | 1.937736 | 12183/260 | 403.038 | Scaled resume, PASS |
| 3 | 1.508480 | 1.928238 | 1.928238 | 12117/259 | 393.351 | Scaled resume, PASS |
| 4 | 1.506789 | 1.989337 | 1.989337 | 12499/266 | 439.910 | Scaled resume, PASS |
| 5 | 1.506514 | 1.927220 | 1.927220 | 12111/258 | 380.008 | Scaled resume, PASS |

All five moderate trials had zero decryption failures, reconnects and
renegotiations, and exactly one scaled envelope event. Trial 1 is from the
initial investigation; trials 2–5 are the four follow-up runs.

Artifact roots (all include driver logs, child logs and metrics):

- `bin/crash-run-3379891562`: five-second no-kill trials.
- `bin/crash-run-2326292457`: 3.5-second no-kill trials, including first post-resume samples.
- `bin/crash-run-1739562693`: crash rerun, including preserved caller report and fault timestamps.
- `bin/crash-run-361189385`: original passing moderate trial.
- `bin/crash-run-1328466976`: four passing follow-up moderate trials.

VERIFIED — all requested investigation runs and five total moderate trials;
vet, crash-run/child-tracking race tests, actionlint and diff checks pass.
The crash long-stall acceptance is deliberately red; product fixes were not
implemented. Hosted CI and 60-second consent acceptance are NOT-RUN.
