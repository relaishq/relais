# Local load-generator evidence

Worker checkout: `feat/38-load-generator`, base `b2ce35e`, darwin/arm64, Go 1.26.9.

These measurements came from a shared developer Mac. They do not establish a stable host capacity or a hosted Linux result.

## 60-second load run

```sh
./bin/relais-load -callers=10 -processes=2 -duration=60s \
  -event=10s,move,2,1 -event=30s,kill,0,0 \
  -output=bin/load-mac-final.json
```

| Process | Callers | Peak CPU / one core | Peak heap MiB | Peak RSS MiB | Send p50 | Send p99 | Peak interval p99 | Send max |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 96763 | 5 | 8.08% | 11.0 | 34.9 | 1.7 ms | 141.2 ms | 148.3 ms | 168.357 ms |
| 96764 | 5 | 7.85% | 11.6 | 35.0 | 1.3 ms | 122.0 ms | 150.2 ms | 150.280 ms |

**Outcome: inconclusive.** Both generators exceeded the default 20 ms interval p99 send-lateness limit. Each had GOMAXPROCS 1. CPU remained below the 85% limit. The command exited 1 and preserved the cause.

All 10 calls returned complete reports. All stayed connected, with zero decryption failures and zero ICE restarts. The move at 10 seconds affected two calls; SIGKILL at 30 seconds affected three calls. All affected calls retained the same per-event yardstick as the ordinary harness. The two move rows passed each recorded metric. The three takeover rows retained trusted audio-loss and video-loss failures. These raw observations do not override the invalid generator result or prove service guarantees.

Full report and child logs remain under ignored `bin/`: `bin/load-mac-final.json` and `bin/load-run-1442116382/`.

## Preliminary calibration and boundary repeat

One generator process, GOMAXPROCS 1, audio plus video, 15 seconds per step. The preliminary ramp was valid at 1, 2, 4 and 8 callers and saturated at 16. A lint run overlapped its 16-caller step. The separate boundary repeat at 8 callers also saturated (peak interval p99 30.4 ms, peak CPU 12.77%). This variation prevents treating the preliminary count of 8 as a stable limit. The final ramp below runs after the worker quality checks finish.

Preliminary reports: `bin/calibration-mac.json` and `bin/calibration-mac-boundary.json`.

## Final Mac calibration

```sh
./bin/relais-load -calibrate -processes=1 -gomaxprocs=1 \
  -duration=15s -calibration-max=16 \
  -output=bin/calibration-mac-final.json
```

| Callers / process | Peak CPU / one core | Peak heap MiB | Peak RSS MiB | Cumulative send p99 | Peak interval p99 | Generator outcome |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| 1 | 1.83% | 3.0 | 23.5 | 2.2 ms | 3.1 ms | valid |
| 2 | 2.46% | 4.0 | 25.4 | 2.2 ms | 2.3 ms | valid |
| 4 | 4.19% | 7.0 | 29.8 | 2.5 ms | 3.5 ms | valid |
| 8 | 7.15% | 11.9 | 39.5 | 2.7 ms | 3.7 ms | valid |
| 16 | 21.66% | 23.0 | 54.3 | 440.1 ms | 440.4 ms | inconclusive |

The final ramp observed **8 valid callers and saturation at 16 callers**. Its peak interval p99 at 16 was 440.4 ms; CPU was only 21.66% of one core. This is a short-run observed bracket. The independent eight-caller saturation and the longer five-call-per-process load saturation show that it is not a stable sustained-capacity limit. Other workers share this Mac. Worker quality checks had finished before this final ramp began.

The final calibration report includes every step, per-call yardstick, thresholds and resource samples. It remains in `bin/calibration-mac-final.json`; process logs are in `bin/load-run-3258105647/`.

## Hosted Linux

**NOT-RUN.** The new nightly/manual `Load generator calibration` workflow uploads the hosted report, runner details and all process logs. The orchestrator must dispatch it after integration. The load binary cross-compiles for linux/amd64; that does not prove native Linux resource sampling.

## Verification

- Race unit tests: `go test -race -count=1 ./internal/loadgen ./cmd/relais-load`.
- Real external-caller socket/timing test: `go test -race -count=1 -run ^TestSendTiming ./pkg/callharness/`, with a worker-owned throwaway Redis endpoint and `RELAIS_TEST_REDIS_REQUIRE=1`.
- CPU-saturation mutation: removing CPU detection from the production report reader made `TestReadProcessUsesActualSaturationLimits/cpu` fail; restoring it returned the race suite to green. The red log is `bin/saturation-mutation-red.log`.
- `go vet ./...`, `make build`, Linux cross-build, repository formatting and golangci-lint v2.14.0 (0 issues).
- Workflow YAML parses. Hosted workflow execution and full namespace load placement are NOT-RUN. No full harness matrix was run.

Cancellation smoke: SIGTERM during an active two-process run produced an inconclusive report with both child records. All eight owned topology/caller/Redis PIDs were absent after cleanup (`bin/load-cancel-check.json`). A final check found all 103 recorded task-owned service/caller/Redis PIDs absent (`bin/cleanup-check.json`).

All started processes are task-owned and stopped between runs. Work is uncommitted; no GitHub writes were performed.
