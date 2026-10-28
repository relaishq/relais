# Operator numeric evidence audit

The caller and media judge validate numbers before arithmetic. Counts, packet
counts, elapsed durations and measurements must be nonnegative and within the
operator JSON exact-integer range (`2^53 - 1`). Signed clock coordinates remain
signed. Content identities remain opaque `uint64` identifiers and are never
aggregated. `observe.Numbers` walks the existing product types; it does not
introduce surrogate product schemas.

`observe.AddCount` checks each operand and the remaining range before addition.
`observe.AddSigned` checks both signed overflow directions. Timestamp spans and
ordered intervals are checked before subtraction. A bounded absolute integer
budget leaves headroom for the product judges' compound expressions. These are
representability checks; the product judges and media policy thresholds are
unchanged.

A positive authentication, replay, decryption or ICE failure is recorded
independently before invalid siblings are excluded. Scoped loss receipts and
trusted failure verdicts also survive missing or invalid numeric evidence. Invalid
numbers cannot qualify a phase or increase the pooled acceptance sample.

The audit covered these arithmetic sites:

| Site | Numbers and protection |
| --- | --- |
| `judge-src/numbers.go`: `callFailure`, `reportFailure`, `callNumbers`, `failureTrials`, `trialsFailed`, `poolFailure` | Individual positive failure counters and trusted failure facts; checked decryption component total. Valid gap observations go to the unchanged failure judge independently of invalid diagnostics; numeric rejection alone did not establish population completeness. `collections.go` now checks nonempty populations and multiset containment, including duplicate multiplicities, before quantiles. |
| `judge-src/main.go`: `previewMedia` | Product report numbers checked before `Judge`; checked completed-call, decryption, ICE, qualified-event and metric totals; validated media-gap maximum. |
| `judge-src/main.go`: `parsePreviewMediaFor` | Numeric validation of probes, calls, events and receipts; checked cumulative generator interval and request totals before `ReadSoakProcess`; independent scoped failures retained. |
| `judge-src/main.go`: `pooledPreviewMedia`, `decodePool` | Checked caller-count addition; combined product tree validated before the aggregate judge; numeric type overflow preserves independent facts while marking the pool incomplete. |
| `judge-src/sample.go`: `readSample` | All generator counters, bytes, frames, CPU and lateness; checked interval and lifetime pacing denominators before percentages. |
| `judge-src/exclusions.go`: `sampleFlagged` | Same numeric and denominator checks before saturation comparisons. |
| `judge-src/exclusions.go`: `loadSamples` | Manifest byte count must equal actual bytes; bounded, ordered sample timestamps and durations before cadence subtraction. |
| `judge-src/exclusions.go`: `statsClockExcursion` | Checked signed wall/monotonic delta and cumulative excursion; minimum signed value rejected before absolute value. |
| `judge-src/exclusions.go`: `sampleSpan` | Validated timestamps and intervals before coverage subtraction/addition. |
| `judge-src/exclusions.go`: `saturationBudget` | Ordered bounded media window; clipped, nonoverlapping interval union keeps flagged seconds within media seconds before percentage. |
| `judge-src/exclusions.go`: `callerWindow`, `eventSpan` | Validated call/sample times and elapsed offsets before additions and min/max comparisons. |
| `judge-src/exclusions.go`: `callerEventWindow` | Validated receipt, call and move; checked frame offset and audio/video recovery-plus-stability sums; bounded interval edges, gaps and fixed padding. |
| `judge-src/exclusions.go`: `applySaturation` | Checked retained-plus-dropped sample count; validated event windows and bounded excursion padding. |
| `judge-src/probes.go`: `matchesProbePrefix` | Ordered probe times and bounded RTT before differences and clock-prefix comparison. |
| `pool.go`: `DecodeSummary` | All numeric summary fields checked; invalid or absent diagnostics, including a sibling too large for the product integer type, cannot erase explicit or positive-counter failure. |
| `pool.go`: `ReadPool` | Event/receipt numbers checked without discarding independently valid failure facts; an unrepresentable numeric sibling marks the pool incomplete after strict member checks. |
| `judge.go`: `extract` | Reject negative/member sizes above 64 MiB; compare against remaining 512 MiB total before addition. |
| Caller `feed.go`: `CollectProbes`; `protocol.go`: `Burst.Validate`, `Register`, `Host.Validate` | Clock timestamps, RTT, bounded probe count and CPU counts; order/range checks precede arithmetic. |
| Caller `protocol.go`: `CallerArgs`, `Plan.Validate`, `Manifest.Validate` | Nonnegative bounded clock parameters; 1–1200 second plan; ordered nonnegative manifest times and 20/105 minute windows bound deadline differences/products. |
| Caller `feed.go`: `SaturationWindow.Add`, `RecordGeneratorSample` | Generator numeric validation and pacing denominators; checked floating accumulated flagged/streak time; rolling window clipped to 30 seconds; invalid samples abort before publication. |
| Procedure `caller.go`: `receiptEvents` | Receipt count is bounded by the exact-integer range before grouping. Affected/count fields use complete group lengths; timestamp bounds use comparisons, with no sum or subtraction. Collection errors retain receipts for independent failure judgment. |
| Caller `broker.go`: `Feed`, `sameIdentities` | Sequence exhaustion checked before increment; bounded input lengths keep multiset deltas representable. |
| Caller `bridge.go`: stats and feed loop | Sequence/index exhaustion checked before increment; exhausted feed exits through child shutdown. |
| Caller `transport.go`: `ProtocolMailbox.exchange` | Request-ID exhaustion checked before increment. |
| Caller `host.go`: `CollectHost` | Logical CPU count bounded before increment. |
| Caller `mailbox.go`: `Directory.Read`, `Directory.Create`; bridge spool reads | Fixed bounded blob lengths and fixed extra-byte overflow detection; no evidence-controlled size sum. |
| Caller launcher scripts, broker waits and feed deadlines | Runtime limited to 20 minutes, manifest lifetime to 105 minutes; remaining time uses bounded deadlines and fixed reserves; exit status is compared, never aggregated. |

Regression tests start with the passing pooled fixture, then inject cancelling
counters, extreme counters, negative packet/gap measurements, overflowing pacing
and event-window sums, and independent known failures. Caller tests cover invalid
samples, accumulated time overflow and exhausted sequence numbers. Shared-driver
mutants remove the independent failure check, numeric range checks and addition
guards. The mutation coverage inventory records execution separately from the
original oracle ledger, whose pending cases stay pending.


## Raw required-field contract

The earlier presence lists did **not** cover duration-map and takeover interval
endpoints. Empty objects could become zero bounds. Those lists have been replaced
by `observe.DecodeEvidenceLimit` and the recursive `EvidenceFields` contract.
The mechanism derives member names and nested types from the actual Go structs,
including promoted fields, pointers, slice elements and map values. Map key and
container decoder methods are checked before descent. Every member
is required regardless of `omitempty`, unless `observe/evidence_optional.go`
explicitly documents an exception with a one-line reason. Union payload exceptions
are conditional on the message discriminator. Supplied optional objects still
receive the recursive check. Required omissions, nulls and empty objects are
rejected before product decoding, including every duration/takeover endpoint.

Unique/canonical member checks run first. Incomplete evidence is quarantined and
cannot qualify a phase or reach acceptance quantiles. Independently valid positive
counters, trusted failures and scoped loss receipts remain failure facts. A product
scalar decoder that stops early cannot erase later failure siblings: failure-only
decoding visits product-typed fields independently. Independent gap failure
populations receive the same recursive contract on their product gap structs and
retain the complete identity/membership checks; unrelated generator or diagnostic
omissions cannot erase those failures. Invalid populations cannot invent relative
failures. No product judge, threshold or verdict order changes.

`TestDurationBoundRequiredMembers` starts with a passing pool and replaces both
`resume` and takeover `kill_to_live` with `{}`, `null`, lower-only and upper-only
objects. Each case is inconclusive; independent receipt and counter failures
remain failed. `TestEveryRequiredEvidenceMember` populates optional diagnostic
containers in an otherwise passing pool, asserts that every reachable struct was
visited, and deletes and nulls each of its **411 required fields across 55 struct
types**. Every mutation traverses the complete containing document root through
`DecodeEvidenceLimit`, asserts `IncompleteEvidence`, and checks that the pooled
verdict cannot pass. This includes every containing pointer, slice and map;
validating only the immediate owning struct is insufficient. `TestEveryRequiredManifestMember` covers all three
manifest fields through the real collected-file reader, and
`TestEveryRequiredSummaryMember` covers all 12 summary fields through its decoder.

The optional-field ledger is tested separately by
`TestEveryOptionalEvidenceMember`: all **81 ledger fields in 132 encountered
discriminator contexts** are individually deleted and nulled in a pooled fixture
whose video decoder errors prevent passing, and against a separate passing control.
Required contexts explicitly assert `IncompleteEvidence` on both controls; an
unrelated failure cannot satisfy that assertion. Applicable optional omissions
must decode successfully and preserve the passing verdict. Both sweeps build
controls once and mutate independently owned values. Each mutation validates
from the full document root. Required contexts also check pooled quarantine on
both the passing and failing fixtures. Full-root typed equality may reuse a
control verdict. The test-local verdict cache otherwise reuses a real pooled
judgment only for identical complete decoded documents at the same boundary in
the same fixture, including the main presence-contract and independent gap
recovery-contract outcomes. Delete/null wire variants still traverse both raw
contracts individually. Independent test cases run in parallel. The test covers both
ingress aliases and materializes omitted defaults so deletion changes the wire.
Two typed consistency constraints are asserted separately: takeovers still require
`KillToLive` (planned moves may omit it), and removing `DuplicateWindows` from one
of two matching receipt copies is inconclusive. `TestOptionalMirroredDiagnostics`
deletes or nulls it consistently from both copies and requires a pass. These tests
cover the stated fixtures; they do not claim every possible optional diagnostic
or omitted product flag is independently complete evidence. Historical takeover `MoveResult.Start` is ledger-optional because qualification
uses independent signal timing; planned move starts remain required. Product
omission semantics remain unchanged.

`TrackReport.Video` is optional only for audio. Video tracks require the object;
the raw contract and the typed collection check both enforce that rule.
`TestPooledVideoEvidenceCannotDisappear` starts with decoder errors and removes or
nulls the video object. Neither case passes, and independent scoped losses remain
failed. The synthetic passing control now includes the video object that the real
caller emits.

The struct-member count excludes custom decoder internals. The reachable graph
has exactly **one custom decoder type, `time.Time`**, whose wire value is an RFC3339
JSON string, not a member object. `TestCustomEvidenceDecoderInventory` enumerates
JSON and text decoder interfaces across the input, manifest and summary type
graphs, inspecting map keys, pointer targets and named container types before
descending. `TestCustomEvidenceDecoderInventoryTraversal` inserts synthetic custom
key, slice, array and map decoders to prove that inventory traversal sees them;
`TestCustomEvidenceKeysAndContainers` requires rejection by both the full contract
and independent failure-population validation. The inventory rejects unaccounted
additions. `observe.TestTimeEvidenceScalarContract`
checks omitted, null, empty-object, array, numeric, empty-string and malformed
values, plus a valid timestamp. `EvidenceFields` permits only this reviewed scalar
decoder; unknown custom object or scalar decoders are rejected and tested by
`TestUnreviewedCustomDecoderCannotBypassContract`. No custom decoder is included in
the 55-struct member-traversal coverage claim.

The exhaustive input graph includes these product-owned structures:

- Pool/phase envelopes, registration probes and receipts; `Event` and `EventResult`.
- `Message`, `Stats`, `RuntimeMemory`, `Lateness`, `Pacing` and manifest entries.
- `Report`, `RecordingReport`, `WholeCallDelivery`, `ContentDelivery`, `SentTrack`,
  `TrackReport`, `MediaGap`, `VideoReport`, `OnlineDecodeReport` and `FullDecode`.
- `AnswerFacts`, `AnswerMedia`, `StateChange`, `ConsentReport`, `DialReport` and
  `CandidatePairChecks`.
- `DecryptionFailures`, `SRTPDiagnosticReport` and `SRTPFailureSample`.
- `MoveReport`, `MoveTrackReport`, `VideoRecovery`, `EventMeasurement`,
  `ContentLossEvidence`, `ContentLossWindow`, `ContentLossCounts`, `FreshnessReport`
  and `MetricVerdict`.
- `DistributedRecording`, `DistributedEvent`, `DistributedMeasurement`,
  `DistributedGap`, `DurationInterval`, `FirstBaselines`, `FreshnessBaseline`,
  `OperationIdentity`, `ClockProbe`, `ClockRateInterval`, `OffsetInterval` and
  `TimeInterval`.
- `MoveResult`, `CheckpointState`, `HandoverResult`, `DuplicateWindow` and summary.

## Collection evidence audit

The earlier arithmetic audit did **not** prove baseline population containment.
`TestBaselinePopulationCompleteness` now starts with the passing pool and removes,
truncates or changes possible populations. Each case is inconclusive and cannot
qualify; independent counter and server loss failures still fail. A separate test
checks that incomplete populations cannot fabricate a relative failure by lowering
the baseline bound to zero. Absolute failures and trusted failure facts remain
independent of qualification.

The following structures were checked against the fields actually consumed by the
operator judge and generator exclusions. Product structs still own JSON values.
Strict member checks precede the recursive raw contract; typed collection checks run
before qualification judgment. Invalid phases are not judged as a smaller pool.

| Structure | Required membership and consistency |
| --- | --- |
| Pool and phases | Nonempty phase list, same run and source, no repeated registered sessions or operations; explicit `missing` blocks acceptance. The supplied phase inventory is authoritative; the wire format cannot prove that an external phase was never supplied. |
| Registered sessions and callers | Unique nonempty expected sessions; final caller count and sessions match registration. |
| JSONL process stream | Product reader checks owned PID, valid unique call IDs, expected call count, one terminal `done` and no later messages. Failure quantiles check the same complete call graph separately from generator validity. |
| Optional dial inventory | If supplied, all calls have one matching dial ID, PID, session and connected outcome. An absent diagnostic dial inventory cannot alter media bounds or denominators. |
| Received tracks | One audio and one video kind, no duplicate kind; video tracks require video evidence, while audio may omit it. Supplied arrival count cannot exceed packet count. Packet history arrays are diagnostic, not a substitute for gap populations. |
| Armed identities, missing receipts and moves | Full run/operation/session/worker/epoch identities; unique logical operations; exact armed-to-move inventory. Missing or extra operations prevent qualification. |
| Scheduled events | Positive affected count equals unique session and identity counts; every identity matches one move of the scheduled kind, with no duplicates or leftovers. Incoming `recorded` is derived and recomputed by the product judge. |
| Server and optional caller receipts | Exact full identity, kind and result kind; one receipt per move. Result session/source IDs match the operation identity. Uncertainty, result kind and result IDs must be explicit in raw JSON. Missing members mark the phase incomplete before defaults can hide them. When a caller receipt is present, its times, result and uncertainty also match the server receipt. Optional codes retain the product omission contract; no independent code-count certificate is exported, and codes cannot qualify an operation on their own. |
| Registration and report probes | Required registration prefix is present and identical; report probes have valid clock order and distinct increasing send times. Unexported later probes cannot be counted as extra evidence. |
| Per-event gap inventory | Nonempty, unique audio/video gap kinds match received tracks, including takeovers. Missing kinds cannot remove a track from acceptance. |
| Certain/possible baseline populations | Both nonempty; possible contains the entire certain **multiset**. Missing, short, same-length non-containing and wrong-multiplicity populations cannot reach quantiles. There is no exported original sample-count certificate, so jointly truncating both arrays while preserving containment cannot be detected; the unchanged minimum-sample policy still applies. |
| Gap interval members | Explicit lower and upper members, ordered nonnegative numeric bounds. Missing members cannot become clean zero bounds. Descriptive serialized `baseline_p95` is not trusted; product quantiles use the validated populations. |
| Content metric verdicts | Required `Trusted` and `Failed` members are explicit; missing failed flags cannot become successful verdicts. Reasons are explicitly optional diagnostics and do not affect thresholds. |
| Event mapping and duration map | Mapping and all interval endpoints must exist; all consumed duration keys are present. Takeovers require the kill-to-live bound, and observation horizons cannot exceed hangup. No missing bound becomes zero. |
| First-baseline references and evaluation count | Nonempty distinct reference sets, valid sample/median/p95 relationships, enough evaluations for the exported references, and consistent reference sets across the call. These checks do not reconstruct unavailable classifier cells. |
| Content-loss windows and counts | Lost counts do not exceed sent counts; longest runs do not exceed losses; supplied baseline windows have the event width and do not overlap. The format has no completeness certificate for these optional windows. |
| Generator exclusion bounds | Coverage conservatively includes caller start through hangup, plus observed event extensions and padding. Missing or truncated optional baseline/window lists therefore cannot shorten the required coverage. This intentionally broadens exclusions; no media threshold changed. |
| Generator messages and collected stats | Required scalar members, complete terminal stream, retained-plus-dropped count, and exact collected sample values (including timestamps) match source JSONL; cadence and caller/event coverage are checked. |
| Stats files and manifest | Unique names, exact file membership/count, byte lengths and hashes; missing, extra, reordered or changed samples cannot qualify. |
| Process-to-job mapping | One nonempty unique job per process; mismatched lengths cannot index past the inventory or silently skip a process. |
| Summary and yardstick | Summary required members and numeric counts are checked; yardstick rows and coverage are recomputed from complete event/caller memberships, never trusted from serialized rows. |

`ContentLoss.Baselines` and first-baseline candidate lists have no independent
exported total that proves every original candidate was retained. The audit claims
only the checks above. State histories, decoder diagnostics, causes, reasons,
computed `baseline_p95` summaries and online/offline packet histories do not set
this wrapper's acceptance quantiles; they cannot replace required evidence.
Only the exception ledger determines optionality. Diagnostic fields that remain
required still must be present.

Regression controls were completed with the inventories emitted by the real
caller: armed IDs, both track kinds for each event, and mapping/classification
records. Existing expected passing counts remain 20 moves, 10 takeovers and two
sequential callers. Product judge implementations remain unchanged.
