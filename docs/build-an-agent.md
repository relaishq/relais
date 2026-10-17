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

Custom agents support the plain crash-run worker takeover path. `-sigterm` (drain),
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
Ignore unknown events. Routed `Control` events carry a stable `EventID` and a copied `ControlMessage`.

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

## Routed Control

Send `POST /calls/{id}/agent/control` through `relais-signal` with the existing
bearer token and `Content-Type: application/json`:

```json
{"id":"press-1","sequence":1,"name":"ptt.start","data":"AP8="}
```

`data` is base64 for binary bytes, as with Go `[]byte`. It is optional and is
limited to 1,024 decoded bytes. IDs and names must be nonempty UTF-8 strings of
at most 128 bytes. Use a new unique ID for each message. Start sequences at 1;
send each new message with the next sequence. Retries keep the **same ID,
sequence, name and bytes**. Future sequences return `control_order` without
consumption; send missing messages first. This preserves callback order.

A reply contains `{id, sequence, event_id, checkpointed}`. **202 Accepted** means
the actor accepted the callback and saved its local state and dedup record.
**200 OK** means a successful fenced checkpoint recorded that acceptance.
Retry until 200 or `control_stale`, with bounded delay and an application deadline. A lost reply or
routing error is uncertain: retry the same message, including after a move or
takeover. Never mint a new ID to recover an uncertain request. Control has no
media data channel. The client supplies no worker URL or private route.

The agent receives `agent.Event{Kind: agent.Control, ID: ..., Control: ...}`.
The ID is stable across identical retries, moves and crash redelivery. Local
admission, callback state, effects and receipt share one publication transaction.
An in-flight callback completes before export, or the request is refused and
retried on the adopted route. Exact moves carry the receipt window and its
checkpoint acknowledgement. A 202 can be lost on a crash: the callback can run
again with the same EventID on the new owner. Only committed logical history is
deduplicated; external effects still need the task class and provider rules above.

Receipts store fixed-size SHA-256 ID/body digests. Their versioned binary encoding
is 73 bytes per record (102 bytes in JSON). Control has a separate byte cap
measured from **two** records and maximum-width sequence/floor counters, at most
one third of pending metadata. At default limits at most two unacknowledged
Controls fit. Timer/task admission charges only receipt bytes actually held;
agents without Control retain their original encoding and accounting.
Control-bearing snapshots pack task-refusal digests and sparse turns into a
versioned binary representation, preserving every task ID, order and outcome.
This keeps refusal-only fan-out representable even when the receipt cap is full.
Upgrade every pool worker and `relais-control` to this slice before enabling
Control clients. Rolling back workers or `relais-control` while Control calls
exist breaks takeovers for those calls. The new
reader also accepts the previous object-form refusals; older workers refuse
any Control-bearing snapshot: its session state version is 9. Snapshots without
Control remain version 8, and the current reader accepts both versions.
Pool workers should use the same agent ceilings, or effective limits can make
Control support differ between workers.

For the default-limit unit registration, read-only tasks with short steps and
zero turns admit the following numbers; remaining tasks receive Capacity:

| Fan-out width | No Control | One held receipt | Two held receipts (full) |
| --- | --- | --- | --- |
| 20 | 4 | 4 | 4 |
| 24 | 3 | 3 | 2 |
| 33 | 0 | 1 | 0 |

Exact capacity depends on registration metadata, turns, timers and task contents.
If retained Control prevents an otherwise representable refusal-only transaction,
the host drops acknowledged receipt details within the accepting transaction,
keeping the sequence floor and high mark. If unacknowledged receipts still block
admission, the host restores the callback and returns backpressure. The actor
keeps the event/cursors and retries each receipt-blocked event at most once per
250 ms; other events continue normally. Playback/mailbox backpressure does not
arm this delay. A new checkpoint acknowledgement permits an immediate retry.
Oversized callbacks that cannot fit even without Control still fail closed.

The callback's real encoded `data` also counts while admitted. A full Control cap
returns retryable `agent_capacity` without invoking Handle or consuming a sequence.
Only acknowledged prefixes may compact, including under task, timer, audio or
emission admission pressure; at defaults a prefix more than one
sequence behind the new sequence can expire. Smaller pending limits may compact
acknowledged prefixes earlier. If the runtime registration's pending limit cannot
hold even one record at maximum counter widths plus registration/quota metadata,
Control is definitely `control_unsupported`; increase the agent limits rather than
retrying the same message. This does not disable its other v2 events.
Old sequences remain closed after
compaction and return `control_stale`, never another callback. For a client
retrying its own original message, `control_stale` proves **durable acceptance**:
sequences are contiguous and only checkpointed prefixes can compact. Stop
retrying that message; its detailed receipt is no longer retained. Retained receipts
return the original EventID; changed IDs, sequences or bodies that collide with a
retained entry return `control_conflict`. IDs must remain unique even after an
old receipt expires. This sparse receipt window has no wall-clock expiry.

Before Started is accepted, Control returns retryable `control_not_ready`.
A Control Handle error or deadline outside Save is quarantined; after successful
Restore the host returns retryable `control_rejected` without consuming the
sequence or ending the call. Retry the identical message with bounded backoff.
Save/Restore failures and invalid state/effects remain terminal agent failures.
Keep external side effects in Tasks, since Restore only rolls back app state.

The route remains serialized with operator changes. Move and Drain wait a
bounded time for an in-flight Control, and waiting operators take priority over
new Control retries. A Control holds the route for at most 500 ms; takeover can
cancel that RPC within its existing 1.5-second budget. A transport timeout may
still follow worker acceptance: retry the same message on the live route.
Malformed JSON, unknown fields, invalid base64 and wrong field types return
400 `control_invalid`; identical retries cannot repair them.

Legacy echo and demo registrations return `control_unsupported`. Their media
behavior is unchanged. Authentication and network exposure retain the existing
deployment constraints; this adds no per-call authorization or public Internet
control-authentication guarantee. The private worker endpoint is
`POST /sessions/{id}/agent/control`; trusted control resolves live store ownership
under the existing call lock.


Control error classes (codes preserve the existing HTTP status):

| Class | Codes | Client action |
| --- | --- | --- |
| retryable | `agent_backpressure`, `agent_capacity`, `barrier_timeout`, `canceled`, `checkpoint_clock`, `closed`, `control_not_ready`, `control_rejected`, `deadline`, `export_not_flushed`, `held`, `hold_expired`, `hold_limit`, `lease_lost`, `move_in_progress`, `no_target`, `not_established`, `replay_in_progress`, `replay_not_ready`, `replay_target_changed`, `session_exists`, `transient`, `unknown_session` | Retry the identical message with bounded backoff and a deadline. |
| definite | `move_budget`, `agent_incompatible`, `agent_oversize`, `agent_restore`, `agent_save`, `agent_version`, `buffer_disabled`, `control_conflict`, `control_invalid`, `control_order`, `control_stale`, `control_unsupported`, `ended`, `export_incompatible`, `not_found`, `sequence_budget`, `srtcp_exhausted`, `unknown_call`, `unsupported_offer` | Stop automatic retries; resolve ordering, registration, limits or call state. For control_stale, acceptance is durable. |
| uncertain | `internal`, `uncertain` | Acceptance is unknown; retry the identical message. Internal is exposed as uncertain. |

`move_budget` is definite for Control: exceeding move-memory admission fails
closed under the existing host contract. `agent_incompatible` is definite because the registration, state or callback effects need a software
or operator fix; replaying the same message does not repair them. `closed` means
retry on the live route. Unknown/missing codes are uncertain on every status,
including 409. Only a non-envelope 400 (private JSON decoding failure) becomes
`control_invalid`; an unfamiliar JSON error envelope remains uncertain.

## Reading provider Ogg/Opus

`pkg/agent/opusio` provides a pure-Go reader:

```go
reader, err := opusio.NewReader(body)
// Handle err. NewReader validates OpusHead and OpusTags.
for {
    audio, err := reader.Next(ctx)
    if err == io.EOF { break }
    if err != nil { return err }
    // Emit audio from the task handler; it contains one unchanged Opus packet.
    _ = audio
}
```

`Header()` reports channel count, input sample rate and pre-skip. Timing always
uses 48 kHz. `PacketDuration()` reports the encoded packet duration from its TOC.
`Position()` subtracts pre-skip and final granule trimming from reported audible
time. Encoded packets retain preroll and padding; this is **not sample-exact
PCM trimming**. The checked spike fixture is mono, 24 kHz input metadata,
pre-skip 312 and 80 packets of 20 ms each. Tests rebuild Ogg from those packets
with the pinned Pion writer; no provider connection runs in those keyless tests.
Demo A's opt-in local tests exercise the real speech endpoint.

The reader keeps one page and one packet, each bounded near 64 KiB. Oversized
headers/packets, CRC failures and truncation are errors. Empty or malformed
audio packets are discarded per RFC 7845 section 3 and RFC 6716 section 3.4;
`DiscardedPackets()` exposes the count. Timing resynchronizes at the next
completed page's granule after a discard. Malformed Ogg structure and headers
remain fatal typed errors (`ErrContainer`, `ErrCRC`, `ErrHeader`, `ErrGranule`,
`ErrLimit`), rather than audio-packet errors. OpusHead versions 0–15 are accepted
as compatible major version zero. Mapping family zero
(mono/stereo) and zero output gain are supported. Chaining/multiplexing, other
mappings, gain, and cropped streams with a nonzero initial granule offset are
explicit typed errors. Cancellation is checked between reads; the underlying
reader must provide its own cancellation/deadline for a blocked read. Read
through EOF to validate the stream end and reject a chained stream.

## Same-registration upgrades

All pool members must restore the previous app `State.Version` and pending task
request/result schemas, and retain compatible recovery capacity. Test old
snapshots before a rollout. Compatible names alone do not authorize mixing
incompatible task code. Use another pool or end calls for an incompatible change.
Install v8-capable platform readers before enabling custom v2 snapshots; this is
not a general mixed-version deployment promise.

## Run Demo A

The [voice-agent example](../examples/voice-agent) is a push-to-talk worker in
this module. It registers `voice-agent/v1` and four versioned ReadOnly task kinds.
It uses public imports only and is built by `make build`.

```sh
CGO_ENABLED=0 make build
# Set OPENAI_API_KEY in this shell through your secret manager. Do not put it
# in a command argument, a recording, a result file, or a committed .env.
./bin/relais-demo -http=127.0.0.1:19102 -worker-bin=bin/voice-agent -agent=voice-agent/v1
```

Open the printed launch URL, which includes a per-launch Control capability in
its fragment. Use headphones. Start the microphone call, hold **Hold to talk**,
speak, then release. The page shows the transcript, reply, phase and recovery
notice. The returned voice is AI-generated. No key: the worker still starts,
but each press reports `Failed` / `provider_unavailable` on the page and sends
nothing to OpenAI. The example reads only `OPENAI_API_KEY` in the worker's
process environment. The launcher passes it only to worker children, including
replacements; it does not send it to the browser, relay, control or Redis.

Demo A declares **8,192 raw app-state bytes**. Each worker needs these operator
flags. `relais-demo` supplies them automatically for `voice-agent/v1`; a worker
started in another topology needs all three explicitly:

```sh
-agent-max-snapshot-bytes=24576
-agent-snapshot-bytes-per-second=270336
-agent-snapshot-burst-bytes=98304
```

Keep the same ceilings and registration on all workers. The default 1 MiB
move budget per call and 64 MiB per worker suffice for this bounded example.
These are demo settings, **not a capacity claim**. They do not change the
keyless example's manual capacity gate or the default worker ceilings.
`relais-demo -worker-arg=-flag=value` passes an additional non-secret worker
flag to every initial and replacement worker.

### Push-to-talk Control

The browser sends `ptt.start` and `ptt.stop` through the authenticated
`POST /calls/{id}/agent/control` route. The owned demo checks its per-launch
`X-Relais-Demo-Token` capability and proxies to trusted control. A deployment
uses the existing bearer authentication on `relais-signal`. The browser retains each exact JSON message
while retrying. A 202 permits the next sequence to be sent, so release can
follow a press during a store stall. Each message continues retrying until
200 with `checkpointed: true`, or `control_stale`. Retryable and unfamiliar
codes use per-message backoff from 50 ms to 1 s, with a 15 s deadline. Definite
refusals stop. A timeout reports uncertainty and disables new presses until a
new call; it never invents another ID for an uncertain press. At most two
unacknowledged Controls fit: the next press can wait on `agent_capacity`.
Pointer release, cancellation, keyboard release, focus loss and a hidden page
all release the button. Caller audio is never delayed behind Control retries.

`start` cancels older tasks and clears their unsent audio. This is barge-in.
`stop` submits the recording; an eight-second timer or 64 KiB bound also ends
it. Empty or invalid recordings produce an explicit outcome. The agent accepts
only unseen caller packet indexes and retains packets in move memory.

### STT upload, saved text, and speech

The task chain is deliberately small and split at paid-result boundaries:

1. `voice/stt/v1` wraps the recorded packets with Pion `oggwriter` and uploads
   **`recording.ogg`** to `gpt-transcribe`. The spike verified that these bytes
   named `.ogg` work while `.opus` is rejected. Pion defaults to an 80 ms
   pre-skip. Caller RTP has no Ogg encoder preroll, so this writer sets pre-skip
   to zero and updates the header CRC. Even one 20 ms packet retains its duration.
2. `voice/reply/v1` sends the saved transcript plus at most four recent
   exchanges to `gpt-6-luna`. The response is limited to 96 tokens and asks
   for at most three short spoken sentences (at most 30 words). The saved reply
   is limited to 512 JSON bytes at UTF-8 rune boundaries, including escaping.
3. `voice/speech/v1` requests `gpt-4o-mini-tts`, voice `coral`, with
   `response_format: opus`. `opusio.Reader` reads its Ogg/Opus response. The
   result is bounded per segment to 64 KiB / 8 seconds, mono, with no discarded
   packets; all segments together are limited to 128 KiB / 20 seconds.
4. `voice/play/v1` emits unchanged packets in stable 100 ms chunks, with the
   first chunk immediately available to the pacer. A move resumes above
   `TaskRun.After()` from the same accepted speech bytes.

The saved reply splits deterministically at sentence ends. Very short pieces
merge forward; long sentences split at whitespace or a rune boundary. There
are at most eight segments of 64 Unicode characters each. The first segment
plays as soon as its complete rendering is accepted. While it plays, exactly
one later segment may render. A short inter-segment gap is possible.

Each segment gets a separate playback turn from a reserved range, so local
send completion can advance to the next segment while its successor renders.
The cursor is the segment index plus that playback task's chunk sequence.
Completed audio stays move-only in either the pending segment buffer or the
playback request. A move carries those exact bytes; it never regenerates a
completed or playing segment. An interrupted, unpublished render may repeat,
because none of its audio has played. This uses the existing ADR task and
playback contracts without a provider connection or external audio store.

Only transcript/reply text, four bounded exchanges, phase and cursor metadata
enter version-2 app state. Version-1 app snapshots are refused; drain old
voice calls before deploying this incompatible example update. Save never
reads/copies/hashes audio. A stage whose result was already accepted skips its provider call when retried with a
nonzero `After()`. Later stages take saved text, so STT is not repeated after
the transcript exists, and the LLM is not repeated after the reply exists.
Provider transport/5xx/429 uncertainty gets one same-key retry, then a typed
`OutcomeUnknown`; definitive failures report `Failed`. Neither ReadOnly nor
an idempotency header guarantees exactly-once billing. Moves during an
unfinished request can repeat that request and charge again.

**Move / Drain:** recording, provider-task requests, accepted speech and the
playback cursor move together; the turn continues. **Kill:** move-only input,
provider requests and unsent audio are lost. The page reports
`OutcomeUnknown` / `turn_interrupted`, preserves checkpointed text, clears the
lost lengths, and accepts the next press. It never silently reconstructs a
lost recording or restarts paid stages from `Resumed`.

For a short 2–5 s input and a short spoken reply, allow roughly **$0.002–$0.01
per turn**, depending mainly on generated speech length; unfinished retries
can add charges. Current provider references: [transcription](https://developers.openai.com/api/docs/guides/speech-to-text),
[GPT-6 Luna](https://developers.openai.com/api/docs/models/gpt-6-luna),
[speech output](https://developers.openai.com/api/docs/guides/text-to-speech)
and [pricing](https://developers.openai.com/api/docs/pricing).

Keyless unit and harness tests use the recorded synthetic speech fixture and
canned text behind the same Provider interface. Network cut tests skip under
`-short`. Live tests are opt-in only. `RELAIS_VOICE_LIVE=1` runs the in-process real
provider turns; `RELAIS_VOICE_LIVE_PROCESS=1` checks the shipped worker
with a real OS SIGKILL. Both require `RELAIS_VOICE_LEDGER` and a key in the
process environment, or an explicit private `RELAIS_VOICE_LIVE_ENV` file
loaded only in process. They never run in CI or load an env file by default.

Realtime/live STT, PCM conversion, durable conversation/result blobs, and provider
connections that survive worker restart remain outside this slice. See
[ADR 0010](adr/0010-pluggable-agents.md) for the accepted contract and later work.
