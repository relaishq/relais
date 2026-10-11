# Soak implementation and local evidence (#41)

The rework 3 contract is in [soak.md](soak.md): an uninterrupted memory
sentinel, whole-call gap inventory, token-based rejoin and Redis scan positive
controls. Earlier sections below record historical judgments. Old artifacts
without the new evidence cannot prove the new contract.

## Rework 3 verification

Verification used macOS/arm64, Go 1.26.9, and GOMAXPROCS=4 for Go tools.
Changes remain uncommitted. No GitHub writes or hosted/full-hour run were
performed in this stage. Redis 6379 was never used.

| Check | Status | Evidence / limit |
| --- | --- | --- |
| Formatting and static checks | VERIFIED | Changed tracked/untracked Go files are gofmt clean; diff whitespace, native/Linux vet, golangci-lint v2.14.0, make build and actionlint passed. |
| Memory race suite | VERIFIED | All six changed packages passed `-race -short -p=1`: relais-load, loadgen, capacity, processrss, controlplane and callharness. |
| Redis race assertions | VERIFIED | First five packages passed. All 160 compiled race-build harness tests completed or intentionally skipped across three bounded invocations. Required Redis servers on 53915, 58059 and 59564 were reaped and independently confirmed absent. |
| One uninterrupted Redis harness invocation | PARTIAL | Initial 10 m limit and follow-up 6 m limit expired in existing recovery trials. No earlier assertion failure or race report. Remaining top-level tests were rerun in full; the final invocation passed. Timeout logs are retained. |
| Sentinel and memory policy | VERIFIED | Growing one-hour sentinel plus short recycled workers fails memory/outcome and cannot establish acceptance. Missing sentinel proof reports `worker memory unproven`. Resolver protects worker 0 from drain/kill while allowing moves; N+1/floor tests pass. Matched-age recycled-worker trends remain informational. |
| Whole-call gap inventory | VERIFIED | Actual recorder packet seam retains old mid-run gaps after rolling history expires; cap/overflow tests pass. Unscheduled 500 ms freeze cannot hide behind a larger scheduled takeover gap. Only saturation in that caller's generator makes it inconclusive. Missing inventory is inconclusive; overflow fails. |
| Rejoin fence and real recycle | VERIFIED | Private reset preserves death and token state. A paused process with a bare heartbeat remains dead and gets a challenge. Owned real-process recycle passes in both race-suite invocations, with a fresh heartbeat and completed membership before return. |
| Darwin metrics and no-cgo behavior | VERIFIED | Native API reports resident size and physical footprint separately; judgment ignores resident growth when footprint is flat, and fails footprint growth. `CGO_ENABLED=0 go build ./...` and no-cgo tests pass with clear sampling skips. No-cgo soak exits before creating a topology directory. |
| Scan controls and frame retention | VERIFIED | Real Redis scan finds epoch counter/default relay hash; wrong prefix finds neither. Frame keys with 30 s TTL are accepted; missing/31 s TTL rejected. Missing controls make cleanup inconclusive. |
| Nightly and log format | VERIFIED | Six-call workflow default/fallback use 10m10s. Schedule test confirms 20 events, permitting one inconclusive under exact 95%. Playbook uses stable verdict prefixes and no branch-specific dispatch ref. |
| Production-source mutations | VERIFIED | Nine weakened guards fail their targeted tests: sentinel proof/protection, confirmed failure, gap retention/overflow, caller saturation ownership, rejoin fence, scan controls and Darwin metric choice. Source restored; regression suite green. |
| Linux runtime and hosted/full-hour memory | NOT-RUN | Linux source vetted; runtime, publication and larger-runner choice remain with the orchestrator/project lead. Local short runs cannot establish flat memory. |

Evidence is retained in ignored `bin/soak-verification/rework3-*` files.
`rework3-race-redis-combined-inventory.log` records compiled test coverage and
server absence. One source test, `TestAcceptanceRelayBufferTakeover`, has a
`!race` build constraint and is excluded from that inventory.

## Rework 3 local soak

One new macOS/arm64 loopback run used four calls, three minutes, seed 41,
three workers and two callers per generator. It ran after race tests stopped;
this shared Mac could still be contended. The command exited 1 with
`outcome=inconclusive`, `acceptance=false`. No policy was relaxed to pass it.

```sh
./bin/relais-load -soak -callers=4 -duration=3m -chaos-interval=30s \
  -seed=41 -workers=3 -worker-floor=8 -relay-floor=8 \
  -callers-per-process=2 -output=bin/soak-mac-rework3.json \
  > bin/soak-mac-rework3.log 2>&1
```

| Local check | Status | Evidence / limit |
| --- | --- | --- |
| Sentinel lifecycle/churn | VERIFIED | Worker 0 kept PID 97417, with 182 samples over 180.512 s, peak two sessions, a move away and a handover into it. Every drain/kill resolved to worker 1 or 2. The sentinel stays eligible for takeover; this short run's takeovers went to worker 2. |
| Recycles including the last | VERIFIED | Worker 2: 97430 → 388; worker 1: 97429 → 1512 → 4042. All three records have fresh heartbeats and completed rejoin after launch. Last recycle completed at 10:15:54.678702-07:00, before media ended. |
| Calls and failure counters | VERIFIED | All four calls cover intended lifetime. Six affected/recorded call events match. Zero caller/worker decryption failures and zero lost calls. No telemetry errors. Maximum worker sessions two, below floor eight. |
| Whole-call gap capture | VERIFIED | All eight tracks declare the inventory recorded; four retained gaps, all attributable to scheduled events. No overflow and no unscheduled-gap cause. Each generator retained 181 samples. |
| Event validity | PARTIAL | Four of five events pass; first drain overlaps saturation and is inconclusive. Coverage 80% is below exact 95%; all kinds have a conclusive pass. |
| Generator validity | PARTIAL | 8/180 wall-clock seconds saturated (4.444% >1%). Excluded process intervals are retained; run remains inconclusive. |
| Memory trend evidence | PARTIAL | All 21 metrics are insufficient-data. Physical footprint and resident samples are separate. Sentinel memory is explicitly unproven; short duration prevents acceptance. Both recycled-worker trends have matched-age observations for every incarnation and remain informational. |
| Cleanup and scan controls | VERIFIED | After 35.000088542 s relay wait, zero sessions/routes/pending routes/buffers/holds. Both Redis scan controls found. Four retained epoch keys have 266957–267915 ms TTLs within 308000 ms. Released relay hash and epoch counter are bounded retention. No remaining processes/listeners/errors. |
| Independent process/group proof | VERIFIED | All 22 owned PID/group probes absent after shutdown. |
| Greppable verdict | VERIFIED | Log includes `soak outcome=`, both `soak cause:` lines, `soak acceptance=false`, and `soak acceptance reason: worker memory unproven`. |
| Hosted/full-hour proof | NOT-RUN | Orchestrator owns publication/dispatch; larger hardware and capacity floors still required. |

| Offset | Kind | Worker | Affected / recorded | Outcome |
| --- | --- | ---: | ---: | --- |
| 30 s | Move | 1 | 1 / 1 | Pass |
| 60 s | Drain | 2 | 1 / 1 | Inconclusive: saturation |
| 90 s | SIGKILL | 1 | 2 / 2 | Pass |
| 120 s | Move | 1 | 1 / 1 | Pass |
| 150 s | Drain | 1 | 1 / 1 | Pass |

Ignored artifacts: `bin/soak-mac-rework3.json`,
`bin/soak-mac-rework3.log`, `bin/load-run-2181169564/`,
`bin/soak-verification/rework3-local-proof.log` and
`bin/soak-verification/rework3-process-probes.log`.
The initial post-run Python helper rejected Go's five-digit RFC3339 fraction;
timestamp normalization fixed the offline assertions. Raw report/log data
were unchanged and no second soak ran. The initial parser log is retained.

## Hosted report rejudged after rework 2

The orchestrator published commit `8ac0040` and supplied downloaded artifacts
for [run 38152777929](https://github.com/relaishq/relais/actions/runs/38152777929):
eight calls, ten minutes, seed 41, Linux namespace topology on ubuntu-latest.
No new hosted run was dispatched during rework 2.

The new policy is implemented with an **exact 95%** threshold. This artifact
has **18/19 conclusive events = 94.737%**, so it remains **inconclusive** with
`acceptance=false`. Its only run-level cause is now insufficient event
coverage. This conflicts with the rework brief's expected `pass`: rounding
coverage to a whole percent would give 95%, but that is a different policy.
The orchestrator has been asked to choose; no rounding was silently added.

Event 5 (zero-based, the kill at three minutes) remains individually
inconclusive because its window overlaps generator saturation. Every other
event passes. Every affected-call count matches the recorded count. All
three kinds have conclusive passes. Saturation is exactly 6/600 seconds (1%),
which is allowed. Bounded cleanup passes. Contrary to the brief's summary,
the raw artifact has conclusive relay/control/Redis memory metrics; workers
and callers have insufficient data. No short run establishes acceptance.

| Rework 2 check | Status | Evidence / limit |
| --- | --- | --- |
| Formatting, vet, lint, build, workflow | VERIFIED | gofmt, diff whitespace, native/Linux vet, golangci-lint v2.14.0, make build and actionlint passed. |
| Changed-package race tests | VERIFIED | relais-load (both changed Go files and tests) passed with memory storage and required throwaway Redis; Redis reaped. |
| Outcome policy tests | VERIFIED | 19/20 conclusive passes; 10% inconclusive does not; absence of a conclusive kill does not; any failed event fails. |
| Acceptance tests | VERIFIED | Short runs, failed outcomes and insufficient long-lived memory cannot establish acceptance. A full-scale fixture with conclusive memory can. Short runs can pass without memory evidence. |
| Production guard mutations | VERIFIED | Five mutations reject weakened event coverage, per-kind coverage, acceptance scale, acceptance memory and acceptance outcome. Restored source passes. Prior twelve mutation logs retained. |
| Hosted JSON rejudgment | PARTIAL | Raw events unchanged, 18/19 conclusive, outcome inconclusive under exact 95%, acceptance false. Expected pass needs a threshold/rounding decision. |
| Nightly scale | VERIFIED | Dispatch default and scheduled fallback use six calls; ten-minute duration and 30 s interval unchanged. |
| New six-call hosted nightly | NOT-RUN | Orchestrator owns publication and dispatch. |

Rejudged artifacts are ignored local files:
`bin/soak-linux-38152777929-rework2.json` and
`bin/soak-verification/rework2-hosted-rejudge.log`. Runner metadata is copied
from the measured Linux run, rather than the Mac doing the offline judgment.
The JSON lists every event's kind, verdict and causes, exact conclusive count
and percentage, per-metric memory requirements, and acceptance reasons.


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
