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
`bin/delayed-loopback`. Its default agent is `delayed-loopback/v1`. It accepts
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
task request. The handler waits 300 ms, computes SHA-256 over the versioned
recording, then emits encoded chunks with stable sequences. Normal 20 ms input
produces five packets per 100 ms chunk. Other packet durations keep their packet
boundaries; chunks reach at least 100 ms and the final chunk may be shorter.
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
demonstrates `SideEffecting` plus `Retry`. Inspect its server options with:

```sh
./bin/delayed-loopback ledger -h
```

Run its `ledger` mode as a separate process on a literal loopback address, with
`-file` naming its private JSON-lines file. Keep exactly one ledger server per
file. Set each demo worker's `DELAYED_LOOPBACK_LEDGER_URL` to the printed HTTP
origin and select `-agent=local-ledger/v1`. This separate registration commits one
demonstration entry per call. It uses no provider and accepts only loopback HTTP.
The loopback audio agent does not require the ledger.

The ledger syncs a first commit before acknowledging it and returns the same
receipt for the same key and value. Delivery can occur more than once; commit
occurs once per logical key. A different value for that key is a conflict. The
file survives worker and ledger restarts. Lost replies, server uncertainty, and
unverifiable receipts end with `OutcomeUnknown`; they do not prove that no commit
occurred. A write error makes the running ledger
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

**The manual custom-worker capacity/Save-CPU and store-stall gates arrive with
slice 7b.** Green CI is not capacity approval. Before enabling v2, attach the
exact build, operator settings, and measured manual results for review. ADR 0010
requires all store clients/retries, 100 commands and 128 KiB request+reply per
call-second, checkpoint-age and snapshot-age p99 at most 550 ms, plus Save CPU/time.
Export HTTP size and memory need separate evidence. This example makes no new
capacity claim, and Demo A's larger budgets will not make one either.

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
