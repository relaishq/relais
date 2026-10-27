# Operator observations

These internal packages provide the observation and release-input boundaries
for the operator session. They perform no deployment effects themselves.
Inventory passes the original nested manifest bytes to the existing
`releasecompat.Manifest` decoder. The public release-compatibility decoder is
unchanged, including its newer-manifest refusal.

| Source function or asset | Go owner |
| --- | --- |
| Product JSON reads in `preview-processes`, `preview-release`, `preview-trials` and admission observations | `internal/operatorgates/observe`; values are the original `controlplane`, `mediaworker` and `relay` types |
| Python process, inventory and telemetry assets | `internal/operatorgates/host`; embedded under their original Python module names |
| `clock_evidence` helper | Standalone embedded `host/clock.py` |
| `parseInventory`, `templateBootHash` | `internal/operatorgates/inventory` |
| `freshCompatibilityPool`, `previewClocks` | `operatorgates.Session.FreshInventory` and `FreshClocks` |
| `verifySignalBracket` and clock parsing | `internal/operatorgates/clocks` |
| `planArtifacts`, `verifyStageDigest`, checksum/archive checks | `internal/operatorgates/artifacts` |
| Manifest/policy decoding, semantic versions and compatibility preflight | Existing `internal/releasecompat`, without a second schema implementation |
| `publicReleaseReader` | `artifacts.PublicReader`; explicit optional API token, no environment credential lookup |

The field ledger is `observe/fields.json`. Contract tests check its member names
and JSON tags against the product structs. Required-field metadata is beside the
decoders. Missing required booleans and counters cannot become healthy zero
values. Product nullable slices and pointers remain nullable. Omitted `lost`
decodes as false; true remains a known loss. A loss with no takeover target is
retained. A drain error is retained even when its move list is empty. The control
registration's draining mark owns the placement fence.

Fresh observations use the real session's `Exchange`, `Peer`, monotonic deadline
and clock. Replies carry `operator.HostOutput` in `operator.Reply.Payload`;
transport certainty and exit status remain on the reply. Invalid or unknown
replies latch the session and fence following effects. Confirmed inventory-read
failures retain their fixed refusal category. Inventory and clock actions join
the fixed pure-read allowlist; control status and telemetry remain potentially
effectful observations. Inventory pins bind to the session's initialized
`operator.Scenario.ReleaseA` and `ReleaseB` identities. There is no separate
host-read interface or now callback.
The procedure engine must call fresh checks before effects and owns clock-cache
policy.

The Python inventory collector and process staging renderer retain the existing
arm64 host contract. The release planner supports arm64 and x86_64 archives.
Extending host inventory and staging to another architecture needs its own
reviewed source change and evidence; it must not be claimed from planner support.

## Mechanical relocation

`host/relocation.json` records the original byte hashes for the inventory,
telemetry and original Python tests. Three process-test comments lose execution
provenance; their Python AST hash is unchanged. The process module uses an AST hash after
removing only its credential-read prefix and the new credential helper. The test
also requires its replacement prefix to be exactly `password = control_password()`.
The standalone clock function retains the original function-byte hash.
AST receipts use canonical JSON node kinds, field names and values, omitting
source locations and empty fields. This keeps receipts stable across Python
versions that add empty AST fields or change `ast.dump` formatting. The recorded
hashes were computed from the original sources and matched against the relocated
sources; the original byte hashes remain recorded separately.

The explicit credential change replaces a cloud-secret read in `ownership()`
with the prepared root-owned, root-only `/run/relais/control.env`. It parses the
single `RELAIS_REDIS_PASSWORD` value as data and validates the template's generated
64-character value. It never sources the file, executes its contents, prints the
credential or falls back to another credential source. Missing, unreadable,
malformed, duplicate and wrong-permission credentials refuse before store I/O.
The template prepare step already removes old credentials before a failed refresh.

No archive extraction helper was moved from `cmd/crash-run`. Its extraction
contract accepts more tar layouts than the planner's exact GNU producer grammar.
The planner only reads the manifest and needs no extraction or child executable.

## Frozen replay and mutations

`oracletest/observations.json.gz` contains 179 extracted invocations: 168 inventory
parses, seven independent clock observations and four signal brackets. Inputs,
host results and expected outputs come from the frozen language-neutral corpus.
There are also 13 distinct control/move/drain observations from the original
omitted/false/true `lost` scenario. Each case retains its original logical member hash and invocation ID. The
extractor verifies the source archive, logical archive and every logical member
against its manifest before resolving blobs. It never invokes Go to make an
expected result. Pure clock reads are gathered in logical slot order; their
parallel response arrival order has no effect on the bound calculation.
The test adapter translates the published `os: target-os` alias to the actual
Linux target in both inputs and expected products. Production validation still
requires Linux; other OS values retain their rejection behavior.

Replay commands:

```sh
go test -count=1 ./internal/operatorgates/...
go test -race -count=1 ./internal/operatorgates/... ./internal/releasecompat
go run ./scripts/ci/gate-mutation
```

The mutation driver copies source into a temporary tree, skips `.git` and secret
environment files, and checks a clean baseline. It locates each Go declaration
with the Go parser and requires one exact guard match. It applies the original
12 Python edits to the copied assets. A kill requires the named regression's
assertion failure. Build errors, missing tests, panics and timeouts are invalid.
All cases use `scripts/ci/gate-mutation/cases.json`, including the retained 12
Python-source edits. The specifications retain the original guard fingerprints.
P2 adds 34 retained Go cases, 12 unchanged Python cases and one additional
presence case to P1's cases. The shared inventory records 60 original cases
killed, 14 additional cases killed and 354 original cases still pending.
Native decoder and compatibility cases mutate the reused `releasecompat` guard
in the temporary tree, never a parallel implementation.

`testdata/coverage.json` records the replayed observation projections for 20
scenarios. Only the standalone signal-bracket fixture is marked fully replayed;
outer gate fixtures remain pending. The corresponding mutation inventory is
`testdata/mutation-coverage.json`.

Complete session transcripts, clock-refresh caching, ordered mutation effects,
fresh takeover selection and release-gate procedure replay remain with the
procedure engine. Template bytes in the corpus are digest-only; boot extraction
is checked separately against the public template's actual boot script. Release
archive inputs are digest-only and are not claimed as archive-byte oracle replay.
The planner and reader instead have offline producer-layout, policy, digest,
pagination, reread, body-limit and token-scope regression tests. The source-read
ledger also contains config/library and media/caller reads; their full ownership
audit remains with those modules before cutover.
