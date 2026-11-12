# CI jobs

This page details the jobs in `.github/workflows/ci.yml`. For the contributor
summary, see [PR and full checks](../../CONTRIBUTING.md#pr-and-full-checks); for
local Redis setup, see [Redis and tests](../../CONTRIBUTING.md#redis-and-tests).
`make check` runs the PR `quality` and `unit-tests` jobs locally
([local PR checks](../../scripts/dev/README.md)).

## Pull requests

PR CI runs lint (v2.14.0), build, vet, race tests for every package except
`pkg/callharness` with Redis required, the short memory harness, a separate
race run of `TestRelayUncertainTransferSurvives` with Redis required, and the
memory timing smoke in parallel jobs. Evidence judge tests run separately: PRs
use a deterministic required/optional field sample; main/nightly/manual runs use
full sweeps. Named regressions, pinned-release comparisons and all owned mutants
run on PRs. The smoke retains 20 planned moves,
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
