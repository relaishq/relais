# Coding standards

Reviewers apply this checklist to each change. `make check` enforces the mechanical rules: shipped-code example dependencies, behaviour names, shared Redis test endpoints, workflow Redis ports, local paths in Markdown, lint version parity, and configured formatting.

## Claims match evidence

- Cite the test, run or measurement behind each claim in docs, ADRs, result tables and PR text. Label numbers from one machine or one run as such.
- “Pass” means the gate ran under the stated condition. Use VERIFIED, PARTIAL or NOT-RUN for verification status.

## Verdicts and gates

- Test every verdict: pass, fail and inconclusive. Show that a known-bad input fails. “Inconclusive” must never hide a failure.
- Check each phase. A whole-run average must not hide a failing phase.

## Call safety

- Use typed errors on move, takeover and placement paths. Classify definite and uncertain outcomes. Bound each retry and end it with a typed outcome. See [ADR 0002](docs/adr/0002-checkpoint-age-envelope.md) on uncertain ownership and terminal accounting, and [ADR 0008](docs/adr/0008-store-outage-budget.md) on command and retry bounds.
- Never drop input and report that nothing happened. Never report a refusal as success. Preserve accepted input and explicit outcomes as described in [ADR 0010](docs/adr/0010-pluggable-agents.md).
- A fail-closed path must not turn a survivable fault into a dropped call. Preserve live calls through transient store faults under [ADR 0008](docs/adr/0008-store-outage-budget.md). A successful hang-up is a terminal end, not a loss ([ADR 0002](docs/adr/0002-checkpoint-age-envelope.md)).

## Repeating defect classes

- Check that size and byte reservations follow configuration ([ADR 0002](docs/adr/0002-checkpoint-age-envelope.md), [ADR 0010](docs/adr/0010-pluggable-agents.md)).
- Give callbacks copies: they can run after cancellation ([ADR 0006](docs/adr/0006-agent-interface.md)).
- Handle the store clock stepping backward ([ADR 0002](docs/adr/0002-checkpoint-age-envelope.md)).
- Keep Redis I/O and heavy loops out of hot-path locks. Check lock costs against CI runner timings, as in [ADR 0009](docs/adr/0009-planned-move-export-replay.md).
- Bound memory and work that a caller can grow ([ADR 0010](docs/adr/0010-pluggable-agents.md)).
- Check timing margins against CI runner timings. A fast local machine is insufficient evidence ([ADR 0009](docs/adr/0009-planned-move-export-replay.md)).

## Tests

- Every bug fix needs a regression test that fails on the old code. Call-loss regression tests also run in `-short`.
- Heavy tests skip in `-short`. Keep the short harness within its time budget.
- No test depends on a 100 ms callback deadline under `-race`.

## Compatibility

- Preserve legacy echo and demo behaviour, golden fixtures and documented guarantees unless the ticket says otherwise.
