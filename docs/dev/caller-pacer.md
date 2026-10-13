# Caller pacer and final report cost (#118)

The distributed feed's arm and receipt methods validate and retain identities.
They do not evaluate media evidence. The expensive work was in recording
maintenance and final evidence evaluation on the same scheduler P as the media
pacer. Taking the recorder lock out of classification did not keep that shared
P available.

The offline reproduction uses three calls, 180 s configured retention and
185 s of observations: audio at 50 Hz, video at 30 fps with three RTP packets
per frame, and the production send/return indexes. The report fixture has
30 mixed move/takeover receipts per call. Twenty-five of those events reach
the existing 4096-evaluation limit. A second fixture drives the real
`ArmDistributedEvent` and `RecordDistributedEvent` entry points alongside
maintenance while six ticker-driven `framePacer` instances acquire the real
recorder locks, with `GOMAXPROCS=1`.

## Reproduction

```sh
RELAIS_MEASURE_CALLER_COST=1 RELAIS_TEST_STRICT_TIMING=1 \
  go test ./pkg/callharness -run '^TestCallerCost$' -count=3 -v \
  -cpuprofile=/tmp/caller.cpu -memprofile=/tmp/caller.mem

go tool pprof -top /tmp/caller.cpu
go tool pprof -top -alloc_space /tmp/caller.mem

# Delayed maintenance with 30 expiring receipts per call, including capped cells.
RELAIS_MEASURE_CALLER_COST=1 RELAIS_MEASURE_CALLER_COST_HEAVY=1 \
  RELAIS_TEST_STRICT_TIMING=1 go test ./pkg/callharness \
  -run '^TestCallerCost$' -count=1 -v

# Full old/new evidence comparison, including the 4096-cell limit.
RELAIS_MEASURE_CALLER_COST=1 \
  go test ./pkg/callharness \
  -run '^TestDistributedReportBudgetEvidenceEquivalent$' -count=1 -v

# Actual HTTP handlers and UDP-writing video pacers (short live calls).
RELAIS_TEST_STRICT_TIMING=1 go test ./internal/loadgen \
  -run '^TestDistributedFeedWithVideoPacers$' -count=1 -v
```

Strict timing asserts interval p99 below 20 ms, no skipped frames, and combined
report building plus JSON encoding below 10 s for the offline fixture. Timing
is opt-in because shared runner load and race instrumentation affect results.
The ordinary tests compare complete `MoveReport` evidence, judge outcomes,
freshness fields, content-loss counts and classification points against frozen
pre-change implementations. The expensive full-budget oracle is opt-in.

## Cost and change

The baseline CPU/heap profiles attributed 58.5% of sampled CPU and 94.1% of
allocated bytes to freshness evaluation. Each interval cell rebuilt identity
maps and temporary slices. Other repeated work built whole-history boundary
arrays, scanned all decoded frames for the first eligible frame, and copied
and sorted all sends for each small content-loss window. Maintenance also
copied history and built measurement indexes when there were no expiring
events to evaluate.

The implementation reuses view-owned freshness buffers and identity visit
arrays. It caches identical freshness inputs within an event. Each evaluation
still runs, contributes to the same agreement, and counts toward the same
4096-cell limit. It retains only boundaries that can appear in the original
start/end point lists, including translated planned-move boundaries and packet
predecessors. It filters content-loss windows before sorting and uses the
existing sorted frame index for first-content durations. Empty event prefixes
prune history directly. Pruning reuses bounded history buffers and clears
removed pointer slots instead of copying the retained arrays every second.
Those buffers can retain spare capacity up to the recorder's count limits.
Work outside the recorder lock yields between bounded batches so active media
can run on the same P.

The final artifact adds `report_build_duration_ns` to each call report. It
measures evidence snapshot, evaluation and judgment. It excludes teardown,
optional offline decoding, JSON encoding and output transport. Event capacity,
history count limits and the evaluation limit bound the computation; no
wall-clock timeout discards evidence or changes a verdict.

## Local measurements

Measured with Go 1.26.9 on darwin/arm64 and one scheduler P. These are offline
measurements, not a multi-host capacity claim.

| Measurement | Before | After |
| --- | ---: | ---: |
| Report build + JSON, three calls / 30 events each | 34.465 s | 2.612–3.285 s |
| Report allocation, three calls | 53.036 GB | 0.518 GB |
| Arm/receipt + maintenance pacer p99 | 43.965 ms | 2.020–2.477 ms |
| Arm/receipt + maintenance pacer max | 43.977 ms | 2.020–2.477 ms |
| Skipped frames | 12 | 0 |

JSON encoding took 4.793–7.799 ms across the three after runs. The last
empty-prefix prune held the recorder lock for 78–83 µs.

The delayed-maintenance stress expires 30 retained receipts per caller after
an additional 150 s of synthetic recorder time. It evaluates capped events
while pacers run. Its work took 2.619 s, with p99 3.706 ms, max 3.791 ms and
zero skipped frames (675 pacing samples). This is a heavier stress case than
the matched before/after arm/receipt fixture.

The separate real HTTP/UDP test measured p99 2.1 ms, max 2.270 ms and zero
skipped frames. It verifies the transport-to-caller bridge with short calls;
the offline fixture supplies realistic retained-history costs. The offline
pacer fixture isolates scheduler and recorder-lock contention and does not
write actual RTP packets.

The before strict gate failed. Three after runs passed. The full-budget
comparison preserved every field of all 30 move reports, including 25 capped
events. Additional comparisons cover missing content, identity corruption,
unknown content and reversed receipts. Redis was not needed for these checks.
A live multi-host SIGINT run and its network teardown/output time were not run.
