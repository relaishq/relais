# Capacity evidence for #40

## Latest hosted Linux run 38142256615

[Run 38142256615](https://github.com/relaishq/relais/actions/runs/38142256615),
commit `be8901a`, passed the recorded acceptance policy with the **product-default
relay buffer on** (`relay_buffer_enabled=true`). Linux/amd64, Go 1.26.9,
a shared four-vCPU runner, namespace LAN, audio plus the harness VP8 fixture,
30 seconds per step and two callers per generator process with GOMAXPROCS=1.
Worker ramps used two workers; relay and fault samples used three. Evidence is
in the run's `capacity-linux-38142256615` artifact. Service-exit, takeover-count,
whole-call gap, shared-window weighting, store budget, drain-empty and
survivor-peak checks were enabled. The three subsequent verdict refinements
for caller stalls, later-track takeover recovery and crashes with invalid
generators have not yet been exercised in a hosted run.

| Trial | Call counts | Usable result |
| --- | --- | --- |
| Concentrated worker, move all | 2, 4, 8 | Pass; worker floor 8 calls |
| Concentrated worker at 16 | 16 | Inconclusive: generator interval p99/skipped-frame limits |
| Relay, spread placement and one move | 2, 4 | Pass; relay floor 4 calls and 684 forwarded datagrams/s |
| Relay at 8 | 8 | Inconclusive: peak one-second-interval send-lateness p99 20.68 ms exceeds 20 ms |
| N+1 kill, then planned move | 4 | Pass; two killed calls recovered, one subsequent move |
| Loaded drain | 4 total, 2 drained | Pass; 27.25 ms API interval, worker confirmed empty |

The generator process that limited the relay's eight-call step had a **whole-step
send-lateness p99 of 5.3 ms**. The acceptance cause uses the **peak one-second
interval p99**, not that whole-step quantile. No service capacity or failure
claim follows from the inconclusive step.

For C=8, R=4 and W=3, `T = min((W-1)*C - 1, R) = 4` was validated.
N+1 survivors peaked at **3 of 8 calls (37.5%)** and two calls; drain survivors
peaked at two each. This validates recovery at a demonstrated floor, not
survivors near capacity. The relay packets-per-second limit remains **NOT-RUN**;
684 datagrams/s is a demonstrated lower bound. Cost is
`NOT-RUN(no input prices)`.

| Trial | Calls | Client commands/call-second | Redis request + reply B/call-second | Total CPU seconds/call-second |
| --- | ---: | ---: | ---: | ---: |
| Worker | 8 | 67.03 | 85,282 | 0.033171 |
| Relay | 4 | 66.86 | 84,904 | 0.044120 |
| N+1 | 4 | 65.69 | 84,592 | 0.041693 |
| Drain | 4 | 66.78 | 85,148 | 0.041180 |

The passing relay step's Redis request + reply split is **50,277 B session
store**, **31,371 B frame cache**, and **3,256 B owners/routes per call-second**.
These sum to 84,904 B/call-second and fit the 100 commands/128 KiB budgets.
Buffer state comes from the running relay gauge, not from an inferred replay.

### Run-to-run variance

At identical workload settings, the earlier buffered run demonstrated a relay
floor of **8**, while this run demonstrated **4**. Both demonstrated worker
floor **8**. The product and instrumentation changed between commits, and the
shared runner's generator interval peaks constrain these floors. This is
variance in usable evidence, not proof that relay capacity halved. Track trends
with the **worker floor and per-call store traffic and CPU**, comparing rates
at matched call counts, workload and layout. Do not use T as the trend measure:
it also changes with
the relay's generator-limited floor and spare-worker formula. Neither run
supports independent-machine or sustained-load sizing.

## Earlier hosted Linux run 38136419832

[Run 38136419832](https://github.com/relaishq/relais/actions/runs/38136419832),
commit `f132ffb`, passed its recorded acceptance policy. Linux/amd64,
Go 1.26.9, shared four-vCPU AMD EPYC 9V74 runner. The workload was audio plus
the harness VP8 fixture, 30 seconds per step, two callers per generator process,
GOMAXPROCS=1. Topology: Linux namespaces with an unshaped LAN, one relay,
control and Redis. Worker ramps used two workers; relay and fault samples used
three. Reports and process logs are in the run's `capacity-linux-38136419832`
artifact.

The relay buffer was **on**, through the former capacity-only override
`-caller-buffer=true`; positive buffer packet/byte gauges confirm it. The load driver now passes no
buffer override and records the running buffer configuration explicitly.
This historical run therefore does not measure the pre-#35 buffer-off product
default. The newer guards and product default were exercised in run
38142256615 above; this earlier artifact remains evidence under its older
acceptance policy.

| Trial | Call counts | Usable result |
| --- | --- | --- |
| Concentrated worker, move all | 2, 4, 8 | Pass; worker lower bound 8 calls |
| Relay, spread placement and one move | 2, 4, 8 | Pass; relay lower bound 8 calls and 1371 packets/s |
| Both ramps at 16 | 16 | Inconclusive: generator saturation |
| N+1 kill, then planned move | 8 | Recorded pass; 3 killed calls recovered, one subsequent move |
| Loaded drain | 8 total, 3 drained | Recorded pass; 43 ms including the old status/result-recording overhead |

**Eight was the highest usable step in this earlier sample**. It was a
generator-constrained lower bound on this shared runner. Sixteen exceeded
generator interval p99/skipped-frame limits and supplies no service-capacity claim. These are
short samples, not sustained-load or real-camera limits.

For C=8, R=8 and W=3, the sample load was
`T = min((W-1)*C - 1, R) = 8`. Eight calls over three workers is a **floor**,
not an operating recommendation. Survivors peaked at **5 of 8 calls (62.5%)**
in the N+1 sample; the other survivor peaked at four. This proves failover
mechanics at that load, not survivors near capacity. Drain survivors peaked
at four each. The **relay packets-per-second limit is NOT-RUN**; only a lower
bound of **1371 forwarded datagrams/s** was measured.

## Earlier run: store and CPU observations

The kill-step report originally overstated counter and CPU rates by dividing
the retired worker's five-second contribution by its own window. The table
below recomputes raw first/last deltas using the common **30-second × call
count** denominator for every process. It does not rerun the trial. The other
steps change only slightly because their observed windows were almost 30 s.

| Trial | Calls | Client commands/call-second | Redis request + reply B/call-second | Total CPU seconds/call-second |
| --- | ---: | ---: | ---: | ---: |
| Worker | 8 | 66.84 | 84,759 | 0.022055 |
| Relay | 8 | 66.99 | 84,929 | 0.024675 |
| N+1 | 8 | **65.94** | **84,524** | **0.023588** |
| Drain | 8 | 67.01 | 84,886 | 0.024237 |

The obsolete N+1 values were 85.69 commands and 110,902 B/call-second.
Complete client command counts include encrypted session storage, owners and
routes, and frame cache. Lua internal commands and the driver's INFO connection
are excluded. The historical byte counters have no per-client split; that
split is **NOT-RUN for this artifact**. New reports label each client.

These observations fit the objective-derived **100 commands and 128 KiB**
window-average budget, with about 33% command margin and 35% byte margin.
The throughput assumptions and binary-unit rounding are stated in
[capacity.md](capacity.md). Actual Redis sizing for 500 calls is **NOT-RUN**.

## Earlier run: memory and marginal slope

Memory/call is **total sampled RSS divided by concurrent calls**. It includes
fixed service and idle-worker overhead. Go services report time-averaged
lifetime RSS peaks, including setup; Redis reports time-averaged current RSS.
The mixture is labelled in raw process observations. These figures are not
live-heap size or a marginal-memory guarantee.

| Layout | Calls | Total sampled RSS MiB | RSS MiB/call |
| --- | ---: | ---: | ---: |
| Worker ramp, two workers | 2 | 119.96 | 59.98 |
| Worker ramp, two workers | 4 | 125.19 | 31.30 |
| Worker ramp, two workers | 8 | 134.14 | 16.77 |
| Relay ramp, three workers | 2 | 213.75 | 106.87 |
| Relay ramp, three workers | 4 | 234.02 | 58.50 |
| Relay ramp, three workers | 8 | 234.49 | 29.31 |

Empirical total-RSS slopes for the worker layout are **2.62 MiB/additional
call** from 2→4 and **2.24 MiB/call** from 4→8 (2.36 MiB/call over 2→8).
Relay-layout slopes are **10.14** and **0.12 MiB/call**, respectively. Their
instability prevents a reliable relay marginal-memory estimate. Short runs,
setup peaks and allocator history limit both slopes; they are descriptive
secants, not independent-host sizing rules. Saturated 16-call steps are
excluded from this comparison.

## Cost and remaining evidence

**NOT-RUN — monetary cost:** no machine or transfer prices were supplied.
At the latest T=4 (earlier T=8), any input-priced allocated cost/call-hour is an
**upper-bound estimate** at a demonstrated floor, assuming unchanged workload and prices. It includes
all three workers plus relay/control/store, including spare capacity.
It is not an operating recommendation. Cost validation now requires a passing
relay step and a validated planned sample load, and reports missing prerequisites
explicitly.

Product-default buffer-on hosted acceptance is recorded above. **NOT-RUN —
relay packet rate limit, sustained capacity, region/far-caller guarantees,
60-second consent and independent-machine sizing.**

## Latest local Mac smoke (buffer-on)

The single 2026-10-11 darwin/arm64, Go 1.26.9 loopback smoke used two calls,
two callers per generator process, and four 20-second steps. All verification
checks finished before the smoke. The relay reported **buffer-on** in every
step, and every service resource window was complete.

| Trial | Verdict | Commands/call-second | Redis B/call-second |
| --- | --- | ---: | ---: |
| Worker move-all | Pass | 67.87 | 86,429 |
| Relay move | Pass | 67.55 | 85,357 |
| N+1 kill/move | **Inconclusive: generator saturation** | 66.65 | 85,317 |
| Loaded drain | Pass, 4.35 ms API interval; worker confirmed empty | 68.17 | 86,034 |

The N+1 generator's peak one-second-interval send-lateness p99 was **22.4 ms**
and skipped frames reached **1.25%**, exceeding the unchanged 20 ms/1% limits.
Its whole-step send-lateness p99 was 2.4 ms. The command exited 1, the outcome
was inconclusive, and T=2 remained unvalidated. This is a generator limitation,
not a service failure. No retry, buffer override or threshold change was used.
Cost printed `NOT-RUN(no input prices)`. All **27 owned process PIDs were gone**,
and the four per-step reports matched the top-level report.

VERIFIED — gofmt/diff, native and Linux vet, lint v2.14.0, build, all-package
memory race coverage, and Redis-backed package race coverage using an owned
non-6379 Redis that was reaped. The initial ten-minute all-package run timed
out only in the full call harness; that package passed with the repository's
30-minute full-harness allowance. Both shared-package CI regressions and all
three capacity verdict regressions were reproduced red before their fixes.

PARTIAL — this single local smoke did not validate N+1 load. It does not
replace the passing hosted T=4 evidence or establish namespace/host sizing.

## Earlier local Mac evidence (pre-#35)

Earlier two-call, 20-second loopback trials passed with the former forced-on
relay buffer. They are separate local checks and do not establish hosted
capacity or namespace behavior. The pre-#35 product-default smoke is recorded
separately below; it does not replace the hosted trials.

The single pre-#35 default smoke ran on 2026-10-11, darwin/arm64, Go 1.26.9,
with two calls and four 20-second steps. This run's validation checks finished before the smoke.
All four generators were valid and every service resource window was complete.
The running relay reported **buffer-off**.

| Trial | Verdict | Commands/call-second | Redis B/call-second |
| --- | --- | ---: | ---: |
| Worker move-all | Pass | 67.97 | 86,543 |
| Relay move | Pass | 67.75 | 85,911 |
| N+1 kill/move | **Fail: audio and video loss** | 66.27 | 85,872 |
| Loaded drain | Pass, 8.88 ms API interval; worker confirmed empty | 68.27 | 86,420 |

The command exited 1 and the planned load remained **unvalidated**. This is
the expected pre-#35 buffer-off recovery result; no buffer override or threshold
change was used to turn it into a pass. Cost printed `NOT-RUN(no input prices)`.
Per-step and top-level reports agree, and all 27 owned process PIDs were absent
after cleanup. The relay step's Redis split was **50,169 session-store**,
**4,418 owners/routes**, and **31,323 frame-cache B/call-second**, summing to the
reported total. This local observation is not Linux namespace evidence.

VERIFIED — format/diff, vet, Linux vet, lint v2.14.0, build and changed-package
race tests on memory and owned non-6379 Redis. Regression evidence includes
red→green unexpected-exit/takeover/gap guards and shared-window weighting;
a real child exit retains its error. Partial drain, empty-worker confirmation,
report agreement, cost prerequisites, runtime buffer state, client labels,
default TLS and dial timeout are tested. The test Redis was reaped.

Post-#35 product-default buffer-on hosted evidence is recorded above. This
pre-#35 local smoke made no passing N+1 operating-load claim.
