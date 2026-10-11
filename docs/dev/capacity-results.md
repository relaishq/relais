# Capacity evidence for #40

## Hosted Linux — setup failed, capacity inconclusive

Run **38135614200**, commit `3a6cad5`, dispatched `mode=capacity`,
`duration=30s`, `capacity_max=16`, `callers_per_process=2`. The runner was
Linux/amd64, Go 1.26.9, four logical CPUs on AMD EPYC 7763. The job selected
`-topology=netns`: an emulated, unshaped LAN. Redis bound private port 16379;
relay, control and two worker listeners started on their planned addresses.
Private-listener and isolation probes completed before the caller launch.

The first two-caller worker step failed before media. Its caller factory bound
`0.0.0.0:0`, which the harness correctly refused with
`callharness: caller socket must bind a specific IPv4 UDP address`.
There were no successful media sends or caller reports. Worker and relay lower
bounds were zero, and the planned load was **unverified**. This run provides
setup evidence, not hosted service-capacity evidence. Artifacts were uploaded
and the owned namespace cleanup completed.

Rework passes the specific caller public-side bind address to the generator
child and shares the namespace socket factory with `crash-run`. The real UDP
listener regression failed on the old wildcard bind and passed after the fix.
Plan/child-flag checks also cover the caller interface, private service binds,
allowed networks, non-6379 Redis and password-file cleanup. The relay buffer
switch remains unchanged pending the orchestrator's #35 merge.

**NOT-RUN — corrected hosted namespace sample.** After committing/pushing the
rework, the orchestrator must re-dispatch **Load generator calibration**
(`load-calibration.yml`, `mode=capacity`) with the same inputs. Supply
`prices_json` only with explicit machine and transfer inputs. Record the new
runner, lower bounds or failure bracket, generator validity, N+1, drain and
cost after CI. The orchestrator posts the evidence on #31.

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
complete counts. The initial handoff had no local ramp after the instrumentation
fix; the rework smoke below now covers the complete command hook.

The stated budget is **256 client commands and 1 MiB of request+reply bytes per
call-second**. All measured wire rates were below the byte budget, and the
measured command lower bounds are below the command budget. Final complete
operation-budget validation for this older sample is **PARTIAL**; the rework
smoke below has complete local counts. Hosted sampling remains required. Checkpoint
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
in `bin/w40-vet.log`, `bin/w40-lint.log`, `bin/w40-build.log`. The initial implementation was committed by the orchestrator as `3a6cad5`.
Rework below remains uncommitted. No worker GitHub writes or delegation occurred.

## Rework 1 — one bounded Mac loopback smoke

Commit `3a6cad5` plus uncommitted caller-bind rework, darwin/arm64, Go 1.26.9.
Exactly one local capacity command was run after the fix, with this worker's
checks complete before media began:

```sh
./bin/relais-load -capacity -topology=loopback -capacity-start=2 \
  -capacity-max=2 -duration=20s -callers-per-process=2 -workers=3 \
  -output=bin/capacity-mac-rework1.json
```

Report/log: `bin/capacity-mac-rework1.json`, `bin/capacity-mac-rework1.log`,
`bin/load-run-3566735516/`. Exit 0; all four generators valid, all **40**
caller yardstick rows pass. Worker and relay lower bounds are two calls;
relay throughput lower bound is **342.8 forwarded datagrams/s**. The derived
two-call N+1 kill/move and drain both pass. Takeover audio/video gaps are
447.5/457.2 ms; drain took **3.96 ms**. These are capped local observations.
They do not prove the repaired Linux namespace path or a saturation limit.

| Step | Complete application Redis commands/call-second | Redis wire bytes/call-second |
| --- | ---: | ---: |
| Worker | 67.88 | 86,428 |
| Relay | 67.85 | 86,496 |
| N+1 | 90.52 | 117,272 |
| Drain | 68.14 | 85,888 |

The final owners-client hook and heap byte-second fields were present at
runtime. All steps are below 256 commands and 1 MiB of Redis wire bytes per
call-second. Redis current RSS remains explicitly unavailable on this Mac;
the relay-step mean allocated Redis memory is 0.92 MiB/call. Monetary cost
remains NOT-RUN because no prices were supplied. No buffer or snapshot policy
changed during rework.

VERIFIED — UDP bind regression red (`0.0.0.0:<port>`) then green; parent/child
namespace-and-bind flags and service address/auth checks pass. Evidence:
`bin/w40-rework-bind-red.log`, `bin/w40-rework-bind-green.log`,
`bin/w40-rework-unit.log`. Format/diff, vet, lint v2.14.0 (0 issues), Linux vet,
Linux namespace-test cross-compilation and `make build` pass:
`bin/w40-rework-vet.log`, `bin/w40-rework-lint.log`,
`bin/w40-rework-linux-vet.log`, `bin/w40-rework-linux-test-build.log`,
`bin/w40-rework-build.log`. Changed-package race tests include `relais-load`,
`nettopology` and the affected `crash-run` consumer:
`bin/w40-rework-race.log`. Workflow structure check confirms explicit netns
invocation and always/bounded artifact ownership restoration:
`bin/w40-rework-workflow-check.log`. All 27 recorded owned process PIDs are
absent after the smoke: `bin/w40-rework-load-cleanup.json`.

NOT-RUN — corrected native Linux namespace runtime and hosted capacity;
re-dispatch required. The Linux cross-compilation is compile evidence only.
