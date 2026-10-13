# Distributed evaluation budget (#120)

The production limit remains 4096 start/end/floor cells per event. Each cell
is checked against every first-event baseline reference. References affect
the classification agreement, but do not consume additional cells. The
`evaluations` field now reports that cell count.

Only a completed self evaluation establishes the call's first-event baseline
references. Later exhausted or unmapped events cannot erase those references.
An exhausted first event still leaves that baseline unknown. Exhaustion still
widens the baseline-floor interval and leaves unsupported metrics inconclusive;
this fix does not narrow that uncertainty or choose a new baseline.

## Proof

`TestDistributedCellBudgetEquivalence` compares the frozen pre-change classifier
against the production-called budget seam with an effectively unlimited cap.
It covers the dense 185 s recording and an additional deterministic jitter
variant, for both moves and takeovers, with four different first-baseline
references. Every metric verdict (including reasons), duration interval and
baseline-floor interval matches. An independent grid enumeration checks that
`Evaluations == cells`, where the old accounting was `cells × references`.
The test also checks the exact-budget boundary, one cell less, and the fixed
production cap.

`TestDistributedExhaustedEventKeepsFirstBaselines` drives the real three-event
handoff. The first event establishes a known latency failure. The second event
exhausts the production 4096-cell limit. The third retains the first references,
completes below the cap, and preserves that known failure. Its freshness
verdicts exactly match an uncapped evaluation given the same conservative floor
from event two. The floor is deliberately retained: comparing against a run
that did not exhaust event two would remove real uncertainty from the inputs.
`TestDistributedExhaustedFirstBaselineStaysUnknown` prevents later events from
inventing a replacement first baseline when the self evaluation exhausted.

The five required existing tests cover zero intervals, full-history equivalence,
interior nonmonotonic classification, interior end-axis classification and
dense WAN RTP. The #118 performance oracle now uses the corrected accounting
and handoff rules; its older measurement and boundary implementations remain
independent. The new budget test retains the pre-120 reference-count algorithm.

## Retained-history counts at narrow clock bounds

Run the #118 cost fixture with the narrower measured clock inputs:

```sh
RELAIS_MEASURE_CALLER_COST=1 go test ./pkg/callharness \
  -run '^TestCallerEvaluationCounts$' -count=1 -v

go test ./pkg/callharness \
  -run '^TestDistributed(CellBudgetEquivalence|ExhaustedEventKeepsFirstBaselines|ExhaustedFirstBaselineStaysUnknown)$' \
  -count=1 -v
```

These are offline Go 1.26.9 measurements with one scheduler P, three video
calls, 180 s configured retention and 185 s of observations. Each call has
50 Hz audio and 30 fps video with three RTP packets per frame, plus the
merged #119 audio/video repeats and unmatched observations. The move is
at 5 s; the takeover is at 10 s. All three callers produced the same counts.
The first event established one distinct first-baseline reference.

| Clock RTT | Host offset bound | Mapped width | Move cells | Takeover cells |
| ---: | ---: | ---: | ---: | ---: |
| 3 ms | ±2 ms | 7 ms | 171 | 40 |
| 4 ms | ±2 ms | 8 ms | 125 | 48 |
| 3 ms | ±3 ms | 9 ms | 156 | 64 |
| 4 ms | ±3 ms | 10 ms | 144 | 72 |

These counts depend on branch boundaries and the correlated planned-move
span; they need not increase monotonically with width. Every count matches an
independent cell enumeration and remains below 4096. They are fixture results,
not measurements of a live remote run.

The #118 cost check passed again after rebasing onto merged #119: three reports
plus JSON took 2.938 s; arm/receipt plus maintenance had p99/max 2.554 ms and zero
skipped frames on one scheduler P. The 10 ms takeover count increased from 64
to 72 with the repeated-return fixture; the other counts remained unchanged.
