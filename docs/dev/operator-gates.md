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
Control status is effectful. Only the fixed store/worker/relay read allowlist is
pure. A confirmed pure-read failure can end a gate without implying mutation.

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

`testdata/coverage.json` inventories all 163 scenarios. P1 checks deadline and
sanitized-category projections (428 assertions in 33 scenarios), plus classified
certainty/fencing seams from three scenarios. Object key ordering is canonical;
request/effect order is preserved. Original nested invocation IDs are joined
before allocating the session's own monotonic request IDs. These are projections,
not complete gate, caller, host-script or media replay. Every full scenario
remains explicitly pending for P2–P6. Live pipe protocol cases have independent
expectations because the original oracle predates this protocol.

`testdata/mutation-coverage.json` accounts for all 414 original mutation IDs:
14 are owned by this core, and 400 remain pending with their ledger owners.
The late-start projection retains pending/readiness/outcome order while replacing
the removed multi-launch policy with one pending start. The 14 ledger edits and thirteen additional protocol/privacy mutants are in
`scripts/ci/gate-mutation/cases.json`. Several old dispatch/mailbox guards converge
on one session guard; each original ledger ID is still applied and killed
separately. `go run ./scripts/ci/gate-mutation` copies the buildable dependency
slice into a temporary tree and uses an isolated cache with offline, read-only
modules. A kill requires the named baseline test to pass and its mutant to fail
at the named assertion. Build errors, missing tests, panics and timeouts are
invalid. New protocol mutants are counted separately from the 414 original IDs.
Other ledger cases are never included in the killed count.

`make test-operator` runs the fast core tests and owned mutants. It is included
in `make check`. Full process algorithms, caller lifecycle, typed product
observations, judges and container rehearsal land in their owning stages.
