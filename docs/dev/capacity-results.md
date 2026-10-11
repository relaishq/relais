# Capacity evidence for #40

## Hosted Linux run 38136419832

[Run 38136419832](https://github.com/relaishq/relais/actions/runs/38136419832),
commit `f132ffb`, passed its recorded acceptance policy. Linux/amd64,
Go 1.26.9, shared four-vCPU AMD EPYC 9V74 runner. The workload was audio plus
the harness VP8 fixture, 30 seconds per step, two callers per generator process,
GOMAXPROCS=1. Topology: Linux namespaces with an unshaped LAN, one relay,
control and Redis. Worker ramps used two workers; relay and fault samples used
three. Reports and process logs are in the run's `capacity-linux-38136419832`
artifact.

The relay buffer was **on**, through the former capacity-only override;
positive buffer packet/byte gauges confirm it. The load driver now passes no
buffer override and records the running buffer configuration explicitly.
This historical run therefore does not measure the pre-#35 buffer-off product
default. New service-exit, takeover-count, whole-call gap, drain-empty and
survivor-peak guards require a fresh hosted run.

| Trial | Call counts | Usable result |
| --- | --- | --- |
| Concentrated worker, move all | 2, 4, 8 | Pass; worker lower bound 8 calls |
| Relay, spread placement and one move | 2, 4, 8 | Pass; relay lower bound 8 calls and 1371 packets/s |
| Both ramps at 16 | 16 | Inconclusive: generator saturation |
| N+1 kill, then planned move | 8 | Recorded pass; 3 killed calls recovered, one subsequent move |
| Loaded drain | 8 total, 3 drained | Recorded pass; 43 ms including the old status/result-recording overhead |

**Eight is the demonstrated generator ceiling on this shared four-vCPU
runner**, not worker or relay capacity. Sixteen exceeded generator interval
p99/skipped-frame limits and supplies no service-capacity claim. These are
short samples, not sustained-load or real-camera limits.

For C=8, R=8 and W=3, the sample load was
`T = min((W-1)*C - 1, R) = 8`. Eight calls over three workers is a **floor**,
not an operating recommendation. Survivors peaked at **5 of 8 calls (62.5%)**
in the N+1 sample; the other survivor peaked at four. This proves failover
mechanics at that load, not survivors near capacity. Drain survivors peaked
at four each. The **relay packets-per-second limit is NOT-RUN**; only a lower
bound of **1371 forwarded datagrams/s** was measured.

## Store and CPU observations

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

## Memory and marginal slope

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
At T=8, any input-priced allocated cost/call-hour is an **upper-bound estimate**
at a demonstrated floor, assuming unchanged workload and prices. It includes
all three workers plus relay/control/store, including spare capacity.
It is not an operating recommendation. Cost validation now requires a passing
relay step and a validated planned sample load, and reports missing prerequisites
explicitly.

**NOT-RUN — new hosted acceptance guards and product-default buffering.**
Native Linux must be sampled again with the current relay default. Relay packet
rate limit, sustained capacity, region/far-caller guarantees, 60-second consent
and independent-machine sizing remain NOT-RUN.

## Local Mac evidence

Earlier two-call, 20-second loopback trials passed with the former forced-on
relay buffer. They are separate local checks and do not establish hosted
capacity or namespace behavior. The current product-default smoke is recorded
separately below; it does not replace the hosted trial.

The single current-default smoke ran on 2026-10-11, darwin/arm64, Go 1.26.9,
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

NOT-RUN — current hosted acceptance and post-#35 product-default buffer-on
capacity. No local passing N+1 operating-load claim is made.
