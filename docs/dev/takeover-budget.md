# Takeover evaluation cost (#123)

The configured per-event cap is 8192 cells. A live A5-class takeover with about
14.4 ms start uncertainty exceeded the former 4096-cell budget. The synthetic
15 ms fixture did not bound live cell density, so its earlier sizing condition
no longer holds. The larger cap adds operational margin. Two end/floor points
and removal of irrelevant previous-floor choices remain in use. The cell
budget still counts each start × end × floor once, evaluating every
first-baseline reference in it.
Exhaustion still reports unsupported evidence as inconclusive and widens
floor uncertainty. Later exhausted events keep established first-event
references in their reports. Exhausted first events still leave them unknown.
Pass/fail thresholds, the 20-move / 10-takeover pooled minimum, and the
exclusion policy are unchanged; no judge acceptance rule changes.

## Reproduction and fixture

```sh
RELAIS_MEASURE_CALLER_COST=1 RELAIS_TEST_STRICT_TIMING=1 \
  go test ./pkg/callharness \
  -run '^TestTakeover(DensityMeasurement|CapCPUMeasurement)$' -count=3 -v

go test ./pkg/callharness \
  -run '^Test(Takeover(GridEvidenceEquivalent|BudgetCrossover|BudgetMargin|ReplayFixtureGeometry)|ClassificationTwoPointRegionCoverage)$' -count=1 -v

RELAIS_MEASURE_CALLER_COST=1 go test ./pkg/callharness \
  -run '^TestDistributedReportBudgetEvidenceEquivalent$' -count=1 -v

# Repeat with RELAIS_CALLER_COST_PROCS=2. Add
# RELAIS_MEASURE_CALLER_COST_HEAVY=1 to expire 30 receipts per caller.
RELAIS_MEASURE_CALLER_COST=1 RELAIS_TEST_STRICT_TIMING=1 \
  RELAIS_CALLER_COST_PROCS=1 go test ./pkg/callharness \
  -run '^TestCallerCost$' -count=1 -v
```

`replayDensityCall` retains 185 s with 180 s configured retention: 9250 Opus
units (50 Hz), 5550 decoded video frames (30 fps), and 1.1 video RTP packets
per frame. A shared audio/video replay queue returns 42 held packets in 25 ms.
It includes pre-checkpoint repeated returns, the outage backlog, gated live
traffic after the drain, the decode-chain restart at worker handover, and
microsecond return/decode jitter. The 480 ms outage is slightly longer than
the reported live maximum, giving a conservative backlog. Previous recovery
has a 10–15 ms floor interval around the first fresh returns after its drain.
Moves use a point floor. Takeover receipt completion falls within replay.

Clock RTT is 6 ms; symmetric host bounds yield the compared 10, 12 and 15 ms
mapped axes. These are synthetic retained-history measurements, not a trace
replay or an assertion about the next remote run. A separate phase sweep shifts
kill by ±2 ms and receipt completion by −4, −1, +1 and +4 ms.

## Historical local measurements (4096-cell version)

Go 1.26.9, darwin/arm64, GOMAXPROCS=2. CPU is process user + system CPU from
`getrusage`, including boundary construction and evaluation, excluding fixture
construction and its one-time measurement index. CPU columns are medians of
three runs. The old oracle has an unlimited cap and the original three-point
start/end/floor grids; the after run is also uncapped.

| Event | Width | Cells before | Cells after | CPU before | CPU after |
| --- | ---: | ---: | ---: | ---: | ---: |
| Move | 10 ms | 211 | 163 | 10.313 ms | 8.936 ms |
| Move | 12 ms | 212 | 163 | 10.165 ms | 8.923 ms |
| Move | 15 ms | 295 | 223 | 9.580 ms | 9.389 ms |
| Takeover | 10 ms | 5270 | 714 | 34.652 ms | 11.444 ms |
| Takeover | 12 ms | 11288 | 952 | 62.987 ms | 12.494 ms |
| Takeover | 15 ms | 14552 | 1224 | 81.008 ms | 15.220 ms |

The 15 ms phase sweep peaks at 1702 cells: 8192 / 1702 = 4.81x fixture
headroom. This does not establish live-run headroom.
A full-cap stress uses wider 150 ms axes and a recent floor that remains
active, with one and four distinct first-baseline references. The earlier cap
trials actually exhausted 4096 cells. Their worst observed CPU was
**0.0443 s per event**
(20 trials per reference count across four measurement invocations). The
opt-in strict gate requires capped CPU below 0.25 s on the measurement host.
This is a calibration gate, not a hardware-independent wall-clock timeout;
no evidence is dropped based on elapsed time.

The earlier rule kept 4096 while realistic 15 ms phase maxima stayed below
half the cap. The live event invalidates that condition. Size operational
limits with margin, and repeat full-cap CPU measurements and the 1-P/2-P
caller gates when resizing. Select the largest power-of-two cap at or above
8192 whose measured worst capped CPU is at most 0.125 s/event, giving 2x
margin under the unchanged 0.25 s calibration gate. Keep at least 2x the
measured realistic cell maximum. The new cap does not change product
thresholds or turn exhausted evidence into a pass.
Existing bounded history, event/reference counts and the cell cap bound work;
#118's yields remain in boundary building and every 32 evaluated cells.

The #118 cost gate (three video callers, 30 receipts each) passed at one and
two scheduler Ps. At one P: report plus JSON 1.720 s, pacer lateness p99/max
1.745 ms, zero skipped frames. At two Ps: report plus JSON 1.678 s, p99/max
1.029 ms, zero skipped frames. Delayed-maintenance stress expiring 30 receipts
per caller also passes: one P has p99 7.050 ms / max 9.098 ms; two Ps have p99
1.029 ms / max 3.935 ms. Both skip zero frames and build/encode reports in
1.70 s. Evaluation remains outside the recorder lock.

## Cap calibration (2026-11-16)

Go 1.26.9, darwin/arm64, GOMAXPROCS=2. Each batch ran
`TestTakeoverCapCPUMeasurement -count=3`: five trials per reference count in
each invocation, 15 trials with one reference and 15 with four references.
All trials reached the cap. The measurement includes boundary construction
and cell evaluation; it excludes fixture and index construction.

| Cap / batch | One-minute load | Worst CPU, 1 reference | Worst CPU, 4 references | At most 0.125 s/event |
| --- | ---: | ---: | ---: | --- |
| 16384 / shared load | about 19–22 | 0.150671 s | 0.202602 s | No |
| 8192 / shared load | about 18–22 | 0.078970 s | 0.141760 s | No |
| 8192 / qualifying | 7.67 at start | 0.061254 s | 0.068222 s | Yes |

Shared-load readings inflate process CPU; the 16384 cap fails the 2x target
even there by a wide margin and is not used. 8192 qualifies when measured at
load at most 8: its worst capped CPU is 0.068 s/event, 3.7x under the 0.25 s
gate.

The strict caller cost gates also pass at the 8192 cap (same session):

| Scheduler Ps | Maintenance | Report + JSON | Pacer p99 / max | Skipped frames |
| ---: | --- | ---: | ---: | ---: |
| 1 | normal (2 retained) | 1.796 s | 10.483 / 10.483 ms | 0 |
| 1 | delayed (30 retained) | 1.766 s | 8.676 / 9.670 ms | 0 |
| 2 | normal (2 retained) | 1.724 s | 2.043 / 2.043 ms | 0 |
| 2 | delayed (30 retained) | 2.035 s | 6.473 / 8.024 ms | 0 |

The opt-in full-report oracle reports identical full evidence for 30 events,
including 29 capped events.

## Why the reductions preserve evidence

For constant branch membership, the integer region `[b+1, next-1]` is already
represented by `next-1`; `hi` represents the last region. Both endpoints remain.
Thus end and floor grids use `b-1, b`. An exhaustive integer-domain test covers
isolated equality, adjacent boundaries and clipped endpoints.

**Start needs an exception.** Elapsed durations are affine in start, and a
right-hand branch can select a new observed frame/run. Its maximum elapsed
time occurs at `b+1`. Blindly deleting that corner narrowed a dense 15 ms move's
upper duration from 95.999999 ms to 93.500001 ms. `classificationStartPoints`
retains it. Two-point verdict coverage alone is insufficient to preserve the
published duration bounds. The interior nonmonotonic tests remain unchanged.

When `floor.Upper <= max(0, start.Lower - baselineWindow)`, every floor selects
the same units because `BaselineStart = max(0, start-window, floor)`. A takeover's
next floor depends on observed recovery/end, not that unused input floor, so
one representative suffices. A move carries its floor forward, so it keeps
both floor endpoints to preserve that output interval. Active floors retain
the full two-point boundary grid; unknown/wide floors are not narrowed.

The frozen pre-123 classifier/grid compares unlimited verdicts (including
reason order), durations, baseline-floor bounds and first baselines on dense,
jittered, replay, and recent-active-floor variants, for moves and takeovers at
all three widths. The crossover test proves a real replay event exhausts the
old 4096 cap, completes within the production cap after reduction, and exactly
matches the unlimited old oracle. Existing measurement/budget oracles use the
new grid while retaining their independent measurement/accounting logic.
The full-report oracle deliberately widens worker bounds to ±1 s and uses
2 s takeover spans with an earlier arm time, keeping the entire mapping
inside valid ordering bounds. The former narrow fixture no longer exhausts
after dominated-floor removal.
It compares all fields across 30 reports and requires actual capped events,
including the preserved references in later exhausted reports.
`TestDistributedReportFixtureExhaustsCellBudget` checks its first capped
event without running the full legacy oracle, including the exhaustion reason
and inherited references.
