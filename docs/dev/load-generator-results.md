# Local load-generator evidence

Rework checkout: `feat/38-load-generator`, base `e5510a4`, with uncommitted
PR #71 review fixes. Measured on 2026-10-11 UTC, darwin/arm64, Go 1.26.9.
This is a shared developer Mac. These short runs do not establish sustained
host capacity or a hosted Linux result. Worker build, lint and test checks
finished before these runtime measurements; no worker checks overlapped them.

## Correction to previous evidence

The earlier base `b2ce35e` advanced a deadline once per sent frame, while the
Go ticker dropped deadlines during stalls. One short stall therefore left
later sends permanently late. The regression reproduced this: one 60 ms stall
made **80/80 later sends exceed 20 ms**. The corrected pacer follows delivered
tick timestamps and counts dropped frame opportunities separately. The
regression now passes, and a subsequent one-second interval passes the
production lateness and skipped-frame limits.

The earlier five-call-per-process load results (148.3/150.2 ms peak interval
p99), preliminary ramps, eight-caller repeat, and 8/16 calibration bracket are
**withdrawn as generator-capacity evidence**. Their reported lateness was
inflated by persistent schedule debt, so those observations cannot distinguish
a short stall from sustained under-sending. Their raw caller reports remain
historical observations; they do not establish service guarantees. Historical
artifacts include `bin/load-mac-final.json`, `bin/calibration-mac.json`,
`bin/calibration-mac-boundary.json`, and `bin/calibration-mac-final.json`.

The measurements below use delivered-tick lateness and the separate 1%
skipped-frame limit. The 8/16 bracket below is a new observation from corrected
instrumentation, even though its caller counts match the withdrawn bracket.

## Corrected 20-second load run

```sh
./bin/relais-load -callers=8 -processes=2 -gomaxprocs=1 -duration=20s \
  -event=6s,move,2,1 -output=bin/load-mac-rework.json
```

| Process | Callers | Peak CPU / one core | Peak heap MiB | Peak RSS MiB | Send p50 | Send p99 | Peak interval p99 | Send max | Peak skipped frames | Status requests/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 32694 | 4 | 5.81% | 8.6 | 33.2 | 0.600 ms | 2.300 ms | 5.000 ms | 5.575 ms | 0.00% | 0.73 |
| 32695 | 4 | 6.26% | 9.0 | 32.5 | 0.600 ms | 2.700 ms | 5.000 ms | 5.620 ms | 0.00% | 0.73 |

**Outcome: valid; command exit 0.** All eight calls returned complete reports,
with no caller errors, decryption failures or ICE restarts. Each process sent
6,400 frames with zero skips. The move at six seconds selected two calls;
`affected=2` and `recorded=2`. All eight yardstick metrics for this event passed
with 2/2 trusted call coverage. Generator validity remains separate from
service verdicts.

Each process made 16 status requests including setup, and 15 in the measured
media/hangup intervals (0.73 requests/s). The driver made one event status read,
reported separately as 0.05 requests/s over the configured 20-second duration.
The real run confirms that status traffic is shared within each process.
Reports and logs: `bin/load-mac-rework.json`, `bin/load-mac-rework.log`,
`bin/load-run-1041248452/`.

## Corrected Mac calibration — one ramp

```sh
./bin/relais-load -calibrate -processes=1 -gomaxprocs=1 \
  -duration=15s -calibration-max=16 -output=bin/calibration-mac-rework.json
```

Audio plus video, one generator process, GOMAXPROCS 1. Default limits: interval
p99 lateness >20 ms, skipped frame opportunities >1%, CPU/GOMAXPROCS >85%.

| Callers / process | Peak CPU / one core | Peak heap MiB | Peak RSS MiB | Cumulative send p99 | Peak interval p99 | Peak skipped frames | Status requests/s | Generator outcome |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 1 | 1.59% | 3.2 | 24.5 | 2.2 ms | 6.105 ms | 0.00% | 0.64 | valid |
| 2 | 3.74% | 5.0 | 27.2 | 2.3 ms | 2.500 ms | 0.00% | 0.77 | valid |
| 4 | 8.53% | 6.3 | 31.3 | 2.5 ms | 3.800 ms | 0.00% | 0.71 | valid |
| 8 | 9.59% | 14.9 | 41.5 | 2.8 ms | 7.400 ms | 0.00% | 0.90 | valid |
| 16 | 17.94% | 26.7 | 61.8 | 60.8 ms | 80.800 ms | 6.09% | 0.97 | inconclusive |

The ramp observed **8 valid callers and saturation at 16**; calibration exit
0, with saturation as its expected endpoint. At 16 callers, interval p99
reached 80.8 ms and skipped frames peaked at 6.09%. The process sent 18,818
frames and skipped 382 of 19,200 opportunities. CPU stayed below its limit.
All steps had complete caller reports. The corrected evidence catches actual
missing workload; it does not carry one stall forward as permanent lateness.
This remains a short-run observed bracket on a shared Mac, not a stable
sustained-capacity threshold.

Report and logs: `bin/calibration-mac-rework.json`,
`bin/calibration-mac-rework.log`, `bin/load-run-3646616182/`.

## Verification

- `gofmt -l .`: no files; `git diff --check`: clean.
- `go vet ./...`: passed; golangci-lint v2.14.0: **0 issues**.
- `make build`: passed (`bin/rework-build.log`).
- `go test -race -count=1 ./cmd/relais-load/ ./internal/loadgen/`: passed
  (`bin/rework-units-race.log`).
- `go test -race -count=1 -run '^TestSendTiming|^TestExternalMove'
  ./pkg/callharness/`: passed (`bin/rework-harness-race.log`). Package tests
  used worker-owned Redis on a free loopback port, persistence disabled, with
  `RELAIS_TEST_REDIS_ADDR` and `RELAIS_TEST_REDIS_REQUIRE=1`; Redis was reaped.
- Stall red/green: `bin/rework-stall-red.log` records the 80/80 failure;
  `bin/rework-stall-green.log` records the passing corrected harness tests.
  `TestStallIntervalsRecover` exercises real media and the production meter:
  dropped frames appear in the stalled interval, and a later interval is valid.
- Ring-loss regression feeds 300 completed caller reports through the production
  reader and judge, with only 256 matching takeover records. The run is
  inconclusive despite 256/300 trusted coverage exceeding 80%. Repeated moves
  remain separate events, and unrelated action times cannot fill missing records.
- Shared poller regression uses the external harness's `Status` entry point.
  100 concurrent readers cause one HTTP request per one-second refresh, both
  for successful responses and failures. Request telemetry is checked.
- Tests cover unknown owner on a failed external move, exclusion of calls still
  owned by a killed worker, start deadline (including a future `/start` time),
  and parent-change cancellation through the production lifetime helpers.
- Workflow YAML and required settings verified: no concurrency group,
  `persist-credentials: false`, and an `always()` step prints command report
  summaries even if calibration fails. Artifact upload remains unconditional.
- Cleanup: all **46** rework-owned service, caller and Redis PIDs were absent
  (`bin/rework-cleanup-check.json`). Redis never used port 6379.

**NOT-RUN:** hosted Linux workflow, full harness matrix, namespace load
placement, and a deliberate parent-kill runtime test. Parent cancellation and
start deadlines have unit coverage; normal child lifetime and cleanup were
exercised by the load run and calibration. The orchestrator must dispatch CI
for hosted Linux evidence.

Changes remain uncommitted. No push, GitHub writes, or agent delegation occurred.
