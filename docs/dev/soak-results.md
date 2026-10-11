# Soak implementation and local evidence (#41)

Rework 1 used one new macOS/arm64 loopback run on 2026-10-11, Go 1.26.9.
The Mac was shared and possibly contended. No hosted or full-hour soak ran.

```sh
./bin/relais-load -soak -callers=4 -duration=3m -chaos-interval=30s \
  -seed=41 -workers=3 -worker-floor=8 -relay-floor=8 \
  -callers-per-process=2 -output=bin/soak-mac-rework1.json \
  > bin/soak-mac-rework1.log 2>&1
```

The command exited **1**, with overall outcome **inconclusive**. Bounded
cleanup passed. Generator saturation exceeded the 1% budget, one drain's
measurement window overlapped saturation, and every memory metric was
`insufficient-data`. This smoke proves execution and cleanup behavior; it
cannot establish sustained capacity or flat memory.

| Local check | Status | Evidence / limit |
| --- | --- | --- |
| gofmt and diff whitespace | VERIFIED | Changed Go files formatted; `git diff --check` passed. |
| Native and Linux vet | VERIFIED | `go vet ./...` and `GOOS=linux go vet ./...` passed. |
| Lint | VERIFIED | golangci-lint v2.14.0, `run ./...`, zero issues. |
| Build | VERIFIED | `make build` passed with final driver. |
| Changed-package memory race suites | VERIFIED | relais-load, loadgen, capacity, processrss, metrics and controlplane passed. Final driver/report changes also passed again. |
| Changed-package Redis race suites | VERIFIED | Same six packages passed with required owned Redis on 55818. Final driver/report packages passed again on 56386. Both servers reaped. |
| Workflow | VERIFIED | `actionlint .github/workflows/load-calibration.yml` passed. Hosted execution remains unrun. |
| Native current RSS | VERIFIED | macOS native API samples self and an owned child; cgo required. Linux source vetted, Linux runtime unrun. |
| Verdict red paths | VERIFIED | Twelve production-source mutations failed their tests: memory growth, saturation budget, event overlap, seed variation, key/session/process leaks, full interval retention, minimum memory window, missing TTL, overlong TTL and routes after the wait. Source restored and tests green. |
| Real Redis retention tests | VERIFIED | Positive bounded TTL accepted; missing and overlong TTL rejected. Persistent released relay hash accepted; overlong logical lease rejected. |
| Local soak execution | VERIFIED | Four calls covered their intended 180 s media lifetime, with five events: two moves, two drains, one SIGKILL. Seven affected-call records matched. Each generator retained 181 raw samples. |
| Decryption and lost calls | VERIFIED | Zero caller and worker decryption failures across all six worker incarnations. Zero control-plane lost calls, caller ICE restarts or renegotiations. No telemetry errors. |
| Worker floor | VERIFIED | Maximum observed worker sessions two, below supplied floor eight. |
| Event recovery | PARTIAL | Four events passed all ten rows each. The first drain's ten rows are individually inconclusive because its saved observation window overlapped saturation. No raw failed rows. |
| Generator validity | PARTIAL | 4/180 wall-clock seconds saturated (2.222% >1%). Three excluded process intervals reported; whole-call gap judgments also inconclusive due to generator send lateness. |
| Memory gate | PARTIAL | Every metric `insufficient-data`. Services have fewer than five minutes of post-warm-up observations; workers/callers have fewer than ten minutes per incarnation. Samples retained, no short-window growth failure or flat-memory claim. |
| Session cleanup | VERIFIED | After 35.000325458 s wait, zero control/worker sessions, relay routes, pending routes, buffer sessions and holds. |
| Redis retention | VERIFIED | Four epoch keys had TTLs 266995–267829 ms, within 308000 ms bound. `session_epoch` persisted without TTL as allowed. Relay hash persisted without key TTL but was released, with logical expiry zero. Each key/class/TTL is reported. |
| Process and listener shutdown | VERIFIED | No remaining owned process/group or listener. Every loopback TCP/UDP endpoint rebound. Independent follow-up verified 22 PID/group probes absent. |
| Original Mac artifact rejudged | PARTIAL | Overall changed from fail to inconclusive; every memory metric insufficient, all five events still pass. Original saturation remains 5/180 (2.778%). Old cleanup lacks post-wait status and key TTLs, so bounded retention is unverified. |
| Nightly Linux namespace soak | NOT-RUN | Workflow created and checked; orchestrator owns publication and dispatch. |
| Full 50-call, one-hour soak | NOT-RUN | Larger hardware and matching measured floors remain a project-lead decision. No GitHub result posted. |

Ignored local artifacts:

- `bin/soak-mac-rework1.json`, `bin/soak-mac-rework1.log`,
  `bin/load-run-1655639447/`: new run and complete child logs.
- `bin/soak-mac.json`, `bin/soak-mac-current-judgment.json`,
  `bin/soak-mac.log`, `bin/load-run-2851901302/`: original raw run and new judgment.
- `bin/soak-verification/`: validation, race, mutation-red and process-probe logs.

## New run event outcomes

| Media offset | Kind | Resolved worker | Affected / recorded | Outcome |
| --- | --- | ---: | ---: | --- |
| 30 s | Move | 1 | 1 / 1 | Pass |
| 60 s | Drain | 1 | 2 / 2 | Inconclusive: saturation overlaps saved recovery window |
| 90 s | SIGKILL | 0 | 2 / 2 | Pass |
| 120 s | Move | 0 | 1 / 1 | Pass |
| 150 s | Drain | 0 | 1 / 1 | Pass |

## Contract and follow-up boundary

Bounded retention is the cleanup contract, described in [soak.md](soak.md).
The relay fencing hash stores a logical expiry field and keeps its monotone
epoch without Redis PTTL. Release clears identities and sets expiry to zero;
the driver checks expiry against Redis server time. Worker index metadata,
if present, has its own documented default retention bound. No product
fencing or retention policy changed.

`Plane.End` could call `ForgetSession` to free relay state immediately; that
possible product improvement is outside #41. Longer, generator-valid memory
evidence remains needed. A seed replays planned kinds/times/selection offsets;
resolved workers also depend on observed ownership. Short/full CI dispatch
commands and required measured floors are in [soak.md](soak.md).
