# Extracted rehearsal reports

These small JSON fixtures retain fields used by `rehearsal_flakes.py`, plus the
pooled exclusion and saturation counters that explain why no counter guard applies.
Ten come from the public `operator-rehearsal-<run>-<attempt>` artifacts.
Timing rows retain distinct cause lists because the classifier checks only
whether every cause is allowed. No full media evidence or text logs are stored.

There are eleven fixtures: four historical generator positives, five historical
negatives, one Go-generated preparation lease positive and one unmodified
clock-selection positive. Both `excludedEvents`
and `saturationOverlaps` are zero in the historical generator positives. These
event counters are not required: the pooled failure in those runs comes from a
`clock-coverage-missing` timing row, accepted only with generator evidence.
Their original evidence is retained unchanged.

| Run-attempt | JSON classification |
| --- | --- |
| 43668182387-1 | generator-saturation-flake |
| 43683756709-1 | generator-saturation-flake |
| 43713479010-1 | generator-saturation-flake |
| 43713957392-1 | generator-saturation-flake |
| 43774355478-1 | not-known: unexplained-failure-code |
| 43779279943-1 | not-known: not-a-clean-failure |
| 43776714400-1 | not-known: generator-evidence-not-clean |
| 43743583263-1 | not-known: unexplained-timing-cause |
| 43713957392-2 | not-known: not-a-clean-failure |
| lease-preparation-refusal | lease-expired-flake |
| 44984258994-1 | clock-select-flake |

The two 4371 attempt-1 reports have generator exclusions, an interval-max
violation, a move-count shortage and clock coverage gaps. Their JSON has no
precondition refusal. The 43774355478-1 text log mentions lease expiry, but its
JSON has `incomplete-armed-inventory` and no explicit refusal category. It stays
unknown under the JSON-only contract.

`lease-preparation-refusal.json` is generated and checked by
`TestLeasePreparationFlakeFixture` in `internal/operatorrehearsal`. It runs the
real sequence with a preparation refusal, calls `Report.Evaluate`, then checks
the extracted serialized report. Media ports provide a clean three-phase test
population; this is a contract fixture, not a hosted rehearsal measurement.
Regenerate it by setting `RELAIS_UPDATE_FLAKE_FIXTURE=1` when running that test.
The real failure set includes `media-missing-or-failed` because a preparation
refusal creates no gate media, as well as `procedure-error` for the reason.
