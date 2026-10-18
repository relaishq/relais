# Worker-local drain quiescence measurements

This is **one machine's measurement**, not a capacity claim or a shutdown SLA.
Use the procedure on the deployment's worker binary, store, network and session
count before setting `-agent-drain-move-reserve`. The flag has no default.
Unset skips the handler wait; the placement mark always applies, followed by
urgent v2 export.

## Sample: 2026-10-17

A macOS arm64 machine with 12 logical CPUs ran Go 1.26.9 and Redis 7.0.11.
Control used its default drain concurrency: `min(GOMAXPROCS, 16)`, here **12**.
Each trial ran separate relay, control, and two `delayed-loopback` worker
processes, plus a dedicated disposable Redis on a non-6379 loopback port. The
call generator ran in the measurement process. There was no shaping, TLS,
provider traffic, video, browser or remote host. Other local race/build checks
were running during this sample; CPU isolation was not enforced.

The source had 1, 8 or 32 simultaneous v2 sessions. Callers sent 2.1 s of audio
concurrently. The driver waited until every source session was playing a reply
with at least two task chunks accepted, then called **control drain directly**.
That isolates the move reserve from the preceding local handler wait. It uses
the same urgent move path that follows SIGTERM quiescence. Each trial used a
fresh topology, because the drain mark is sticky.

Twenty trials per count (60 total), nearest-rank percentiles.
[Raw samples](drain-quiescence-samples.csv) contain all 60 observations:

| Sessions on source | Call to last acknowledged adoption p50 | p95 | max |
| --- | --- | --- | --- |
| 1 | 9.090 ms | 14.076 ms | 45.635 ms |
| 8 | 12.660 ms | 28.851 ms | 50.855 ms |
| 32 | 34.565 ms | 47.868 ms | 54.012 ms |

Adoption uses control's `LastMove` timestamp, written immediately after the
target's successful resume acknowledgement. The driver subtracts its control
request start. All processes use the same machine's wall clock. This is the
acknowledged adoption boundary, not the exact instruction inside the worker.
The full round also includes relay release and the HTTP reply:

| Sessions on source | Full HTTP drain round p50 | p95 | max |
| --- | --- | --- | --- |
| 1 | 9.517 ms | 16.594 ms | 46.532 ms |
| 8 | 12.993 ms | 30.902 ms | 87.158 ms |
| 32 | 35.216 ms | 48.599 ms | 54.910 ms |

For playout, each trial takes the largest caller-observed consecutive decrypted
audio-packet gap across all its sessions, from the packet before the request
through 250 ms after the drain response. These are distributions of trial
maxima, not percentiles over individual packets. The source's already accepted
reply prefix and playback cursor move; unfinished generation uses urgent retry.

| Sessions on source | Playout packet gap p50 | p95 | max |
| --- | --- | --- | --- |
| 1 | 33.568 ms | 40.600 ms | 54.795 ms |
| 8 | 40.831 ms | 59.285 ms | 64.121 ms |
| 32 | 67.020 ms | 80.734 ms | 93.532 ms |

All 60 drains adopted all sessions. All callers observed output after drain,
with no reported decryption failures, renegotiation or ICE restart. These checks
do not establish whole-reply payload equality, provider continuity, N+1 capacity,
network-profile behavior or any crash guarantee. The exact-mailbox/backlog and
ADR 0009 tests remain separate gates.

For **this sample only, at up to 32 sessions on the source**, use **500 ms** as
the documented starting reserve: the largest full round (87.158 ms), up to
100 ms for placement marking, and about 313 ms of headroom. This is deliberately
larger than the observed p95. It is not an automatic default or an extrapolation
to other loads. With `-drain-timeout=5s`, the default 2 s handler budget can then
fit before the reserved move phase. A smaller operator task budget only tightens
the wait. Remeasure after changing hardware, binary, store, routing, concurrency,
per-worker session count or background load. If that evidence is absent, leave
the reserve unset.

Control Drain can wait **up to 1 s for in-flight incoming reservations** before
listing and moving the source calls. These settled-topology measurements did
not include that wait. A **500 ms** reserve cannot cover a full 1 s reservation
wait, even before moves. If raced incoming work can remain reserved, budget its
settlement time plus measured move rounds and headroom, and enlarge the overall
shutdown deadline if needed. The reserve must stay below `-drain-timeout` and
reduces the available handler wait. With no such measurement, leave it unset.

## Reproduce

Prerequisites: Go and Redis on PATH. No provider credentials are needed. Run
from the checkout being measured:

```sh
RELAIS_DRAIN_MEASURE=1 go test -v -count=1 -timeout=24m \
  -run '^TestDrainMoveMeasurements$' ./cmd/relais-load/
```

Optionally use a ready wefty process node to retain the run and its logs:

```sh
wefty exec --detach --json --max-runtime 25m -- sh -c \
  'RELAIS_DRAIN_MEASURE=1 go test -v -count=1 -timeout=24m -run "^TestDrainMoveMeasurements$" ./cmd/relais-load/'
wefty logs RUN_ID --all
wefty --json inspect RUN_ID
```

`cmd/relais-load/drain_measure_test.go` builds the three binaries once, owns and
stops every child and Redis, records each sample as `DRAIN_SAMPLE`, and prints
`DRAIN_SUMMARY` for adoption, full round, and playout. The measurement skips in
ordinary tests and under `-short`. Its own trial contexts bound hangs, and the
outer run stays below the 30-minute measurement budget. The sample completed in
**262.926 s** including builds and teardown.

Measured run: `run_8767b5a14e533cee8da0b18adaab581c`, measured on **`9a489a3` plus
the then-uncommitted slice-9 changes**, before the
subsequent rebase and review fixes. It does not measure the later PR head.
Retain the raw wefty log alongside the exact build/configuration used for an
operator decision; do not treat a later tree as the measured binary.
