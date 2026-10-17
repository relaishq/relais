# Build your agent

Build a custom Go worker with `pkg/workerapp` and `pkg/agent/v2`. The keyless
[delayed-loopback example](../examples/delayed-loopback) records encoded Opus,
waits 300 ms in a local task, and plays the recording back. It needs no API key,
VAD (voice activity detector), codec installation, or C toolchain.

## Build and check the example

From the repository root, with the Go version in `go.mod`:

```sh
CGO_ENABLED=0 make build
./bin/delayed-loopback -version
./bin/delayed-loopback -h
CGO_ENABLED=0 go test ./examples/delayed-loopback/...
```

The stable build target is `./examples/delayed-loopback`; the resulting worker is
`bin/delayed-loopback`. The shared workerapp entrypoint makes `-version` print
`relais-worker version=…`. Its default agent is `delayed-loopback/v1`. It accepts
the stock worker's flags, including `-agent`, `-name`, `-media`, `-http`,
`-control`, `-relay-leg`, `-relay-media`, `-redis`, `-redis-prefix`,
`-frame-cache-off`, and `-drain-timeout`. Use this binary in place of
`bin/relais-worker` in an existing private topology; keep that topology's
addresses, session-store key, outage budget, and registration settings.
See [deployment](deploy.md) for the topology and security boundary.

The example stays in the main module so `make build`, vet, lint, and the unit
suite include it. Its import test checks every example package's direct imports
and copies the production sources to an external module for a `CGO_ENABLED=0`
build. Go then enforces the public import boundary. Relais's public SDK has
internal implementation dependencies; applications must not import those
packages directly. No dependency or tool is installed by the example.

## Hear it in your browser

Start an owned three-worker demo on a separate loopback port:

```sh
./bin/relais-demo -http=127.0.0.1:19101 -worker-bin=bin/delayed-loopback -agent=delayed-loopback/v1
```

Open the launch URL printed by this process. Select **Microphone and test pattern**, use headphones, allow microphone access,
start the call, and speak for about two seconds. Then listen for delayed playback. The recording ends after two seconds or 64 KiB of encoded Opus. The
local task waits 300 ms before returning the recorded packets. Input during
processing/playback is consumed without recording. The next recording starts
when that playback finishes. Move, Drain, and Kill use the same worker binary
and registration, including replacement workers.

The page accepts the custom name, but its echo audio gap/loss/freshness checks
and demo tone/count continuity verdict do not establish custom application
continuity. Custom events stay **inconclusive** with that explanation; raw media
measurements remain in downloaded results. Listen and inspect private agent
status for application behavior. A kill loses move-only audio; an exact Move
or Drain preserves accepted buffered playback. The scripted API smoke verifies
returned encoded audio, decoded video, Move/Drain/Kill, and process cleanup.
It does not prove browser microphone capture or that a person heard playback.

Run it in the local topology with the same overrides (use a private state
folder and separate ports):

```sh
PORT_BASE=35600 deploy/local/start.sh -bin bin -worker-bin bin/delayed-loopback -agent delayed-loopback/v1 -state /tmp/relais-custom-local -redis-port 19380
deploy/local/stop.sh -state /tmp/relais-custom-local
```

`start.sh` also accepts `WORKER_BIN` and `AGENT`. It saves the resolved worker
path and agent for `-restart-worker`. All drivers require an explicit `-agent` whenever `-worker-bin` is set,
including when the path names the stock worker. `echo` and `demo` retain stock
judging; every other agent name selects custom semantics. With no override they retain the stock
worker behavior. A worker path is relative to the directory where you run the
driver, not its per-trial log directory. The local topology needs `ps` to verify
process identity; a sandbox that denies `ps` cannot complete this smoke.

Custom agents support the plain crash-run worker takeover and drain paths.
`-buffer`, `-buffer-lossy`, `-compare-buffer`, `-relay-restart`, `-relay-standby`,
and `-scenario` require echo audio semantics and reject custom agents before
starting any process. Their stock rules remain unchanged. Store-stall modes
require the named example workload; unknown custom registrations are refused
with an explanation.

## Register the factory and tasks

The [example main](../examples/delayed-loopback/main.go) calls
`workerapp.Main` with `Options{Agents, DefaultAgent}`. Each registration provides:

- A stable, versioned `Name` used in snapshots.
- A `Factory` that creates independent per-session agents. `FactoryArgs` supplies
  the session ID, agent name, and effective admitted `MaxStateBytes`.
- Versioned task kinds mapped to handlers. The loopback registers
  `delayed-loopback/play/v1` as a local `ReadOnly` handler.
- Optional `agent.Limits`. Zero fields request host defaults. Declared limits
  remain part of the registration identity; host-resolved values do not.

The public registration in
[loopback/agent.go](../examples/delayed-loopback/loopback/agent.go) is also used by
the call harness. Startup, readiness, private HTTP, heartbeats, metrics, and
bounded shutdown remain workerapp's responsibility. Every worker in a pool must
register the same agent names, task kinds, declarations, and compatible code.
Selecting `echo` for new calls does not replace a restored custom registration.

## Save, Restore, and stable IDs

`Handle`, `Save`, `Restore`, and move callbacks have deadlines and run serially.
Keep them short, honor cancellation, and never mutate app state after return.
Ignore unknown events. `Control` is reserved in this slice.

`Save` returns versioned crash-recoverable `State.Bytes` and O(1) presence/length
metadata for move-only data. Ordinary Save must never serialize, copy, or hash a
recording. `Restore` rejects unknown versions with `ErrVersion` and validates its
schema. It must also support rollback after refused callback effects. The host
checkpoints changed app state; diagnostics must not cause extra saves.

`SaveMoveOnly` runs at a paused exact export. It is non-destructive and repeatable
because a failed move can return the source to service. `RestoreMoveOnly` restores
the exact bytes on a planned target. The loopback uses a version byte followed by
big-endian packet lengths and encoded payloads. Its ordinary state holds the
turn, phase, chunk cursor, packet and refusal counts, and a bounded sparse input window.
State version 2 stores only app-owned diagnostic keys and reads the previous
version 1 state for same-registration upgrades.
That window accepts previously unseen indexes within its range, including gaps
and RTP sequence wrap, while rejecting recorded duplicates.

Use the event's stable `EventID` and a stable task `Step`. The host derives task
IDs from session, registration, cause, and step. Use `TaskRun.Key()` as the
external idempotency key. Do not generate a new random key on retry. Task result
sequences must identify the same content across attempts; `TaskRun.After()` is
the safe replay floor. The loopback partitions the same request in the same order
and skips sequences at or below that floor.

## Recording, tasks, and audio references

The recording ends at its 64 KiB encoded-byte bound or a store-clock timer two
seconds after the first packet. It needs no further input to finish. Its sparse
window retains 128 extended indexes for the negotiated audio source. It records
only during the recording phase. Input during processing and playout is consumed
without recording, and its indexes still enter the seen window.

At the boundary the app replaces its recording state with a bounded `ReadOnly`
task request. Before its first accepted chunk, the handler waits 300 ms and
computes SHA-256 over the versioned recording, then emits encoded chunks with
stable sequences. Normal 20 ms input
produces five packets per 100 ms chunk. Other packet durations keep their packet
boundaries; chunks reach at least 100 ms and the final chunk may be shorter.
A retry with accepted chunks skips that initial wait and resumes above `After()`.
The host validates output framing, duration, MTU, and budgets. This example does
not decode, encode, or resample audio.

In the current `TaskMessage` callback, return `AudioRef` values naming that
result's task ID, sequence, and packet offset. Never put audio in durable app
bytes or save references to old results for later use. The host owns immutable
payloads, moves the full accepted reply, and paces packets independently of
caller input. Emit can wait for memory and 100 ms publication spacing; it runs
outside the callback budget and does not write Redis itself.

Tag tasks and audio with a monotonically increasing `Turn`. On cancellation,
use `CancelBefore` and `ClearAudioBefore` to cancel older tasks and remove their
unsent audio. Late results cannot revive a canceled turn. `PlaybackFinished`
means local sending finished; it does not prove the caller heard the reply.

## Read-only and side-effecting work

`ReadOnly` means replaying the handler is safe for external state. Its request
and results are move-only. It may launch without a forced intent checkpoint.
This trust declaration does not make a provider request free or exactly once.

`SideEffecting` work launches only after a successful checkpoint contains its
intent. Its request is crash-persistent within the host metadata budget.
`Retry` permits another delivery with the same task key after move or takeover.
`Abandon` returns `OutcomeUnknown` when an earlier attempt may have started;
it does not repeat an uncertain effect. Set a bounded timeout. The host preserves
the absolute deadline on restore, so retry cannot extend it indefinitely.

The separate [local ledger](../examples/delayed-loopback/ledger/ledger.go)
demonstrates `SideEffecting` plus `Retry`. Set each ledger worker's
`DELAYED_LOOPBACK_LEDGER_URL` to the ledger server's printed HTTP origin and
select `-agent=local-ledger/v1`. Inspect its server options with:

```sh
./bin/delayed-loopback ledger -h
```

Run its `ledger` mode as a separate process on a literal loopback address, with
`-file` naming its private JSON-lines file. Keep exactly one ledger server per
file. This separate registration commits one demonstration entry per call. It uses no provider and accepts only loopback HTTP.
The loopback audio agent does not require the ledger.

The ledger syncs a first commit before acknowledging it and returns the same
receipt for the same key and exact request bytes. JSON carries those bytes as
base64, including non-UTF-8 requests. New ledger records use version 2; the
reader also accepts version 1 text records. Upgrade the ledger server and workers
together: the base64 HTTP value differs from the original text wire format.
Delivery can occur more than once; commit occurs once per logical key. A different value for that key is a conflict. The
file survives worker and ledger restarts. After a lost reply, server uncertainty,
or an unverifiable receipt, the handler makes one same-key retry with identical
request bytes within the remaining 10-second task budget. Cancellation stops the
retry. If both deliveries remain uncertain, it returns `OutcomeUnknown`; that
does not prove that no commit occurred. Conflicts and definitive failures are
not retried. A write error makes the running ledger
refuse further writes; a malformed file is refused on reopen. Inspect such a
file before recovery. This small example is bounded at 4,096 entries and is not a
production database or an atomic transaction between Relais and a provider.

## Crash loss and store outages

Exact moves preserve the recording, ReadOnly request, accepted reply, and playback
cursor. An urgent move cancels unfinished handler attempts; the logical task
continues according to its class and resume policy. Buffered audio is preserved.

A crash loses move-only recording/request/audio. The host delivers
`Resumed.MoveOnly == OutcomeUnknown`. The example immediately clears its lost
length, clears unsent refs, cancels the old turn, and enters a new recording turn.
It starts no task from `Resumed`; new work follows on a later input or timer.
Developers must clear lost move-only data when the turn ends this way. If an app
keeps claiming a lost length in its Resumed save, a smaller target's
`-agent-move-bytes-per-session` causes `ErrCapacity`. A crash restore must reserve
nothing for content it discarded.

The default **1 KiB app state does not preserve normal conversation history
across a crash**. Earlier conversation can be lost as well as the current turn.
New turns and retried provider work can incur new charges. There is no general
single-render or exactly-once provider execution promise.

During a store outage, bounded local ReadOnly work and playout may continue.
SideEffecting launches wait for an acknowledged checkpoint containing their
intent, then recheck the deadline, fence, and cancellation. Failed writes neither
debit successful-write byte tokens nor produce Capacity. Full local mailboxes
backpressure Emit with cancellation. Recovery checkpoints current state; a crash
can still lose local data. See [ADR 0008](adr/0008-store-outage-budget.md).

## Budgets and the capacity approval gate

The default v2 raw app budget is 1,024 bytes. The serialized host metadata allowance
is 2,048 bytes, including durable side-effect requests, task cursors, and reference
metadata. The default active task count is four. Move memory has a separate
budget and includes recording, ReadOnly requests, reply payloads, and backlog.
Size reservations from real payloads, not fixed estimates.

These worker-only flags set operator ceilings:

| Flag | Default | Meaning |
| --- | ---: | --- |
| `-agent-max-snapshot-bytes` | 7168 | Whole encrypted snapshot size |
| `-agent-snapshot-bytes-per-second` | 81920 | Successful snapshot payload bytes per session-second |
| `-agent-snapshot-burst-bytes` | 28672 | Snapshot byte burst per session |
| `-agent-move-bytes-per-session` | 1048576 | Move memory per session |
| `-agent-move-bytes-total` | 67108864 | Worker move memory, including export scratch |

Registrations cannot raise those ceilings. Oversized declarations fail startup;
actual content is checked again at runtime. Admission reports typed `Capacity`;
an unavoidable transport snapshot overflow can refuse adoption or end the call
with `ErrCapacity`. A store failure is a different outcome. The exact export and
resume request must also fit the private HTTP wire bound, including base64.

The custom-worker capacity/Save-CPU gate is **manual only** in
`.github/workflows/load-calibration.yml`, mode `custom-capacity`, on
`ubuntu-latest`. It never runs on schedule and is not a release gate. From
GitHub Actions, select **Load generator calibration**, the exact pushed ref,
**Run workflow**, and mode **custom-capacity**. Set `duration`, `capacity_max`,
`callers_per_process`, `save_cpu_us`, and `save_wall_us` for the proposed operating
load. Defaults are 30 s/step, at most 16 callers, two callers/generator, and
1,000 microseconds of Save CPU and wall time per Handle event. Those Save limits
are explicit operator inputs, not new SDK guarantees. The orchestrator dispatches
the workflow after pushing; this guide's author did not dispatch a workflow.

The workload uses continuous harness Opus input and VP8 video. Delayed-loopback
records up to two seconds / 64 KiB, replies in bounded chunks after 300 ms, then
records again. **100 Redis commands and 128 KiB request+reply per call-second are
upper budgets**, not a request to generate artificial store traffic or 100 app
tasks per second. This keyless example does not model provider processing.

The report measures both current checkpoint-age and snapshot-age p99 with 100 ms
private diagnostic sampling. Both must be at most **550 ms**. Each session needs at least 90% of its
scheduled 100 ms samples; more than 10% missing samples overall blocks approval.
These allowances cover brief owner handoffs, not an unsampled subset of calls.
The sampler ignores requests retired during a deliberate worker kill and runs
only for custom agent identities. It reports Save
wall time and Linux thread CPU per Handle event and per call-second, Save/event
counts, forced intent attempts/successes/failures, successful byte debits, all
session/owners/frame clients, logical operations, and actual Redis wire command
starts and request+reply bytes. Wire starts count go-redis's internal retries and
connection setup; the custom gate uses those counts for the 100-command budget.
Driver diagnostics/Redis INFO monitoring are separate from application store
traffic. A Redis wire parser error anywhere in the process lifetime blocks approval,
including one before the measurement window. The example uses ReadOnly
work, so zero forced writes is expected; the store-stall semantic lane separately
exercises failed and successful forced writes against the real ledger worker.

Export evidence is the largest actual encoded HTTP response body, below 4 MiB.
Move reservations, worker heap/RSS, and HTTP bytes are reported separately. Save
CPU excludes concurrent packet readers, task handlers, and host serialization.
Non-Linux runs mark per-Save CPU unavailable and block approval. The report keeps
raw echo audio measurements, while the custom judge enforces transport/video
continuity and returned audio presence. Delayed audio cannot pass an echo
freshness limit. Exact audio preservation and crash loss use the separate cut
matrix; loaded audible continuity still needs application review.

A local loopback step can collect preliminary evidence:

```sh
./bin/relais-load -capacity -worker-bin=bin/delayed-loopback -agent=delayed-loopback/v1 -duration=20s -capacity-start=2 -capacity-max=2 -output=bin/custom-local-final.json
```

Linux namespace isolation and the N+1/drain ramp require the manual Linux run.
Over-budget ages, Save limits, store traffic, missing measurements, invalid
load generation, process failure, or an unvalidated N+1 load **block approval**
at the validated operating load. Approval requires every recorded worker/relay
step at or below its validated lower bound, plus N+1 and drain at the candidate
load. Steps above each lower bound are retained as raw data with their status
and causes. An inconclusive stopping step from generator saturation does not
invalidate the lower bound; a failure at or below that bound still blocks.
The artifact and job summary say either **BLOCKING APPROVAL** or **EVIDENCE READY
FOR OWNER REVIEW**. Neither grants approval automatically. Approval binds to the
exact commit, worker binary SHA-256, workload, runner, and operator ceilings in
the artifact. Green main CI alone is not approval, and larger Demo A budgets do
not create a capacity claim.

The existing nightly/manual `store-stall` CI job adds custom-worker live stalls
of 5/15/29 s, a stale takeover, and 35 s ownership expiry. Live Emit evidence
requires a published task turn absent from the pre-stall task list; a reply
already queued before the stall cannot satisfy it.

The job also starts the example's local ledger service and runs its separate
versioned `local-ledger/store-stall-v1` registration under Redis SIGSTOP:

```sh
./bin/crash-run -worker-bin=bin/delayed-loopback -agent=local-ledger/store-stall-v1 -store-stall=ledger -runs=1 -after=5s
```

This diagnostic workload waits for audio before creating two SideEffecting
intents. The driver pauses only its own Redis, lets pre-pause writes time out
for 2.2 s, takes the token baseline, then starts audio for the requested 8 s cut.
It records failed forced attempts, zero successful forced writes and byte
debits during the cut, no Capacity outcomes or ledger deliveries, and a local
Timeout for the short-deadline task. After recovery it requires a later forced
checkpoint, one successful commit, and a launch-boundary witness with the
matching acknowledged intent and an unexpired deadline. The expired task must
never launch. Session diagnostics retain only the last launch witness and a
counter; these process-local fields are never checkpointed or moved.

The normal `local-ledger/v1` teaching registration still commits on Started.
The new registration is necessary to create intents after the store is paused.
Unknown agents are refused in these workload-specific cuts. The existing
in-process fault-injection tests remain and complement these real custom
binary cuts. No cut simulates an external provider.

## Move, kill, and drain evidence

Run the example's real callback/task/pacer tests on memory without a Redis server:

```sh
RELAIS_TEST_REDIS_ADDR= RELAIS_TEST_REDIS_REQUIRE=0 go test -race -v -count=1 -run '^TestDelayedLoopback' ./pkg/callharness/
```

The full matrix moves during recording, processing, and playout; checks exact
encoded output once; takes over at all three cuts; checks cleared refs/new turns;
and retries the local ledger with one committed effect. A smaller-target case
checks that crash-lost bytes do not block adoption. Heavy matrices skip under
`-short`; fast app tests and a real timer/recording-move case remain.

Private worker `GET /status?agent-session=<id>` shows copied app metadata,
input/task cursors, pending move reservations, recording/request/playback bytes,
current checkpoint and snapshot ages, and the app's last resume/replay notice.
An unknown session returns HTTP **409** with `unknown_session`.
It reads accepted cached state without calling Save. Pending reservations include
bookkeeping; they are not exported HTTP sizes. Signal does not expose this query
or the private diagnostic state.

The call harness uses a worker kill seam for in-process takeover on memory and
Redis. `TestDelayedLoopbackProcessCuts` in `pkg/workerapp` separately exercises
real SIGKILL at all three cuts with dedicated Redis. Redis verification must use
a disposable server on a port other than 6379. These tests do not prove provider
behavior, browser DTX capture, loaded drain time, or capacity approval.

Drain currently uses the existing urgent export path. **Drain does not wait for
handlers until slice 9.** That slice adds worker-local quiescence and a measured
operator move reserve. Do not assume a five-second shutdown deadline includes a
handler wait or that it waits for the whole reply to play.

## Same-registration upgrades and Demo A

All pool members must restore the previous app `State.Version` and pending task
request/result schemas, and retain compatible recovery capacity. Test old
snapshots before a rollout. Compatible names alone do not authorize mixing
incompatible task code. Use another pool or end calls for an incompatible change.
Install v8-capable platform readers before enabling custom v2 snapshots; this is
not a general mixed-version deployment promise.

The next tutorial features **arrive with Demo A**:

- Control push-to-talk and its routed HTTP sender/receipt path.
- STT Ogg upload using the encoded packets and a validated Pion writer path.
- The TTS packet reader with bounded Ogg packet assembly and format fixtures.

Realtime/live STT, PCM conversion, durable conversation/result blobs, and provider
connections that survive worker restart remain outside this slice. See
[ADR 0010](adr/0010-pluggable-agents.md) for the accepted contract and later work.
