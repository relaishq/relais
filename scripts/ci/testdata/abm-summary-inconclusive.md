## Operator rehearsal summary

Verdict: **inconclusive**

| Gate | Status | Media status |
| --- | --- | --- |
| rollout | passed | passed |
| control-restart | passed | inconclusive |
| upgrade-rollback | passed | passed |
| relay-deploy | inconclusive | passed |

Pooled media: status **inconclusive**, qualified takeovers 7, qualified moves 15

### Timing undecided

| Gate | Phase | Event | Metric | Causes |
| --- | --- | --- | --- | --- |
| rollout | 0 | 2 | distributed_window | metric-undecided, trial-count-insufficient |
| control-restart | 1 | 0 | first_content | trusted-coverage-below-80-percent |

### Untrusted caller events

- Caller events with `timingEvidence: false`, release callers (`callerTiming`): 2 of 3 events
- Caller events with `timingEvidence: false`, head caller (`headCaller.callerTiming`): 1 of 2 events

### Relay host loss (5 s budget)

Procedure completed: missing
Failed phase: missing
Call gaps: missing

### Clock selection

Cause: **admitted**

Selected bounds: frontendControlBoundNs 120000, frontendWorkerBoundNs 170000, driftPpm 100

Failed windows: 0
