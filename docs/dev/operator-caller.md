# Operator caller and media evidence

The caller bridge runs unchanged `relais-load`. The session owns the gate order, its
fatal latch and caller identities. Delivery adapters own host access and mailbox
transport. No provider SDK or deployment identity belongs in this code.

## Launcher seam

`internal/operatorcaller.Launcher` exposes:

```go
Ready(ctx context.Context, deadline time.Time) operator.Certainty
Start(ctx context.Context, identity Identity, phase Phase, deadline time.Time) StartResult
Stop(ctx context.Context, identity Identity, deadline time.Time) StopResult
```

Allocate the identity and its deterministic `Unit()` before submission. Use one
`FixedLauncher` throughout that caller's lifecycle. Wrap the delivery `Store` once
with `NewMailbox`, and pass that same mailbox to every broker and to
`NewFixedLauncher(configs, executor, mailbox, fence)`. The constructor copies
`map[Identity]LaunchConfig`; survivor and boundary identities have separate immutable
configurations. Supply the same `Fence` used by the session. The fence exposes only
`Latched()` and `Halt(code)`. Its implementation must respect the session's single
event-loop ownership; an adapter cannot clear it.

`LaunchConfig` binds the bridge, load and configuration paths and SHA-256 hashes,
mailbox directory, existing work root, expected architecture, and `RuntimeDeadline`.
That last value is the validated manifest's absolute deadline. The method's deadline
bounds delivery and waiting; it does not restart or shorten the caller's media
lifetime. Configure both immutable caller bundles before calling the launcher.
Readiness checks every configured bundle. The configuration and mailbox contain
confidential inputs and are never public artifacts.

`ReadyScript` checks executable/configuration hashes, architecture, available CPU
count/model evidence through the bridge host collector, ffmpeg, the service manager
and synchronized chrony state. It starts no caller. `StartScript` creates a private
working directory, copies and checks the three inputs, and submits one `systemd-run`
with `RuntimeMaxSec`, a fixed unit name, `GOMAXPROCS=2`, and a 180-second stop grace.
The configuration identity must match the preallocated command identity. Start results
are `started`, `proved-not-started`, or `unknown`. Another caller ID cannot launch a
second unit for the same gate and phase through that launcher.

The shared mailbox reserves each terminal key before delivery, so broker hangup and
launcher stop cannot resubmit a lost hangup through another API. Stop creates one
terminal `hangup.json` outside numbered feed, submits at most one service stop, then
performs bounded read-only inactive checks. Only exact `LoadState` and `ActiveState`
output proves inactivity. A specifically recognized not-found unit is accepted;
arbitrary command errors and empty output are not. Once the session is latched, only
the terminal hangup and read-only confirmation are attempted. The runtime ceiling
remains in place. An unknown start or stop is never resent. Later inactivity never
clears the earlier uncertainty. Repeated stop calls return the cached result.

The container adapter supplies bounded root execution of these same scripts. The
supplied executor must report transport certainty separately from exit status and
fixed pre-submission refusal evidence. A positive script exit cannot establish that no
effect occurred.

## Bridge and mailbox

```text
relais-gates caller -config CONFIG -load LOAD -mailbox MAILBOX
relais-gates caller -config CONFIG -load LOAD -adapter ADAPTER -spool SPOOL
```

`BridgeConfig` binds `identity`, `bind`, `certificateFile` and `loadSHA256`. The
bridge collects Linux CPU evidence itself. P2's `host` package renders remote
procedures and does not define caller CPU evidence, an execution transport, or the
caller polling clock. `Host`, `Executor`, and `Clock` retain those separate
responsibilities. The launcher additionally passes `-identity` as structured JSON so
stale configuration cannot select another unit's mailbox. The load process receives
the filtered environment and file paths for its bearer token and TLS CA. The child
receives only path, home, temporary-directory, locale and timezone variables from an
explicit allowlist, plus the fixed `GOMAXPROCS=2`.

The directory mailbox publishes complete files atomically with create-only hard links.
Reads distinguish absent objects from read failures. Conflicts prove no content
equality. Only a conductor-owned terminal hangup conflict proves an already-landed
stop.

An executable mailbox adapter serves `mailbox.read` and `mailbox.create` requests
using the existing versioned JSON Lines frames. The frame session is the caller ID.
`MailboxArgs` carries a scoped relative key, inline bytes or a checked blob.
`MailboxValue` carries explicit `found` for reads, or a typed create state. Transport
completion is still required. Large input/output uses the agreed spool with opaque
names, byte lengths and SHA-256 checks. A missing or lost result never becomes absence
or a second create. Adapter arguments are structured; the bridge does not evaluate an
adapter shell command or pass credentials to it.

The bridge publishes setup evidence, then waits for `begin` before spawning the load
process. Pre-dial signal RTTs can reject a far caller without dialing. A dedicated
ten-probe registration burst preserves nanosecond RTTs and the existing 10 ms
threshold. Feed delivery uses the child's literal loopback API. Terminal hangup is
watched independently while probe/feed delivery is pending. Generator abort precedes
evidence publication. All abort routes share one SIGINT/SIGKILL grace timer.

For load, `PrepareCallers` collects and validates boundary setup evidence before
survivor begin. `BeginBoundary` requires successfully collected survivor evidence and
a confirmed inactive survivor unit before boundary dialing. The two-call phase never
enters survival pooling.

## Pinned media evidence

`internal/operatorevidence.ReadPool` joins raw JSONL, product event results and
receipts to immutable registration, runtime source, fault time, clock and inventory
evidence. `Phase.Clocks` takes the merged `clocks.Evidence` from
`Session.FreshClocks`; `Phase.Inventory` takes the `releasecompat.Pool` from
`Session.FreshInventory`. Zero values preserve missing evidence. Those session methods
own `host.Unpack`, `clocks.Parse`/`Check`, and `inventory.Parse`; the pool join does
not repeat their validation. Summary presence uses `observe.Decode`. It does not add
per-phase verdicts to manufacture a sample. Missing sibling fields mark a phase
incomplete while retaining its independent receipts and caller report. Strict decoding
rejects duplicate and noncanonical members recursively in envelopes and typed JSONL;
valid failures on other lines survive rejected lines. Mixed runtime pins require
separate pools and judges.

`BuildPinned` archives an exact local public commit into an isolated directory, adds
the embedded judge entry/helpers and the shared P2 strict decoder, then builds with
offline, readonly module settings. The compiler must report exactly the repository's
pinned `go1.26.9`. A separate build environment allowlist disables workspaces and
persisted Go configuration and fixes proxy/bypass settings. Build before media
windows. The executable hash is checked before each judgment. The pool retains its
required source commit outside the judge JSON; a mismatched pinned judge refuses
before execution. Neither `pkg/callharness` judges nor `relais-load` are changed.
`relocation.json` records source and relocated hashes and the bounded relocation
changes: module imports, comments, formatting, helper-prefix extraction and the
product-owned `controlplane.MoveResult` receipt value. The baseline relocation hashes
remain recorded separately from subsequent input-hardening hashes. Product policy,
thresholds and verdict order are retained.

## Verification scope

`TestOracleCallerManifests` replays 172 frozen manifest-validation projections.
`TestOracleBrokerCallbacks` replays prepare/begin/arm effect order above the recorded
write/wait callbacks in three frozen scenarios. The export does not contain every raw
mailbox-read callback, so these are partial projections. They do not establish whole-
session replay. The coverage inventory retains the pending scopes explicitly.

`TestPinnedReleaseJudgesMatchFrozenEvidence` compares every summary field on unchanged
synthetic judge evidence at both published release commits. The relocated original
tests cover raw identity joins, missing evidence, generator exclusions,
duplicate/replayed events and known failures. The repository-local mutation driver
runs the original-ledger and new caller guard cases in isolated source copies.

PARTIAL — Unit, replay, framed-pipe and pinned-judge checks are executable here. Real
systemd start/begin/hangup/stop/inactive confirmation requires a Linux host and
remains pending container integration. Local tests do not establish provider
transport, independent-host clocks, managed failover or paid runtime behavior.

Numeric input boundaries and each arithmetic site are recorded in
[the numeric evidence audit](operator-numbers.md).

Raw media joins carry a phase `missing` flag when required receipt members are
absent. The pinned judge preserves that flag before typed defaults can hide the
missing data; independent failure evidence remains available.

## Procedure and pool composition

`operatorgates.CallerRuntime` implements P3's broker seam with the real
`operatorcaller.Broker`. It projects registration timing, passes product-owned
operation identities, and decodes receipt results with the product's
`controlplane.MoveResult`. Raw procedure receipts retain omission and timestamp
spelling for stale-history comparisons. The shared optional ledger permits a
historical takeover to omit `start`: qualification uses the independently
observed signal bound in the outer receipt. Planned move receipts still require
`start`. Every receipt validates its original bytes against the recursive field
contract before typed conversion. Missing/null required checkpoint or result
members mark both the delivered receipt uncertain and the retained pool incomplete.
Valid loss facts remain available; zero-filled conversion cannot restore completeness.

Wire `CallerRuntime.Media` to `ProcedurePorts.Media`, `Collect` to
`MeasurePorts.Collect`, and `MarkFault` to the process runtime's mutation hook or
measurement fault hook. Collection has one reservation and rejoins its worker
before returning control for shutdown. It builds the raw event/receipt join and
retains it through `operatorevidence.ReadPool`, including failures when caller
collection or clock evidence is missing. Stopped, incomplete and nonzero completion
return readable raw caller rows alongside the collection error. Only successful
collection authorizes a boundary caller. Measurement deadline and cancellation
exits cancel and join the collection owner before final pooling can snapshot it.
After collection ends, the single report read and the judgment each use a separate
read-only retention context capped at 30 seconds, detached from collection
cancellation. These budgets retain already-published evidence; they do not renew
collection success or permit a boundary after cancellation/the original deadline.
The composed mailbox and judge reject cancelled contexts and check retention
bounds. `TestCancelledCollectionRetainsPublishedDirectoryReport` exercises the
actual directory's cancellation rejection and proves that the broker still
retains the published report. `TestRetentionCannotExtendCollectionDeadline`
checks that readable evidence cannot authorize a late boundary. `EvidenceRuntime.Media` is the final
read-only sequence callback for one source pin. Its judge seam is implemented by
`operatorevidence.PinnedJudge`; build and verify that binary before dialing.
Procedure elapsed deadlines and the immutable manifest both bound feed publication;
an expired deadline cannot consume a sequence or create a mailbox entry.
Caller preparation and stop continue through `PrepareCallers`, `BeginBoundary`
and the fixed launcher's shared identity reservations.

`TestComposedTrialOracle` replays the frozen warmup and six move/takeover trials
through the actual broker, mailbox and bridge delivery handler. Host effects and
the child's HTTP API remain injected offline seams. `TestComposedProcedureCallerPool`
uses the real measurement procedure, broker collection, pool reader and unchanged
judge on the complete synthetic population. It checks 20 moves, 10 takeovers,
one collection per identity, and independent failures with missing clock evidence
or a missing caller report. It also checks deletion/null of required checkpoint
and result members, stopped/incomplete/nonzero completion with positive failure
counters, and pool publication before deadline/cancellation returns. Frozen
partial trial receipts are deliberately projected as uncertain by the composed
adapter; procedure ordering and raw receipt comparisons remain unchanged.
Incomplete uncertain drain receipts retain independently
scoped loss facts without qualifying. This is separate from native child, systemd, pinned
release rebuild and whole-session proof.

The frozen archive is `6e5d5919cd43aff2b89c44f9a2d43aa5608f9bbb25dc40639fae3862274b8c48`.
Fourteen scenarios contain only the exporter correction of numeric `HungUpAt`;
151 are byte-identical. P2's 179 function inputs/results and 13 typed observations
are unchanged; their source fingerprints are refreshed. `TestCorrectedFrozenCallerRows`
checks 22 collected streams and 38 product JSON rows, including 20 restored
181-second call durations. The intentional malformed collection row still fails
decoding. Full measurement telemetry reductions and whole-session replay remain
pending; the coverage inventory records the narrower composed projections.
