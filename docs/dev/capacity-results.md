# Capacity evidence for #40

## Hosted Linux — NOT-RUN

The orchestrator must push `feat/40-capacity` and dispatch **Load generator calibration**
(`load-calibration.yml`, `mode=capacity`), with `duration=30s`, `capacity_max=16`,
`callers_per_process=2`. The default topology is emulated, unshaped LAN with
three media workers and one relay/control/store. Supply `prices_json` only
with explicit machine and transfer inputs. Record the runner specification,
measured lower bounds or failure bracket, generator validity, N+1 results,
drain and cost here after CI. The orchestrator posts the evidence on #31.

The earlier hosted **generator calibration**, run 38133371237 on main, is an
input to sizing generators: four callers per process were valid, eight first
saturated. It is not hosted service-capacity evidence for this change. These
capacity runs use two callers per process with GOMAXPROCS=1, below that proven
four-caller calibration step.

## Shared Mac, 2026-10-11 — short loopback samples

Base `40baa89`, uncommitted worker changes, darwin/arm64, Go 1.26.9. Each step
used two callers in one generator process for 20 seconds, audio plus the
harness VP8 fixture. One worker ramp and one relay ramp were capped at two;
no saturation frontier or exact limit was sought. Three workers were used for
the relay, N+1 and drain trials. These observations are local and potentially
contended; they do not establish sustained load, hosted capacity, namespace
isolation, shaped-network thresholds, or real-camera capacity.

```sh
./bin/relais-load -capacity -capacity-max=2 -capacity-start=2 \
  -duration=20s -callers-per-process=2 -workers=3 \
  -output=bin/capacity-mac-buffered.json
```

Report/log: `bin/capacity-mac-buffered.json`,
`bin/capacity-mac-buffered.log`, `bin/load-run-2962887771/`.

| Status | Step | Calls | Generator peak interval p99 | Peak generator CPU/core | Peak skipped frames | Caller verdict | Relay forwarded packets/s |
| --- | --- | ---: | ---: | ---: | ---: | --- | ---: |
| VERIFIED | Concentrated worker, move all calls | 2 | 2.5 ms | 5.90% | 0% | All eight event rows pass, 2/2 records | 342.7 |
| VERIFIED | Relay with spread placement, one move | 2 | 2.6 ms | 4.51% | 0% | All eight rows pass, 1/1 record | 341.6 |
| VERIFIED | N+1: SIGKILL at 6 s, move at 12 s | 2 | 5.5 ms | 2.49% | 0% | All 16 rows pass, 1/1 kill and 1/1 move record | 343.5 |
| VERIFIED | Loaded worker drain | 2 total, 1 drained | 2.5 ms | 4.66% | 0% | All eight rows pass, 1/1 record | 341.4 |

All four generator outcomes were **valid**, command exit 0. There were no
caller execution errors, decryption failures, ICE restarts or renegotiations.
The buffered takeover returned every measured audio/video unit: **0 lost
audio, 0 lost video frames**. Caller audio/video gaps were 399.6/431.0 ms;
the existing content and freshness rows passed. Survivors never exceeded the
proven two-call worker lower bound. Draining the one-call worker took a
conservative **4.83 ms**, including status selection and result recording.

The capped sample proves lower bounds of **two calls per worker**, **two calls
per relay** and **341.6 forwarded datagrams/s** for this fixture and host.
The relay lower bound caps the local planned load at two total calls over
three workers. The general plan uses
`T = min((W-1)*C - 1, R)`, reserving one post-failure move slot. This small local
plan was tested; it is not a recommendation to operate production at this load.

## Resource and store observations

The following table is from the valid relay step. Every value is normalized
by **total topology calls**, including idle-worker overhead. CPU seconds per
call-second are CPU cores per concurrent call. RSS is a time average of sampled
**lifetime peaks**, not current RSS. Process setup is outside the CPU/counter
window, but included in RSS high-water marks.

| Status | Process | CPU seconds/call-second | Mean sampled RSS peak MiB/call | UDP payload bytes/call-second | Observed TCP bytes/call-second |
| --- | --- | ---: | ---: | ---: | ---: |
| VERIFIED | Worker 0 | 0.005155 | 12.40 | 8,408 (in+out) | 15,043 |
| VERIFIED | Worker 1 | 0.013124 | 12.26 | 47,607 (in+out) | 71,391 |
| VERIFIED | Worker 2, idle | 0.001928 | 10.37 | 0 | 2,373 |
| VERIFIED | Relay | 0.009723 | 10.86 | 56,748 (forwarded out) | 12,170 |
| VERIFIED | Control | 0.005568 | 11.39 | 0 | 13,001 |
| PARTIAL | Redis | 0.006446 | Unavailable: INFO returned RSS 0 | — | Application traffic counted at clients |

Redis allocated memory is retained as `relais_process_heap_bytes` in the raw
observations. The final report schema explicitly marks unavailable RSS and
also computes current heap byte-seconds/call-second; those schema additions
were made after this runtime sample and are unit-tested, not remeasured here.
Redis/HTTP counters measure successful TCP read/write bytes, with protocol
and monitoring overhead. Worker UDP counters exclude relay headers; relay
private UDP output includes them. These are application transport observations,
not a complete NIC-byte trace, and rejected packets are not billed as egress.

| Status | Step | Application Redis client commands/call-second | Redis wire bytes/call-second |
| --- | --- | ---: | ---: |
| PARTIAL | Worker ramp | 63.96 | 86,854 |
| PARTIAL | Relay ramp | 63.70 | 86,144 |
| PARTIAL | N+1 | 86.29 | 115,505 |
| PARTIAL | Drain | 64.13 | 85,973 |

The sample already included all Redis wire bytes, encrypted checkpoints and
frame-cache traffic. Its command counter omitted the relay's separate owners
client. The final hook is on the shared Redis connection factory, and a real
Redis regression proves that both encrypted PutState and NewRedisOwners/Owner
produce command and byte counters. These **pre-fix command observations are
lower bounds**, not final complete operation rates. Hosted CI must supply the
complete counts. No further local ramp was run after the instrumentation fix.

The stated budget is **256 client commands and 1 MiB of request+reply bytes per
call-second**. All measured wire rates were below the byte budget, and the
measured command lower bounds are below the command budget. Final complete
operation-budget validation is **PARTIAL** pending hosted sampling. Checkpoint
cadence, snapshot-age safety and takeover margins were preserved. No cadence
optimization was needed from the measured traffic; a hosted budget exceedance
blocks planned-load approval and requires event-driven checkpoint work.

**NOT-RUN — monetary cost:** no machine or transfer prices were supplied.
Arithmetic and strict price-file validation are tested with explicit synthetic
inputs. At the measured occupancy, machine cost is
`(3 * worker/hour + relay/hour + control/hour + store/hour) / 2`.
Transfer uses measured public IPv4 UDP egress and the supplied price/GiB.

## Withdrawn preliminary configuration

The first run (`bin/capacity-mac.json`, `bin/load-run-3494205243/`) used the
relay command's existing phase-1 default, with its caller buffer disabled.
Generators were valid, but the kill lost 340 ms of audio and ten video frames.
It also exposed a scrape racing the killed process's exit. Those data are
**PARTIAL**, not phase-2 capacity acceptance. Capacity now enables the existing
buffer through an opt-in relay flag and retires its sampler before the real
SIGKILL. The corrected run above passed. No saturation thresholds were changed.
A brief lint run overlapped the preliminary drain; the corrected four-step
sample ran without this worker's checks overlapping it.

## Verification

| Status | Check | Evidence |
| --- | --- | --- |
| VERIFIED | Format, vet, lint v2.14.0, build and Linux cross-build | `gofmt -l` empty; diff check clean; `bin/w40-vet.log`, `bin/w40-lint.log` (0 issues), `bin/w40-build.log`, `bin/w40-linux-build.log` |
| VERIFIED | Workflow YAML and branch-dispatch configuration | Existing registered workflow confirmed read-only with gh; parsed inputs, nightly job and no PR trigger; `bin/w40-workflow-check.log` |
| VERIFIED | Final changed measurement/price packages under race | `bin/w40-final-targeted-race.log` |
| VERIFIED | Saturation guard mutation red → restored green | Removing the production generator-validity guard makes a saturated fixture incorrectly pass; `bin/w40-saturation-red.log`, `bin/w40-saturation-green.log` |
| VERIFIED | Race tests on memory and disposable Redis | Changed non-harness packages; `bin/w40-race-memory.log`, `bin/w40-race-redis.log` |
| VERIFIED | Targeted caller harness race tests | External move/drain observer, send timing and real buffered takeover on memory/Redis; `bin/w40-harness-race.log` |
| VERIFIED | Load cleanup | All 54 recorded owned service/caller/Redis PIDs absent; `bin/w40-load-cleanup.json` |
| VERIFIED | Test Redis cleanup | Owned Redis was reaped, non-6379 port; `bin/w40-redis-cleanup.json` |
| NOT-RUN | Hosted namespace sample, full harness matrix, region/far-caller, 60 s consent, sustained capacity | Orchestrator dispatch required; no Docker/VM was started |

Final formatting, vet, pinned lint and build results are recorded separately
in `bin/w40-vet.log`, `bin/w40-lint.log`, `bin/w40-build.log`. Changes remain
uncommitted. No GitHub writes or delegation occurred.
