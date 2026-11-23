# CI jobs

This page details the jobs in `.github/workflows/ci.yml`. For the contributor
summary, see [PR and full checks](../../CONTRIBUTING.md#pr-and-full-checks); for
local Redis setup, see [Redis and tests](../../CONTRIBUTING.md#redis-and-tests).
`make check` runs the PR `quality` and `unit-tests` jobs locally
([local PR checks](../../scripts/dev/README.md)).

Final validation is CI on the exact revision that merges: the PR head, or the
batch head when several PRs are tested and merged as one batch. Before handover,
run `make check-quality` and the changed packages' tests locally; the full race
suites count as final from that CI run.

## Pull requests

PR CI runs lint (v2.14.0), build, vet, race tests for every package except
`pkg/callharness` with Redis required, the short memory harness, a separate
race run of `TestRelayUncertainTransferSurvives` with Redis required, and the
memory timing smoke in parallel jobs. Evidence judge tests run separately: PRs
use a deterministic required/optional field sample; main/nightly/manual runs use
full sweeps. Named regressions, pinned-release comparisons and all owned mutants
run on PRs; the owned mutants run in their own `gate-mutation` job, in parallel
with the race tests. The smoke retains 20 planned moves,
20 hard kills, planned sequence wrap, and the full consent windows. Short mode reduces the
baseline echo calls to 5 s; some fault scenarios retain their fixed duration.
It skips `TestPlannedHandoverKeepsConsent` and the Redis-only uncertainty
scenarios. The separate PR Redis step restores uncertainty coverage.
The `build` job aggregates these results. Making that gate required in GitHub
settings is a separate admin decision. Superseded PR runs are cancelled;
main pushes and nightly runs are not cancelled by this policy.

The Redis Cluster job also runs on PRs. It uses disposable local Redis processes
on `ubuntu-24.04`, with TLS/ACL and process-topology coverage, and runs packages
serially to protect the media timing gates. It remains outside the `build` aggregate.

## Main, nightly and manual runs

Nightly at 08:00 UTC, pushes to `main`, and manual `workflow_dispatch` runs
add the full race harness and timing gates on memory and Redis, benchmarks,
and `make crash-run`. The `store-stall` job adds five trials per case on nightly/manual dispatch,
with failure logs and a 25-minute limit: live workers survive 5 s, 15 s and 29 s
pauses with a waiting standby; 1.5 s crash resumes with scaled margins; 5 s crash
counts checkpoint-unsafe loss; 35 s live pause counts `ownership_expired` loss.
All require zero decryption failures. The default store outage budget is 30 s;
configure matching `-store-outage-budget` on worker and control
([ADR 0008](../adr/0008-store-outage-budget.md)).
Relay TTL stays 600 ms; reachable staleness observation preserves fast frozen
relay takeover while failed polls prevent outage-driven takeover
([ADR 0004](../adr/0004-standby-relay.md)).
To run manually, select **Actions → CI → Run workflow** on the desired branch.

## Redis in tests

The short targets clear the Redis test address and disable its requirement,
so all Redis subtests skip even when the invoking shell has Redis configured.
Storage stream and group tests skip before constructing a client when
`RELAIS_TEST_REDIS_ADDR` is unset. Use a disposable Redis for local tests.
Storage benchmarks also skip Redis unless `RELAIS_TEST_REDIS_ADDR` is set.
Memory benchmarks always run; Redis variants use random prefixes and remove
their keys at cleanup. With `RELAIS_TEST_REDIS_REQUIRE=1`, missing or
unreachable Redis fails integration tests and Redis benchmarks.

The separate `operator-rehearsal.yml` job adds a clock admission step (152 seconds plus bounded preflight, at most 162)
after negative controls restore. It archives sanitized evidence, policy, receipt
and status; report/log print bounds/counts or refusal. Selection is recorded only
([admission details](operator-gates.md#rehearsal-admission-receipt-slice-5c)).
On every outcome, `scripts/ci/rehearsal_summary.py` writes the verdict, gate and
media statuses, timing-undecided rows, untrusted caller event counts and the clock
selection cause to the run page. It reads only `report.json` and
`clock-selection-status.json` and never changes the job result.

## One automatic rehearsal rerun

`operator-rehearsal-flake-rerun.yml` handles failed `Operator rehearsal` runs
only on attempt 1 and only for `pull_request` or `push`. Scheduled and manual
runs stay red. It reruns failed jobs once, then adds one short line with the
signature, run URL and attempt numbers to the open tracking issue
[#511](https://github.com/relaishq/relais/issues/511), "Operator rehearsal
automatic reruns". The workflow-level `RERUN_TRACKING_ISSUE` value is the only
place that names it. The second attempt cannot trigger another automatic retry.

`generator-saturation-flake` and `lease-expired-flake` require a failed schema-1
report, clean calibration, no latch, no errors or uncertainties, completed
cleanup, four identified gates and no unexplained failure code or media failure. `generator-saturation-flake` requires
only `pooled-media-not-passed`, completed procedures and media, generator event
exclusion with an interval-max violation, at least 10 qualified takeovers and
fewer than 20 qualified moves. Pooled `excludedEvents` and `saturationOverlaps`
are not used: both counters are zero in the four historical generator positives,
whose pooled failure comes from a `clock-coverage-missing` timing row; the rule
accepts that row only together with generator evidence.
Each media phase inventory must match the exact expected population.
A trial-count shortage alone is insufficient.
`lease-expired-flake` requires exactly one gate with `reason: clock-lease-expired`
and an empty category, refused before its procedure starts. Its media is absent;
the other three procedures complete, with exactly three CPU phases and matching
media phase inventories. Later gates must complete, rather than report
`prior-gate-stopped`. The exact failure set contains that gate's
`procedure-incomplete`, `media-missing-or-failed`, `procedure-error`, `not-passed`
and `pooled-media-not-passed`. The missing-media failure is emitted by Go for a
preparation refusal. A missing lease or incomplete armed inventory alone does
not prove lease expiry.

`clock-select-flake` requires a failed schema-1 report with no latch, completed
cleanup and no uncertainties or exemptions. Failed windows must be nonempty.
Clock selection and every failed window must report `clock-select-ceiling`.
The errors are exactly `clock-binding-failed` and `clock-binding-missing`. The failure set adds only
`calibration-failed`, `gate-count-differs` and `pooled-media-not-passed`.
No gate, clock, caller timing, undecided timing or CPU phase can be present.
Pooled media must be null, and caller affinities must be empty. Calibration must
be unstarted: failed with only `incomplete-calibration-evidence`, no checks,
zero counters and an empty media window. This signature covers clock selection
failure before any gate or caller work starts.

Missing, malformed or unexplained evidence produces `not-known` and no retry. Classification and a fixed reason appear in
the job summary. Extracted report fixtures live in
`scripts/ci/fixtures/rehearsal-flakes/`; they include a log-only lease refusal
that deliberately stays `not-known` because its JSON lacks that cause.

This privileged workflow checks out only default-branch code. It reads the
triggering run's artifact as untrusted JSON with Python `-I`, outside the trusted
checkout. Artifact text never becomes shell code or comment text. Its token has
only `actions: write`, `issues: write` and `contents: read`. A final live-run
check prevents a queued handler from retrying an attempt that has already changed.
The job also requires the triggering workflow path to be exactly
`.github/workflows/operator-rehearsal.yml`.
