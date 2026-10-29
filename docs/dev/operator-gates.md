# Operator session core

`relais-gates session -config FILE [-spool DIR]` accepts one init and runs the
public `RunSequence` over JSON Lines on stdin/stdout. Stdout contains protocol
frames only. The private JSON configuration supplies `parameters` (the existing
process endpoints), two release `pins`, clock `bounds`, `architecture`,
caller `endpoints` and `token`, and an absolute `judgeBinary` path.
Optional `callerIdBase` allocates caller identities after setup callers.
The binary is checked against Init's judge SHA-256 before resource requests;
release pins are bound to Init's scenario. No release build or download occurs
inside a gate. `-spool` binds an agreed local blob directory without putting its
path in artifacts. Configuration and the spool are local inputs, not public
reports. `relais-gates version` emits the unchanged protocol-v1 hello.

The resource-backed runner owns preparation, gate admission, process/release
procedures, caller brokers, the evidence pool and the pinned judge. Host steps
use `host.execute`; all caller and mailbox access uses the same session's
requests, deadlines and latch. Failover's optional store boundary uses
`store.observe` and `store.failover`. Missing required capabilities refuse
preparation before the first resource request with `precondition-failed`;
there is no fabricated operation or unknown effect for that refusal.

Load additionally requires the optional `caller.admission` capability marker.
The free adapter does not offer it or either store capability. This is an
additive protocol-v1 negotiation value, not a new effect request: the existing
caller/mailbox operations carry their already-defined `admission-boundary`
manifest phase. Old peers omit the marker and are refused before effects.
Private configuration must supply `admission.survivorSeconds` and
`admission.boundarySeconds`. The combined media duration plus the existing
seven-minute campaign reserve must fit the 20-minute cap; no deadline is renewed.

`PrepareCallers` proves both setups before survivor begin. The public
`RunAdmission` then collects the survivor, confirms its one reserved stop,
proves control, local workers and store ownership idle, and calls `BeginBoundary`.
The latter requires terminal survivor evidence and the same confirmed stop.
The two-call boundary runs the existing load measurement definition and owns a
separate collection and stop reservation. Its raw JSONL remains in the resource
mailbox and is retained separately in `ResourceResult.BoundaryJSONL`; it never
enters the survivor pool. Known boundary decryption, ICE or loss failures remain
failures even when other evidence is incomplete. Native admission/measurement
verdicts remain conservative and inconclusive; no new passing policy or pinned
judge threshold was introduced.

`TestSessionBinaryOwnsAdmission` exercises both phases through a built command,
missing admission/store capabilities, unknown launch/stop and known boundary
failure against a fake resource server. Its synthetic evidence proves lifecycle
and fencing only, not a paid media pass. `TestResourceStoreFailoverRequests`
pins request-backed observe/failover/observe and unknown fencing.

No frame fields or effect request operations changed. Caller operations have an
explicit payload contract: Args is `{"manifest": <operatorcaller.Manifest>}`.
The public session allocates that immutable manifest. The resource server
configures its fixed launcher from it. A zero-exit readiness reply carries
`{"ready":true}`; start carries `{"start":"started"}`; stop carries
`{"inactive":true}`. Only a proved `notSubmitted` refusal can establish that
a start was not submitted. Transport `unknown` dominates every payload and
cannot supply operation classifications, including a refusal. Positive product
failures from confirmed transport remain separate from refusal classifications.

Mailbox Args and payloads reuse `operatorcaller.MailboxArgs` and
`MailboxValue`: read requires explicit `found`; create returns `committed`
or `conflict`. Unknown reads never become absence. Writes remain create-only.
Large read values use the existing reply blob envelope and agreed spool.
The bounded decoded value may contain up to `MaxBlobBytes` of JSON, including
base64 data; it is not restricted to a small host observation.

Sequence sessions stream traces as trace frames. Final's `trace` contains only
new terminal entries published after streaming (normally it is empty), avoiding
a duplicate trace that could exceed the unchanged 256 KiB frame limit. Clients
append that terminal tail to the streamed trace.
The session request bound is 32,768: half-second caller polling can exceed the
old 4,096 bound within a real campaign. Pending timeouts still have deterministic
request order. Concurrent caller collection and host requests serialize through
one exchange owner and share the session's locked fatal state. Admission to that
owner is cancellable: queue time consumes the supplied context and absolute
request budget, and an expired waiter never submits a request.

`testdata/resource-session.json` is a language-neutral negative boundary
fixture. Its inventories reuse frozen observation values with the documented
target-OS translation. `TestSessionBinaryUsesResourceSequence` builds the real
command and checks requests through caller start, unknown/no-retry behavior,
definite refusal and missing-capability refusal against a fake resource server.
It does not claim a completed media campaign.

The engine owns request identities, deadlines and the fatal latch. An adapter
serves requests; it does not choose a gate verdict or repair an unknown effect.
The public operations are host execution, caller readiness/start/stop, mailbox
read/create and optional store observe/failover. Arguments and payloads are
operation-specific JSON. No generic shell-command adapter is provided.

Request IDs and Unix milliseconds are decimal strings. Init supplies the
original armed time and absolute end, at most 105 minutes apart. The engine maps
remaining time to its elapsed clock once. Gate windows are at most 20 minutes;
mutation windows end at minute 103 and final pooling ends by minute 105. Wall
clock changes do not extend these bounds. Each request carries its absolute end
and remaining milliseconds. The adapter must clamp its wait to both. Each active gate fixes its cutoff once;
every request and wait is clamped to that cutoff. Adapter execution must call
`operatortransport.RequestContext` once at admission and retain the returned
context through execution and polling. Expired queued requests receive a proved
`deadline-before-submission` refusal from the pipe adapter boundary.

Each effect is submitted once. Its ID is recorded before writing. A timeout
retains its pending identity. Unknown completion latches before returning to
gate code; late success cannot erase it. Duplicate/mismatched replies are
protocol errors. Fatal-event publication and actual request writes share one
admission lock; a queued fatal event fences submission before the engine reads
it. Duplicate inbound requests are never delivered to the adapter. Already queued
requests must also pass fatal-state admission before delivery. `Peer.Finish`
serializes the final builder and write with fault publication, propagates any
fault into the session latch and rejects an unlatched or passing final artifact. Writer admission rechecks cancellation and deadlines after encoding. No further request can pass the latch, including restore,
caller start/stop and mailbox writes. Resource teardown belongs to its separate
owner. Transport completion and operation evidence remain separate: exit zero
does not prove a valid receipt, and a nonzero mutating exit can still be unknown.
Control status is effectful. The fixed store/worker/relay, inventory and clock
read allowlist is pure. A confirmed pure-read failure can end a gate without
implying mutation. Inventory and clock host replies put `operator.HostOutput`
(`{"stdout":"..."}`) in `Reply.Payload`; certainty and exit status stay on the
reply. Session observation methods validate and decode stdout before accepting
the result. Unknown or malformed replies fence later effects through the same latch.

Control frames are limited to 256 KiB, scripts to 64 KiB, and host output to
23,999 bytes at the host decoder. Larger payloads use a bounded blob reference
with an opaque name, exact size and SHA-256. `operatortransport.ReadBlob` verifies
an agreed local spool root before returning bytes. `Exchange` validates and
resolves every blob before calling an operation classifier. Inline payload and
blob evidence are mutually exclusive. Spool paths and payloads do
not enter public traces. Subprocess adapters use absolute executable paths and
structured argv, with a minimal environment and discarded stderr.

Public categories use an explicit allowlist of complete fixed strings. Resource
names, arbitrary class names and arbitrary category suffixes become
`unclassified`. Initialization requires a lowercase SHA-256 scenario digest;
final artifacts use only that stored identity. This is an intentional privacy
tightening of the original permissive category syntax. Frozen oracle categories
retain their original results. Timeout traces have a deterministic request order.

## Frozen replay coverage

`internal/operatorgates/testdata/transcripts` contains the exact frozen gzip
archive and checksum manifest. Loading verifies compressed, decompressed and
logical member hashes and resolves nested content-addressed blobs. Expected
results are never generated from Go.

`testdata/coverage.json` inventories all 165 scenarios. P1 checks deadline and
sanitized-category projections (430 assertions in 34 scenarios), plus classified
certainty/fencing seams from three scenarios. Object key ordering is canonical;
request/effect order is preserved. Original nested invocation IDs are joined
before allocating the session's own monotonic request IDs. These are projections,
not complete gate, caller, host-script or media replay. P2 adds 179 inventory/clock
function replays and 13 typed observation projections across 20 scenarios. Only
the standalone signal-bracket scenario is fully replayed; other full scenarios
remain pending. Live pipe protocol cases have independent
expectations because the original oracle predates this protocol.

`testdata/mutation-coverage.json` accounts for all 414 original mutation IDs:
160 public mutants are owned and killed, 41 are retired with explicit successor
contracts, and 213 remain assigned to private owners. The 138 additional native
mutants are counted separately. These are accounting categories, not claims that
private code was tested.
The late-start projection retains pending/readiness/outcome order while replacing
the removed multi-launch policy with one pending start. The public mutation edits and additional mutants are in
`scripts/ci/gate-mutation/cases.json`. Four additional resource-session mutants cover retry on unknown, unknown as
failure, fabricated capability and rehearsal bypass. Three more mutants cover uncertain transport classification, refusal promotion with an unknown exit status, and queued exchange deadlines. Several old dispatch/mailbox guards converge
on one session guard; each original ledger ID is still applied and killed
separately. `go run ./scripts/ci/gate-mutation` copies source, excluding Git,
secret environment files and build outputs, into a temporary tree and uses an isolated cache with offline, read-only
modules. A kill requires the named baseline test to pass and its mutant to fail
at the named assertion. Build errors, missing tests, panics and timeouts are
invalid. New protocol mutants are counted separately from the 414 original IDs.
Other ledger cases are never included in the killed count.

`make test-operator` runs operator packages, process/caller/evidence composition
tests, both pinned-release comparisons and owned mutants. It is included in
`make check`. Real container rehearsal runs only in its GitHub workflow. See
[operator observations](operator-observations.md) for the typed boundaries and relocation proof.

## Public four-gate rehearsal

The command calls `operatorgates.CheckReadiness()` before release reads or host
effects. Incomplete replay or mutation accounting refuses with the specific
`operator-readiness` reason. This accounting check does not establish rehearsal
or calibration success. `TestRehearseRefusesIncompleteReadiness` pins the refusal.

On a disposable Linux runner with systemd, Docker, ffmpeg, chrony and at least
four allowed logical CPUs, build with `make operator-rehearsal-build`, then run
`make operator-rehearsal OPERATOR_REHEARSAL_RUNNER='sudo -n'`. The command uses
v0.3.0 and v0.3.1 linux_amd64 archives with fixed SHA-256 pins. It reads the
current checkout's template, stages both releases before media, builds both
release-specific judges offline, and uses the unchanged v0.3.0 caller binary
from its verified archive. Cache the release commits' Go modules before running.

Release staging copies each archive to `/opt/relais/relais-stage-<version>.tar.gz`
and passes that exact `file://` path to the template downloader. It verifies
SHA-256 before extraction, then writes the receipt under
`/opt/relais/preview/<version>`. Both upgrade and rollback consume these staged
directories. Container `/tmp` mounts can differ between archive-copy and running
process views. Staging failures name the runner source, copy destination and
downloader source paths.

The workflow separates release reads from execution. Only its release-read step
receives `RELAIS_RELEASE_TOKEN`; it invokes `relais-gates rehearse -prepare-only`.
The execution target uses `OPERATOR_REHEARSAL_FLAGS=-prepared`. The token stays
inside the scoped public release reader and never reaches child processes.

The actual allowed CPU mask determines the layout. Two CPUs belong to the
caller unit, its Go bridge and decoder (`GOMAXPROCS=2`); the remaining CPUs serve
the driver and containers. Before gate faults, a separate 180-second caller
measures send lateness, CPU pressure and decoder counters with the topology
idle. The report keeps the peak p99, maximum lateness and time-weighted flagged
intervals. This check does not replace the pinned judges' event-window guards.
Rejections retain named `causes` and numeric `checks` (rule, observed value and
limit) in the report and `calibration-rejected` log, including partial evidence.
The percentage is computed on every exit from the accepted sample prefix.
Calibration invokes the same hash-verified judge binary as the gates, built
against the pinned release's product types. Its generator entry calls the same
`applySaturation` path: collected sample checks, cadence and cumulative clock
validation, `callerWindow`, `sampleSpan`, interval flags and saturation budget.
There is no separate minimum packet/sample count, media duration or GOMAXPROCS
rule in calibration. The launcher still enforces the declared two-CPU profile.
The first and final intervals may be partial for cadence validation. All
intervals, including the first, are flagged above 1.025 s. A final zero-packet
row is allowed and still participates in timestamp and span validation. Flagged
intervals are clipped to first-video-send through hangup and merged before the
1% budget is computed; time before media and after hangup contributes nothing.
Intervals above 85% CPU per GOMAXPROCS are flagged. The TypeScript runner retains every
stats row; its early-abort window does not replace the final judge's policy.
The A/B pinned-build tests compare calibration with the ordinary generator path
for phased-first/empty-final evidence and rejected CPU/budget controls.

Generator event coverage follows each measurement's baseline, recovery and
freshness horizons, with one sampler period of end padding. It does not require
stats during pre-media registration or an extra period after hangup. Incomplete
measurements still use their conservative observation horizon. The separate
media-span check requires stats through hangup, including the final partial row.
`TestGeneratorCoverageUsesEventHorizon` checks the normal pre-media delay, final
partial interval, uncovered event edges and incomplete-measurement fallback.

The shared session runs rollout, control restart, A→B→A, then relay deploy.
After setup, controls and calibration, the container adapter becomes a resource
server on JSON pipes. It serves the same `ServeSequence` and `ResourceRunner`
used by the session command. The rehearsal no longer supplies its own gate
sequence or direct caller/mailbox procedure ports. This uses in-process pipes
to retain the existing typed report and evidence sinks without adding private
report frames to the protocol. `TestRehearsalUsesSessionResourceRequests`
requires actual resource requests through this entry; bypassing it is a killed
mutant. The unchanged GitHub workflow's prepared execution invokes this path,
including the real container, systemd caller, create-only mailbox, blob spool and
both pinned-release preparation steps. The container run itself remains
workflow-only verification.

Owner decision, 2026-10-28: the free rehearsal proves that the operator procedures
complete and that the media evidence contains no known failure. A shared runner
cannot provide the scheduling isolation needed to settle every media timing
measurement. Media timing is judged strictly only in the paid run. The pinned
judge, its thresholds and the paid-run pass rule remain unchanged.

A free rehearsal passes only when every covered procedure completed, every
caller stopped, calibration passed and cleanup was verified. Any unknown command
result, session latch, precondition failure, driver error or interrupt blocks it.
The judge must report zero failed metrics, lost calls, decryption failures, ICE
restarts, excluded events and dropped or invalid phases. All four phases must be
included, with complete caller and generator evidence. Duplicate packets still
block, and the two existing relay exemptions, `relayLocalTiming` and
`relayConnectionInvariants`, apply exactly as before.

The strict `pooledMedia.status`, qualified move/takeover counts and timing
verdicts remain in the report. They are not rewritten to a media pass. Only
inconclusive `first_content`, `first_new_content` and `distributed_window` metrics
are non-blocking for the free rehearsal. Every row appears in `timingUndecided`
with its gate, zero-based phase and event indexes, metric and sanitized cause
codes. The list must account for every inconclusive metric; a missing row,
unknown metric or missing cause blocks eligibility. `procedureCompleted` records
procedure completion separately from the strict media result. The judge's
`completeMedia`, `lostCalls`, phase diagnostics and `undecidedMetrics` retain the
evidence used by this decision. Timing uncertainty may reduce the strict
qualified counts below 20 moves or 10 takeovers without blocking the free run;
the paid judge still enforces those counts.

`TestRehearsalAllowsUndecidedTiming`, `TestRehearsalTimingDoesNotHideFailures` and
`TestRehearsalReportsEveryUndecidedTiming` pin this distinction. Mutants that
accept failed metrics or dropped phases, or reject allowed timing uncertainty,
must be killed. `TestPaidMediaTimingRemainsStrict` and
`TestPaidSummaryRejectsUndecidedTiming` retain the strict paid rule.

Relay deploy contributes one completed caller and zero events or trial metrics.
Its call must complete with valid caller and generator evidence and no known
media failures. The pool records `relayCalls: 1` separately and applies the
unchanged trial judge only to the other phases. Other gates without trial
collections remain excluded. `TestRelayCompletedCallContributesNoTrials` and
`TestRelayPoolKeepsTrialPopulationUnchanged` pin these populations at both release
commits via `TestPinnedReleaseJudgesMatchFrozenEvidence`.
`TestFrozenRelayProcedureHasNoTrialReceipts` checks the frozen relay procedure
fixtures; their media callbacks are canned summaries, so the raw zero-event
caller case uses synthetic product reports.

The scenario digest is created after binary preparation and before container
boot. Reports without a valid digest are refused; an earlier preparation failure
retains diagnostics without emitting an unidentified report. The two fixed
release pins have identical Go sources, module locks and embedded media; only
release compatibility JSON differs. Their deterministic judge binaries therefore
share a hash. Source commit checks still distinguish the pins.

Negative controls log their input paths, expected refusal, unit state and exit
statuses, without file contents. A `Type=simple` start job can succeed before
credential setup fails; qualification requires `ExecMainStatus=243`, a failed
unit and no process. `TestCredentialControlObservesAsynchronousRefusal` and
`TestCredentialControlRejectsOtherOutcomesAndRestores` pin this distinction.
Every changed file is restored byte-for-byte, affected units must run again,
and the restored clock must pass the same source check. The ACL control restores
exact template rules and repeats its positive authenticated probe even after a
failed negative observation. All hosts are verified again before gates start.

The workflow uploads `runtime/operator-report/report.json`,
`runtime/operator-report/host-diagnostics.log` and
`runtime/operator-report/media-evidence.json`. It records the tested commit
(the merge commit for a PR), the separate PR head, template and binary hashes,
release pins, clock samples, CPU masks and phase evidence. Raw mailboxes,
credentials, generated keys and unit environments are not artifacts. Cleanup
runs after partial boot, cancellation and gate failure using deployment-owned
container IDs and caller units. Images remain reusable build cache.
The media artifact retains the judge pool's per-phase caller JSONL, events,
receipts and registration probes, plus the release commit and judge binary hash.
It replaces opaque identities/text with consistent aliases and shifts all
nonzero timestamps by one common offset to a synthetic epoch. Missing, null and
duplicate members and exact known-field number tokens remain intact; unknown
extension values are scrubbed. Replay tests compare every summary field before
and after sanitization for passing, failing and invalid evidence, including
every timing cause family. Recognized product timing reasons retain only fixed,
privacy-safe classifier prefixes. Dynamic text is removed; identities and
unrecognized reasons stay hashed. This preserves `undecidedMetrics.causes`
without changing judge policy or the event coverage window.
To rejudge offline, extract the artifact's `pool` object as the judge input.
Build the pinned judge with `operatorevidence.BuildPinned` from `sourceCommit`
and this checkout's embedded judge sources, verify `judgeSha256`, then invoke
that binary with the input file. Neither caller manifests nor credentials are
needed. Evidence is saved before judgment, including when the judge fails.
Every invalid phase reports a sanitized `issue.path` and fixed `issue.reason`
and prints them in a `media-phase` diagnostic. Each phase qualifies independently;
an invalid sibling keeps the whole pool incomplete but does not suppress other
phases or erase known failures.
Caller failures capture unit state and the last 60 journal lines before cleanup,
with scoped secrets redacted. Unit state includes user, filesystem protection,
working directory, affinity and runtime limit. Loader stderr and file failure
paths remain visible without credential contents.
The launcher waits for executable startup with `Type=exec`; the existing mailbox
handshake still proves caller readiness. Signaling probe failures retain TLS or
HTTP failure codes, and missing completion evidence retains the underlying error.
The preliminary signaling burst remains optional; the required registration
burst still supplies the final caller clock and proximity proof.
Calibration and gate callers share the same start function and P4 launcher.
After calibration stops, fresh inventories and product status must prove zero
control calls and reservations, plus two live, non-draining, locally empty
workers, before the first gate. Precondition diagnostics identify the host,
observation and fixed failure code; reports retain those categories. A caller
proved never submitted needs no stop, while unknown starts still require proof.
Each negative-control restore requires that same health proof before the next
control. The missing-unit restore uses P3's `GatePrecondition` repair to prove
empty, stop, replace and start the worker, clearing control's draining mark.
After a confirmed stop, repair waits for control's settled-death proof even if
its last heartbeat still says alive. Confirmed repair steps are logged before
the final running-process inventory and full health check.
Sanitized control-status lines record the flags and counts before repair, after
every restore and after calibration. The boot-time ACL control's full deployment
check runs once all hosts exist; no store management credential is retained.
Calibration reserves two minutes for preparation: the unchanged loader supplies
one initial clock probe and one every ten seconds, so ten-probe registration
needs at least ninety seconds after loader startup. The media calibration runs
for 180 seconds so a 1.25-second phased first interval stays below the judge's
1% budget, within a six-minute caller runtime limit.
Caller preparation, including the ten-probe registration, precedes each fresh
20-minute gate window. Campaign admission uses one elapsed-clock snapshot so a
1,200-second minimum fits a full window exactly. Before launch, the caller's
separate 20-minute manifest must fit media plus 120 seconds; both windows stay
clamped to the original session cutoff, with mutations ending by minute 103.
Their UDP bind uses the runner's route source for the relay, with port zero;
the loader rejects wildcard bind addresses. The bridge checks the staged loader
hash, executable bit, certificate readability and ffmpeg inside the caller unit.
Unknown outcomes emit structured `command-result-unknown` diagnostics and remain
in the report's `uncertainties` array. Each includes the operation kind, request
ID, logical target or caller ID, action, deadline and remaining milliseconds,
and the fixed certainty rule. Payloads, raw mailbox keys and error strings are
excluded. Diagnostics never clear the fatal latch or authorize a retry. Caller
journals are collected on unknown outcomes even when the unit remains active.

PR checks and one complete execution are not initial profile qualification.
Keep three declared complete executions at the same code/profile/bounds; all
three must meet the strict rule, without discarding failures. This rehearsal
covers a local TLS proxy, local time reference and shared runner clock. It does
not cover managed store failover, load admission, independent host clocks,
arm64 execution or cloud resource lifecycle and billing.

NOT-RUN — The command assembly and report policy have offline tests. Real
Docker execution and free-runner calibration require the workflow artifact.

The shared numeric contract accepts exactly `-1` for
`callharness.MoveReport.From` and `.To`: distributed/external callers cannot map
worker names to local indexes. These are value exceptions in
`EvidenceNumericSentinels`, with release-source locations, rather than optional
fields. Other negative values and out-of-range indexes remain invalid.
`TestDistributedMoveUnknownWorkerIndex` checks both boundaries;
`TestDistributedCallerWorkerSentinelsQualify` checks the complete judge path.
