# Nvwa two-player room spike

Disposable app protocol and WebSocket comparison for #162/#163. This is a message/action-reference demo, not a character or animation player. The room has no durable storage. Relais production media defaults remain unchanged. Only `-agent=room-audio -room-audio-bus=udp://127.0.0.1:PORT` selects the experimental audio bridge.

## Layout

- `room/`: shared authoritative room core, direct WebSocket adapter, frozen upstream framing, capability lobby, signal proxy, UDP audio bus.
- `client/`: Go protocol consumer over a bounded message connection.
- `media/`: Pion caller through real `relais-signal`; stage-2 DataChannel seam.
- `audioagent/`: opt-in legacy `pkg/agent` implementation. No agent v2 or task changes.
- `cmd/room`: standalone room process and browser page.
- `cmd/topology`: owned Redis, relay, two workers, control, signal and room processes.
- `cmd/driver`: two Go/Pion clients, numbered workload, faults, raw recorder.
- `cmd/backpressure`: pauses one app writer, saturates its queue, checks explicit closure while peer audio continues.
- `web/`: browser client for either transport, audio, authoritative snapshots, command/query/reconnect controls.
- `scripts/`: bounded build, matrix and isolated browser launch helpers.
- `evidence/`: stage-1 report and compressed raw receipts. Credentials stay in private scratch.

## Build and run

From the repository root, with existing cached Go modules, Redis and ffmpeg. No module is added or downloaded. Use only your own scratch directory and binaries. The harness allocates loopback ports dynamically, refuses Redis port 6379, and records every child PID. It does not touch 9101, 9201, existing Chrome or the shared DevTools daemon. Ports are intentionally ephemeral; the shared port registry has no Relais reservation and this spike does not change it.

```sh
nvwa_scratch=$(mktemp -d /private/tmp/nvwa-room.XXXXXX)
bash experiments/nvwa-room/scripts/build.sh "$nvwa_scratch"

# Shared local topology, media through relay -> real worker -> room audio bus.
# It prints room_url; open ROOM_URL/web/ for manual clients.
"$nvwa_scratch/bin/topology" -bin "$nvwa_scratch/bin" -out "$nvwa_scratch/manual"
# Ctrl-C / SIGTERM to this topology process stops only its own children.
```

An independent room, when an owned Relais topology is already running:

```sh
"$nvwa_scratch/bin/room" -http 127.0.0.1:38690 -audio 127.0.0.1:38691 \
  -signal https://127.0.0.1:SIGNAL_PORT \
  -signal-cert "$nvwa_scratch/owned-topology/signal.pem" \
  -signal-token "$nvwa_scratch/owned-topology/signal.token"
# Start its task-owned Relais workers with:
# -agent=room-audio -room-audio-bus=udp://127.0.0.1:38691
# Add -test-faults only in an owned experiment to allow the two fault hooks.
```

The topology helper uses the same binaries/flags as `deploy/local`, with ephemeral ports, a dedicated Redis, `frame-cache-off`, and the room-audio option. The production local script currently accepts only echo/demo, so the helper owns this opt-in orchestration instead of changing that script. TLS certificate and IP verification remain enabled; the signal bearer stays server-side. The browser receives only its lobby capability.

One exact independent run (both data transport and fault mode are explicit). Use a fresh output directory for each invocation and finish building before starting the matrix:

```sh
"$nvwa_scratch/bin/driver" -bin "$nvwa_scratch/bin" \
 -out "$nvwa_scratch/normal-1" \
 -tone-a "$nvwa_scratch/tone-440.ogg" -tone-b "$nvwa_scratch/tone-880.ogg" \
 -count 1000 -rate 30 -fault normal
# Substitute disconnect, worker-kill or lost-receipt.
```

The workload is 1,000 numbered operations per player: 500 reliable commands alternating trigger/stop and 500 replaceable state updates. Each player sends at 30 Hz; Opus media sends at 50 Hz. Command/state payloads are fixed JSON. Per-operation `bytes` in `result.json` uses a conservative five-character request-ID placeholder; the evidence exporter corrects this with the observed request-ID length and reports the exact numbered-message app-byte range. Client IDs, room epoch, request IDs, generation and timestamps add measured envelope bytes. Normal runs last approximately 35 seconds. Recovery does not replay missed high-rate ticks or burst a backlog.

For the 12-run matrix (over five minutes), use wefty as required by the working agreement. Its output and `result.json` files go to the supplied task scratch. Each child run is bounded at 90 seconds and the matrix timeout is 12 minutes. Record the run ID/result in the task's wefty log.

```sh
wefty exec --detach --json --max-runtime 12m -- \
 python3 experiments/nvwa-room/scripts/run-matrix.py --scratch "$nvwa_scratch"
# wefty inspect RUN_ID --json
# wefty logs RUN_ID --follow
# wefty cancel RUN_ID   # cancellation only for this owned run
```

For runs expected to outlive the calling session, keep the wefty lifetime. All helpers clean up on normal completion. To stop a manually backgrounded helper, save its `$!` immediately and send SIGTERM only to that owned PID. Never kill processes found by name/port. Never remove shared state or use Redis 6379. A hard kill of the driver/topology itself cannot run Go defers: keep it alive to clean its children, or use its `topology.json` and independently verify PID ownership before manual cleanup. Stopping a test worker is different: the harness retains and stops the rest.

Bounded app backpressure with simultaneous real media:

```sh
"$nvwa_scratch/bin/backpressure" -bin "$nvwa_scratch/bin" \
 -out "$nvwa_scratch/backpressure" \
 -tone-a "$nvwa_scratch/tone-440.ogg" -tone-b "$nvwa_scratch/tone-880.ogg"
```

Two isolated browser clients (about seven seconds of page workload, 90 messages each):

```sh
python3 experiments/nvwa-room/scripts/browser-smoke.py --scratch "$nvwa_scratch"
# --browser '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'
# is an explicit alternative, still using only task-owned separate profiles.
```

It launches the requested executable with separate scratch `--user-data-dir`, fake-device/fake-UI flags, and a different `--use-file-for-fake-audio-capture` WAV for each caller. It creates no shared DevTools connection. It terminates only its own Popen children. Browser runtime requires the executor to permit Chromium's macOS process handshake; it is NOT-RUN when that launch is denied. Do not bypass a sandbox denial through wefty.

## Protocol

One UTF-8 JSON message in each binary WebSocket frame or DataChannel message. App payload max 1,024 bytes, including envelope. Both adapters call the same `Room.Handle`; direct WS uses implicit control/state. DataChannel labels are `control` (ordered reliable) and `state` (unordered, maxRetransmits=0). WebSocket remains reliable/ordered; state replacement is an app rule and does not pretend to provide partial reliability.

`POST /lobby` issues at most two random capabilities, each valid for 30 minutes. A capability binds a server-generated `p1`/`p2`, fixed character reference and one room. Client-declared player identities never authorize operations. The listener is loopback-only, with browser-origin checks. This is a trusted local spike, not a public identity/lobby service.

| Request | Required fields and effect |
| --- | --- |
| `join` | `room_id`, capability `token`, optional known `room_epoch`, `session_id` from signal. Confirms `connection_generation`, takes over the identity and stops the old subscription. Upstream session must match the worker's query parameter. |
| `trigger` | Stable `command_id`, `input_seq`, `payload_digest`, `payload={id:"wave",version:1,start_tick:N}`. The room tick is milliseconds since process start. Restoring a snapshot retains that tick; reconnect does not trigger again. |
| `stop` | Same command identity fields, `payload={}`. Clears the action reference. |
| `leave` | Same command fields. Stops membership/audio routing and future state mutation; recorded leave can still be queried/retried. |
| `snapshot` | Reads both players' character, action reference/time, generation, membership, latest state and sequence. |
| `query` | `command_id`. Returns the recorded result or `unknown_command`. |
| `state` | Increasing `seq`, `sent_ns`, JSON `payload` up to 128 bytes. Stale sequence is rejected with `stale_state`. No input replay queue exists. |

After join, every request also carries `room_id`, `room_epoch`, confirmed `connection_generation` and a fresh `request_id` for correlation. A command's SHA-256 digest covers canonical JSON payload (sorted object keys; Go preserves JSON number text). The stable identity includes command type and input sequence as well. Reconnect changes only the envelope generation; retries keep command ID/type/sequence/payload/digest. The ledger key is `(epoch, player, command_id)`; generation is excluded. Applied results and admitted invalid-action results are recorded. Different content is `command_conflict`, including retries through a new generation. Size/rate/capability admission refusals are not applied commands.

Replies are `joined`, `snapshot`, `state`, `result`, or `error`; they carry `room_id`, `room_epoch`, `room_version`, `room_tick`, optional `request_id`, `error`, `players`, and recorded `result`. A result includes command ID/type/input sequence/digest, applied flag, original room version and any recorded rejection. Only a recorded `applied:true` means the room applied the command. Transport Write, SCTP ACK, and receipt arrival are not app commit. Result envelopes may show the current room version while `result.room_version` remains the original version.

Restart creates a random new epoch. An old-epoch request gets explicit `epoch_reset`; an unknown command in the new epoch is not inferred successful. The ledger is memory-only. A room crash is outside the recovery guarantee.

Frozen worker upstream is `/relais-upstream?session=ID&worker=NAME`. Frames are `[op, labelLength, label, payload]` with open=1, message=0, close=2, end=3, exactly as the dispatch's `INTERFACE.md`. One upstream connection is capped at four labels. The core has framing coverage; real DataChannel negotiation, worker forwarding and its move/drain/crash policies require W1 and stage 2.

## Bounds and measurement limits

- At most two capabilities, 4,096 admitted command results per player, 16 simultaneous sockets, four upstream labels, 120 incoming app/framing messages per connection per second, and 120 authorized operations per identity per second.
- Max app payload 1 KiB. Worker framing adds at most 257 bytes. Receive allocation is explicitly limited; no fragment reassembly queue is implemented in the app.
- Max pending output, including a write in progress, is 16 KiB per connection. A terminal error frame is reserved inside that cap. The queue also has a 128-entry limit. Slow consumers receive typed `slow_consumer` when the socket can still deliver it and are disconnected; a blocked socket has a bounded write timeout and may expose only closure. Commands remain queryable if the receipt is lost.
- Additional bounded app allocations: one inbound message, one JSON output temporary, queue timestamps/slots, and two latest Opus packets (<=1,200 encoded bytes each). Library WebSocket framing/IO, TLS, Pion media buffers and kernel UDP/TCP socket buffers are additional costs. The recorder measures app queue bytes/age, not whole-process/kernel buffering or SCTP buffering; those are NOT-MEASURED in stage 1. W1 must account for SCTP/DataChannel budgets in stage 2.
- No arbitrary asset/media bodies go on the app path. The audio bus caps datagrams at 1,500 bytes, stores only current two joined sessions, and expires peer packets after 100 ms. Agent callbacks use a fresh UDP socket with a 4 ms deadline, avoiding retained sockets without an Agent.Close hook. A missing peer packet suppresses output.
- Room metrics include applied/dedup/conflict/stale/fenced counts, dropped receipts, slow consumers and peak app queue bytes/age. Queue peaks are finalized when the connection closes. Raw client observations and per-operation RTT are retained. The exporter derives duplicate/reordered receipts from actual incoming request IDs within each generation; the driver receipt counters alone only cover successful state request returns. The intentional lost receipt is recorded separately from unrecovered app-message loss. Latest-state age is same-host wall-clock age at receipt, including the peer's last state during a pause, not clock-synchronized cross-host latency. Peer media reports retain packet arrivals and exact peer/self/unknown Opus matches; audio interruption is the largest observed arrival gap, not perceived human quality.
- Fault recovery is explicitly driven by the harness; no automatic browser detection time is claimed. `disconnect` closes A's owned app and media connections and waits 250 ms. `worker-kill` SIGKILLs the recorded owned worker while room/Redis stay alive, then recreates A's app/media connections. WS is independent of the worker and is deliberately rejoined to use the same recovery contract. Recovery time spans the last authoritative observation to the new join snapshot; cut-to-recovered is recorded separately. Reconnect does not migrate the old connection.
- `lost-receipt` applies one reliable command, drops only its app receipt using the test flag, waits 150 ms, queries, retries unchanged ID and checks a changed-content conflict. Side effects must remain exactly one. `test_pause` (test mode only, <=2 s) pauses one writer to exercise real queue overflow with media continuing.

## Verification commands

```sh
GOPROXY=off go test -mod=readonly -race -short -count=1 ./experiments/nvwa-room/...
GOPROXY=off go test -mod=readonly -race -short -count=1 ./pkg/workerapp \
 -run 'Test(RoomAudioSelectionIsOptIn|DemoAgentSelectionAndBoundedHistory|RunReturnsFlagErrorsWithoutExiting)$'
GOPROXY=off go test -mod=readonly -race -short -count=1 ./pkg/mediaworker \
 -run 'Test(AgentDefaultEchoRestoresLegacySnapshot|AgentEchoDoesNotPauseOrDropAtExport|AgentEchoRejectsStatelessCustomState)$'
GOPROXY=off go test -mod=readonly -race -short -count=1 ./pkg/callharness \
 -run 'Test(AudioOnlyCall|BrowserLikeOfferCall|ChromeOfferIsAnswered)$'
```

No paid API, model generation, new module download, cloud or Nvwa source changes. Native Rust/Bevy, actual animation playback, Internet/TURN, cross-host/region, Safari/mobile, production capacity and persistence after room death remain NOT-RUN. Adoption decision and DataChannel/WebSocket comparison wait for stage 2 and orchestrator review.
