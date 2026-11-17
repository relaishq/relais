# Distributed evaluation budget (#120)

The production limit is 8192 start/end/floor cells per event. Each cell
is checked against every first-event baseline reference. References affect
the classification agreement, but do not consume additional cells. The
`evaluations` field now reports that cell count.

Only a completed self evaluation establishes the call's first-event baseline
references. Later exhausted or unmapped events cannot erase those references.
An exhausted first event still leaves that baseline unknown. Exhaustion still
widens the baseline-floor interval and leaves unsupported metrics inconclusive;
this fix does not narrow that uncertainty or choose a new baseline.

A live A5-class takeover with about 14.4 ms start uncertainty exceeded the
former 4096-cell budget. The 8192-cell candidate adds cell margin; the
synthetic 15 ms fixture is not a bound on live cell density. Pass/fail
thresholds, the pooled minimum of 20 moves / 10 takeovers, and the exclusion
policy are unchanged. The judge still requires consistent first-event
references; later exhaustion now preserves them in the event report as well
as the handoff. Missing first-event references remain invalid evidence.
Cap CPU measurements and caller timing status are recorded in the
[calibration results](takeover-budget.md#cap-calibration-2026-11-16).

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

`TestDistributedLaterExhaustionBaselineReport` checks that inherited references
survive exhaustion while unsupported metrics remain inconclusive and floor
uncertainty widens. `TestDistributedExhaustionKeepsIndependentFailures` checks
that server loss, identity corruption and independent gap failures still fail.
The judge-level `TestExhaustedEventBaselineCollections` accepts retained
references in a later exhausted event, keeps its media verdict inconclusive,
and rejects an exhausted first event with missing references.

`TestDistributedExhaustedEventKeepsFirstBaselines` drives the real three-event
handoff. The first event establishes a known latency failure. The widened
takeover start interval and span force the second event to exhaust the
production 8192-cell limit; that event retains those references in its own
report. The third retains the first references, completes below the cap, and
preserves that known failure. Its freshness verdicts exactly match an uncapped
evaluation given the same conservative floor from event two. The floor is deliberately retained: comparing against a run
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

These pre-123 counts came from the earlier fixture, which lacks real takeover
replay/decode boundary density. Do not use them to size the live takeover budget.
See [takeover-budget.md](takeover-budget.md) for the replacement fixture, measured
CPU, current reductions and cap rule.

These counts depend on branch boundaries and the correlated planned-move
span; they need not increase monotonically with width. Every count matches an
independent cell enumeration and remains below the former 4096-cell cap. They are fixture results,
not measurements of a live remote run.

The #118 cost check passed again after rebasing onto merged #119: three reports
plus JSON took 2.938 s; arm/receipt plus maintenance had p99/max 2.554 ms and zero
skipped frames on one scheduler P. The 10 ms takeover count increased from 64
to 72 with the repeated-return fixture; the other counts remained unchanged.
