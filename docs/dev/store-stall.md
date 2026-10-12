# Real-process store outage acceptance

Build with `make build`. This requires `redis-server` and `ffmpeg`. Run on the
loopback topology, with no external Redis configured:

```sh
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=moderate -runs=5 -after=10s
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=long -runs=5 -after=10s
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=live -runs=5 -after=10s
```

Each crash trial establishes a live audio/video call, pauses the driver's throwaway
Redis with SIGSTOP, and continues media past the 550 ms checkpoint envelope.
It SIGKILLs and reaps the owning worker while Redis remains paused, then sends
SIGCONT to that exact Redis child. The control plane detects the dead worker
and completes recovery or records definitive ownership loss. The driver never pauses an external store; `-redis`
and `RELAIS_REDIS_ADDR` are rejected for this scenario. Redis uses a free,
non-6379 loopback port. Signals use the child tracking/reaping lock, which
prevents a reused PID from being signalled.

Moderate defaults to 1.5 s; long defaults to 5 s. Override with
`-store-stall-duration=2s`. The duration must still exercise the selected
variant or the run fails. Runs stop on the first failure.

[ADR 0002](../adr/0002-checkpoint-age-envelope.md) requires RTP margin
`ceil(snapshot_age_seconds * max(5000, recorded_rate) * 1.25) + 65` and
SRTCP margin `ceil(snapshot_age_seconds * max(100, recorded_rate) * 1.25) + 17`.
At exactly 1.5 s these are 9,440 and 205; at 5 s they are 31,315 and 642.
The default RTP cap is `32768 - 10064 - 2 = 22702`. The moderate trial must
resume with scaled margins; the long trial must apply definitive loss without
spending resume margins. Detection and checkpoint cadence add to snapshot
age. Delayed successful writes can make checkpoint age smaller than snapshot
age. Safety uses snapshot age. The adoption deadline's enlarged caller reserve
and retained advances can reduce the effective cap further.

All variants require zero caller decryption failures, reconnects and renegotiation.
`moderate` requires one scaled envelope event and zero losses. `long` now keeps
ownership until checkpoint policy rejects its stale snapshot: one definitive
checkpoint loss, one envelope event, no spent margins, lease/routes cleaned.
`live` requires the same live call/owner, zero loss/takeover/envelope events,
continuing audio/video, and a largest gap under one second across pause and
recovery. It starts a same-host standby on the active relay's exact addresses;
that standby must remain waiting and the relay epoch must remain unchanged.

## Live outage budget acceptance (#100)

The default budget is 30 s. Matching `-store-outage-budget` flags on worker and
control configure it. Worker lease and snapshot retention is budget plus 3 s.
Relay TTL stays 600 ms: standby takeover needs a full TTL of successful unchanged
observations, and failed polls reset that evidence. Renewal cadence stays at one
second for workers and 200 ms for relays. Persisted route consent deadlines stay unchanged.
Retention headroom makes this a supported budget, not an exact termination timer:
some pauses over 30 s may survive. A 35 s default pause expires ownership.

```sh
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=live -store-stall-duration=5s -runs=2 -after=3s
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=live -store-stall-duration=15s -runs=2 -after=3s
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=live -store-stall-duration=29s -runs=2 -after=3s
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=expired -store-stall-duration=35s -runs=2 -after=3s
```

`expired` keeps workers alive, pauses 35 s by default, and requires exactly one
`ownership_expired` loss in status, metrics and caller report, zero envelope
events or invented checkpoint age, no resumed media, and conditional cleanup of
the lease, snapshot, persisted and cached relay routes within three seconds of
store recovery. The accounting event precedes asynchronous cleanup: existing
old-worker media can continue until fencing. No media may resume after the first
independently confirmed cleanup sample. Live trials check worker
session count, status, route count, ownership and snapshot presence before Hangup
can remove them. Redis is resumed before cleanup; every owned child is stopped
and reaped, including the waiting standby.

The driver prints `STORE_STALL` rows and writes `store-stall.json`,
`caller-report.json`, `metrics.json` and child logs under
`bin/crash-run-*/store-stall-*`. Fields include pause duration, policy/margins,
loss cause, media gaps, relay epochs and standby waiting state. Missing terminal
crash evidence still collects the full caller report and fails.
`CHECKPOINT_AT_TAKEOVER NOT-RUN` applies to expired ownership, where no checkpoint
age remains. Zero age fields on live trials do not claim checkpoint recovery.

CI runs five trials per case: live at 5 s, 15 s and 29 s, moderate crash, long
crash and 35 s expiry. Nightly/manual only, 25-minute limit, failure artifacts.
Local `-after=3s` observes at least five seconds after recovery; CI uses ten
seconds. These short runs do not claim the separate 60 s consent acceptance.
Default `-after=60s` retains that longer observation.

`TestPartitionedWorkerStoreTransferHasExclusiveCallerMedia` exercises real
workers, detector transfer, relay and Pion caller on memory and a dedicated
Redis. A's store calls fail while its session/media loop remains live. B takes
a higher epoch before A's expiry. Forty deliberately stale encrypted A packets
never reach the raw caller socket while B media continues. Restored A store
access fences its old session. [ADR 0008](../adr/0008-store-outage-budget.md)
records the safety argument and every retained artifact.

Historical pre-budget and #99 evidence remains in
[store-stall-investigation.md](store-stall-investigation.md).

## Normal endings and expiry

A successful matching store release retains a five-minute ended tombstone for
that owner and epoch. A worker-side DTLS close, consent timeout or other normal
session end therefore remains a terminal end, including a PeerConnection close
before DELETE. It does not increment `ownership_expired` or an envelope event.
The marker is separate from the pruned record and snapshot, and a new claim
supersedes it. Conditional cleanup cannot delete the new owner's state.

If a call ends while the store is unreachable, release may fail and the lease
may expire. With no committed ended marker, that ambiguous outcome counts as
`ownership_expired`. Sweeps run at the smaller of half the lease TTL and 1.5 s.
Cleanup retries use capped backoff outside `/status`. Checkpoint margins stay
unchanged; ADR 0008 changes retention for #100.

The control registry and loss history remain in memory and do not survive a
control restart. A relay restart during an outage retains ADR 0003's partial
restore limit; cross-host relay failover is outside this budget.

## Frozen worker and relay checks

`crash-run -worker-stop -compare-cache=false -runs=1 -after=3s` SIGSTOPs the owner
without ending its process or expiring its 33 s lease. HTTP heartbeat detection
still transfers ownership after 400 ms plus the 50 ms detector tick. The driver
records the pause as the failure start, measures decrypted media recovery, and
kills/reaps the frozen child during cleanup.

`crash-run -relay-standby -runs=2 -after=3s` exercises both SIGKILL and SIGSTOP
on the default 600 ms relay lease, with the under-one-second caller gap and no
reconnect required. No lease override is used.
