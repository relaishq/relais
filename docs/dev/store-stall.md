# Real-process checkpoint store stalls

Build with `make build`. This requires `redis-server` and `ffmpeg`. Run on the
loopback topology, with no external Redis configured:

```sh
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=moderate -runs=5 -after=10s
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=long -runs=5 -after=10s
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

All variants require zero caller decryption failures and no reconnect or
renegotiation. Moderate requires exactly one scaled envelope event and zero
losses. Long and live require exactly one `ownership_expired` loss in status,
the cause-labelled loss metric, and the run report, with zero envelope events
when ownership and the checkpoint expired before recovery. Moderate requires continuing
media on both tracks. Long and live require one counted loss, no resumed media, no
live call, a released lease, and cleared persisted relay routes. Each track
must show media flowing beyond the envelope while Redis was stopped.

The driver prints `STORE_STALL` rows and writes `store-stall.json`,
`caller-report.json`, `metrics.json`, and child logs under the printed
`bin/crash-run-*/store-stall-*` directory. These include actual/requested stall,
checkpoint and snapshot age, required and used margins, cap, policy, outcome,
counters, and rejected evidence. Default `-after=60s` observes the existing
consent window; shorter runs prove this store-stall scenario but do not claim
the separate 60 s post-takeover consent acceptance. Loss calls intentionally
end, so their hangup DELETE can return 404.

CI runs five trials of each variant with ten seconds of observation on nightly
and manual dispatch only. The job has a 15-minute limit and uploads failure logs.

## Live-worker expiry acceptance

The investigation variant leaves both workers alive:

```sh
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=live -store-stall-duration=5s -runs=3 -after=35s
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=live -store-stall-duration=3.5s -runs=3 -after=35s
```

Live defaults to five seconds. `PASS` requires counted definitive loss and
conditional cleanup after the three-second lease expires; it does not require
the call to survive. Surviving a longer outage is #100. The report records media silence (including
trailing silence), whether media stopped, connection/ICE states, worker session
count, control status/counters, relay route count, and lease/snapshot presence.
These observations precede Hangup, which can otherwise remove a live call.
The relay route count is sampled before store reads that can prune expired
records. Counts apply to this trial's single-call topology.

A missing crash takeover now retains the full observation window, collects the
caller report, and exits unsuccessfully with `Outcome: "missing-takeover"`.
Caller integrity is still judged. No zero-valued takeover metadata proves a
fresh checkpoint: `CHECKPOINT_AT_TAKEOVER NOT-RUN` identifies the absent
observation. An already missing call can return 404 on Hangup.

A stall beyond the three-second session lease expires the lease and snapshot
before checkpoint policy runs. Live workers fence their sessions. The control
plane's retained placed-call registry now detects this independently of worker
death and lease enumeration, and records exactly one `ownership_expired` loss.
Its in-memory registry and history do not survive a control-plane restart.

Expired ownership has no measurable takeover checkpoint age. The report's
`loss_cause` identifies this path, and `CHECKPOINT_AT_TAKEOVER NOT-RUN` explains
the unavailable age. Zero-valued age fields are not fresh-checkpoint evidence.
The driver checks zero resume margins, one bounded terminal status event,
`relais_control_losses_total{cause="ownership_expired"}` increasing by one,
absence of the lease/snapshot and persisted routes, and zero cached relay routes.
The moderate variant still uses ADR 0002's age formula and scaled margins.

Ticket #99's required local verification uses three trials of each variant:

```sh
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=moderate -runs=3 -after=10s
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=long -runs=3 -after=10s
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=live -store-stall-duration=5s -runs=3 -after=10s
```
