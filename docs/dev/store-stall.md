# Real-process checkpoint store stalls

Build with `make build`. This requires `redis-server` and `ffmpeg`. Run on the
loopback topology, with no external Redis configured:

```sh
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=moderate -runs=5 -after=10s
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=long -runs=5 -after=10s
```

Each trial establishes a live audio/video call, pauses the driver's throwaway
Redis with SIGSTOP, and continues media past the 550 ms checkpoint envelope.
It SIGKILLs and reaps the owning worker while Redis remains paused, then sends
SIGCONT to that exact Redis child. The control plane detects the dead worker
and completes a takeover. The driver never pauses an external store; `-redis`
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

Both variants require zero caller decryption failures, no reconnect or
renegotiation, and exactly one envelope event. Moderate requires continuing
media on both tracks. Long requires one counted loss, no resumed media, no
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

## Live-worker investigation and missing takeovers

The investigation variant leaves both workers alive:

```sh
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=live -store-stall-duration=5s -runs=3 -after=35s
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=live -store-stall-duration=3.5s -runs=3 -after=35s
```

Live defaults to five seconds. `OBSERVED` means the driver collected valid
observations and found no caller decryption failures or reconnects. It does
not assert that the call survived. The report records media silence (including
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

On the current product, a stall beyond the three-second session lease can
expire both the lease and snapshot before checkpoint policy runs. Live workers
then fence their sessions; crash recovery can miss the session in lease
enumeration. The requested long-stall acceptance remains failing. Product
behaviour and ADR policy have not been changed by this driver work.
