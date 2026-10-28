# Operator session core

`relais-gates session -spool DIR` binds an agreed local blob directory without
putting its path in protocol artifacts. `relais-gates version` emits the protocol-v1 hello. `relais-gates session`
accepts one init and runs one session over JSON Lines on stdin/stdout. Stdout
contains protocol frames only. An absent gate implementation is `not-run` with
`not-implemented`; this stage cannot produce rehearsal eligibility evidence.

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
60 are killed by the core and observation packages; 354 remain pending with their ledger owners.
The late-start projection retains pending/readiness/outcome order while replacing
the removed multi-launch policy with one pending start. The 60 ledger edits
(including 12 Python-source edits) and 14 additional mutants are in
`scripts/ci/gate-mutation/cases.json`. Several old dispatch/mailbox guards converge
on one session guard; each original ledger ID is still applied and killed
separately. `go run ./scripts/ci/gate-mutation` copies source, excluding Git,
secret environment files and build outputs, into a temporary tree and uses an isolated cache with offline, read-only
modules. A kill requires the named baseline test to pass and its mutant to fail
at the named assertion. Build errors, missing tests, panics and timeouts are
invalid. New protocol mutants are counted separately from the 414 original IDs.
Other ledger cases are never included in the killed count.

`make test-operator` runs the core and observation tests and owned mutants. It is
included in `make check`. Full process algorithms, caller lifecycle, judges and
container rehearsal remain with their owning stages. See
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

The workflow separates release reads from execution. Only its release-read step
receives `RELAIS_RELEASE_TOKEN`; it invokes `relais-gates rehearse -prepare-only`.
The execution target uses `OPERATOR_REHEARSAL_FLAGS=-prepared`. The token stays
inside the scoped public release reader and never reaches child processes.

The actual allowed CPU mask determines the layout. Two CPUs belong to the
caller unit, its Go bridge and decoder (`GOMAXPROCS=2`); the remaining CPUs serve
the driver and containers. Before gate faults, a separate 60-second caller
measures send lateness, CPU pressure and decoder counters with the topology
idle. The report keeps the peak p99, maximum lateness and time-weighted flagged
intervals. This check does not replace the pinned judges' event-window guards.

The shared session runs rollout, control restart, A→B→A, then relay deploy.
Rehearsal eligibility requires every covered procedure and the pooled media to
pass, at least 20 qualified moves and 10 qualified takeovers, confirmed caller
stops, and scoped cleanup. Only `relayLocalTiming` and
`relayConnectionInvariants` remain explicitly inconclusive. Unknown outcomes,
precondition errors, driver errors and dropped phases fail eligibility. Phase
indices, exclusion codes and saturation overlap counts remain visible even
when the judge cannot produce qualifying media totals.

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

The workflow uploads only `runtime/operator-report/report.json` and
`runtime/operator-report/host-diagnostics.log`. It records the tested commit
(the merge commit for a PR), the separate PR head, template and binary hashes,
release pins, clock samples, CPU masks and phase evidence. Raw mailboxes,
credentials, generated keys and unit environments are not artifacts. Cleanup
runs after partial boot, cancellation and gate failure using deployment-owned
container IDs and caller units. Images remain reusable build cache.

PR checks and one complete execution are not initial profile qualification.
Keep three declared complete executions at the same code/profile/bounds; all
three must meet the strict rule, without discarding failures. This rehearsal
covers a local TLS proxy, local time reference and shared runner clock. It does
not cover managed store failover, load admission, independent host clocks,
arm64 execution or cloud resource lifecycle and billing.

NOT-RUN — The command assembly and report policy have offline tests. Real
Docker execution and free-runner calibration require the workflow artifact.
