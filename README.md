# Relais Media Server

Relais is a distributed media server built in Go that supports flexible ingress and egress of media streams through a plugin system. Live WebRTC media runs in the media worker prototype, built on Pion v4 libraries.

## Features

- **Plugin System**
  - Ingress plugins for media input (e.g., camera, RTSP)
  - Egress plugins for media output (e.g., WebRTC, S3)
  - Transform plugins for media processing (e.g., watermarking)

- **Storage Backend**
  - Distributed storage for media frames
  - Supports Redis and in-memory implementations
  - Easy to extend with new storage backends

- **Horizontal Scaling**
  - Run multiple plugin instances
  - Distributed storage support
  - Multiple media workers: a live call can move between workers on the same UDP socket (experimental), and separate workers can sit behind a relay on one public UDP port (planned handovers and automatic crash takeovers use fenced ownership leases)

- **Development Quality**
  - Comprehensive linting with golangci-lint
  - Integration and benchmark tests
  - Context-based graceful shutdown
  - Error handling best practices

## Architecture

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   Ingress   │     │  Transform  │     │   Egress    │
│   Plugins   │ ──► │   Plugins   │ ──► │   Plugins   │
└─────────────┘     └─────────────┘     └─────────────┘
       │                   │                   │
       └───────────────────┼───────────────────┘
                           │
                    ┌─────────────┐
                    │   Storage   │
                    │   Backend   │
                    └─────────────┘
```

Live WebRTC calls do not use this pipeline yet. They run in the media worker prototype:

- `pkg/mediaworker` - a minimal WebRTC endpoint built from Pion v4 component libraries (ICE-lite, DTLS-SRTP, RTP) with WHIP-style signaling. It currently echoes the caller's audio and video. A live session can be exported to bytes and resumed on another worker that shares the same UDP socket (a planned handover).
- `pkg/relay` - the relay: one public UDP address that every answer advertises. It routes STUN binding requests by the session owner in `pkg/sessionstore` and forwards everything else to the owning worker without parsing it; workers bind only private sockets behind it.
- `pkg/callharness` - the test seam for media: a Pion WebRTC client plays the caller against an in-process or external topology, can move its call between workers on one socket, or calls through the relay (`make test-harness`).
- `cmd/echo-demo` - a browser echo page for two media workers on one socket, with a "Move call" button (`make demo`, then open http://localhost:9101); `DEMO_FLAGS=-relay` runs one worker behind the relay instead.

The earlier `relais-core` server and its Pion v3 signaling path (`pkg/server`, `pkg/webrtc`) have been retired.

## Scripted audio agent: moves and crashes

```bash
make demo-agent
# Choose another page port when 9101 is occupied:
make demo-agent DEMO_CLUSTER_FLAGS="-http=127.0.0.1:9201"
```

Requires Go 1.26+ and `redis-server`. Open the printed URL in Chrome, click
**Start call**, and keep **Play returned audio** enabled. The worker plays an
owned ten-note scale: one count per 500 ms of consumed audio. The scale rises
through counts 1–9; every tenth count plays a two-tone pulse. A high chime
announces a resume. The page shows the count, phase within the count, last
resume kind, checkpoint age, snapshot age, and duplicated-input flag.
Use **Move call**, **Drain owner**, and **Kill owner (SIGKILL)** to hear and see
it keep its place. Each session has independent state. The agent handles only
audio; the existing page's video echo remains the media yardstick.

`make demo-agent` selects `-agent=demo` on every worker, including replacements.
The worker and cluster launcher otherwise default to `-agent=echo`. The
launcher owns a throwaway Redis on a free port (never 6379) and reaps all child
processes on Ctrl-C. No external audio service, data channel, native codec
installation, or runtime encoder is needed.

### Audio provenance and continuation

The small assets in `pkg/agent/demo/assets/` are sine waves synthesized by
our Go generator. They contain no speech, sampled recording, proprietary TTS
voice, or third-party composition. We use pre-encoded Opus because the #46
interface has no validated pure-Go encoder with serializable codec state.
Regenerate from the repository root with:

```bash
go run ./pkg/agent/demo/generate  # requires ffmpeg with libopus
```

The generator writes raw PCM and uses ffmpeg/libopus with fixed 32 kb/s CELT
frames, a 4 kHz bandwidth ceiling, 20 ms duration, and bit-exact flags.
The common narrowband frame type lets longer packets combine tone and chime
frames without changing codec configuration. It stores only length-prefixed
Opus packets, excluding Ogg metadata. Regeneration is byte-identical with the
same encoder version; different ffmpeg/libopus versions can change the bytes.
Runtime output is deterministic for the committed assets and input sequence.
The demo accepts input durations from 20–120 ms in 20 ms units; shorter input
packets are rejected explicitly. Count and phase advance only on consumed
input, so an idle or dropped input does not advance the script. The resume
chime replaces 100 ms of the scale without changing the script's position.
Save includes the count, tone phase, pending chime phase, consumed input floor,
and resume evidence. The host durably coalesces changed saves at its 100 ms
interval and flushes the accepted queue for a planned move.

On replay, only input already represented by the saved consumed floor is
suppressed. Input after that floor advances the script even inside a flagged
duplicate window: that work is absent from the restored checkpoint. The #46
base does not yet replay caller input (#27); its live takeover flag is false.
Unit tests exercise flagged windows independently. Crash recovery can repeat
uncheckpointed tones and lose input sent during the outage on this base.

### Chrome continuity script

After clicking **Start call**, run this in DevTools on the demo page:

```javascript
await window.relaisDemo.runBaseline(5);
await window.relaisDemo.runMoves(3);
await window.relaisDemo.runDrains(1);
await window.relaisDemo.runKills(3);
const hold = await window.relaisDemo.waitForLongHold();
const results = window.relaisDemo.results();
console.table(results.events.map(e => ({
  kind: e.kind,
  before: e.agentVerdict?.positionBefore?.count,
  restored: e.agentVerdict?.positionRestored?.count,
  after: e.agentVerdict?.positionAfter?.count,
  duplicated: e.agentVerdict?.inputMayBeDuplicated,
  checkpointAgeMs: e.agentVerdict?.checkpointAgeMs,
  snapshotAgeMs: e.agentVerdict?.snapshotAgeMs,
  continuity: e.agentVerdict?.status,
  media: e.verdict.status
})));
window.relaisDemo.saveResults();
await window.relaisDemo.stop();
```

Results version 5 adds `agentMode`, live `agent`, `agentStatusError`, aggregate
`agentVerdict`, and per-event `agentContinuity`/`agentVerdict`. Each continuity
verdict is `pass`, `fail`, or `inconclusive`, independent of the media verdict
and its 60 s hold. The overall event also requires passing agent continuity
when the demo agent is selected. `window.relaisDemo.agentStatus()` reads the
current private worker status through `GET /demo/agent/{id}`. No transport
snapshot or key material reaches the page.

Planned continuity compares the source's **flushed export** count, tone phase,
and consumed progress with the target's initial restored position and its
actual incoming snapshot. A takeover compares the restored position with the
actual checkpoint sent to the target and checks the ages against the control
plane's measured event. Rollback is bounded by **snapshot age plus one 20 ms
packet**, since snapshot age includes copy-to-commit storage delay that
checkpoint write age does not. A fresh post-event observation must show the
agent advancing. Missing source/export/checkpoint/status evidence remains
inconclusive. This proves application state continuity; browser playback and
the existing media verdict prove separate aspects of the caller experience.

Local checks:

```bash
node --test cmd/relais-demo/*.test.cjs
# With your own throwaway Redis on a non-6379 port:
RELAIS_TEST_REDIS_ADDR=127.0.0.1:16379 RELAIS_TEST_REDIS_REQUIRE=1 \
  go test -race -count=1 ./pkg/agent/demo/... ./cmd/relais-worker ./cmd/relais-demo
make build
RELAIS_DEMO_SMOKE=1 go test -race -count=1 -v \
  -run '^TestDemoAgentExternalSmoke$' ./cmd/relais-demo
```

The opt-in smoke starts its own Redis and real workers, uses the external Pion
caller, and runs the page's continuity function on one move, one drain, and
three SIGKILL events. It writes `bin/w47-agent-process-evidence.json`. It does
not replace a Chrome run, audible listening check, recording, or the browser's
60 s hold.

## Browser cluster demo: move, drain and kill

Run `make demo-cluster` with Go 1.26+ and `redis-server` on PATH, then open
[the demo](http://localhost:9101/?source=test&autostart=0) in current Chrome.
Click **Start call** to publish a canvas test pattern and synthetic tone, or
select **Camera with counter and tone (720p)** to publish your camera. The page plays the
echoed video and audio; use headphones for the synthetic tone.
The Start click also unlocks Chrome's audio autoplay policy. Keep the tab
visible for presentation-gap measurements. This demo requires no bundler or CDN.

The launcher starts its own throwaway Redis, one relay, one control plane and
**three workers as separate processes**. By default it binds HTTP and media to loopback
and refuses Redis port **6379**. `make demo` continues to run the older
shared-socket page. The two demos use the same default HTTP port, so run one at
a time or override the cluster address:

```bash
make demo-cluster
make demo-cluster DEMO_CLUSTER_FLAGS="-http=127.0.0.1:9201"
# Optional: an explicitly selected, dedicated Redis; never port 6379
make demo-cluster DEMO_CLUSTER_FLAGS="-redis=127.0.0.1:16379"
```

- **Move call** asks the existing control-plane move endpoint to move the
  live call to another eligible worker.
- **Drain owner** calls the existing drain endpoint, moves all calls off the
  owning worker, stops that process, and starts a fresh replacement.
- **Kill owner** finds this call's owner through control-plane status, sends
  its owned process group SIGKILL, waits for the real heartbeat detector and
  takeover, then starts a fresh replacement. It never tells the control plane
  that the worker died. Each successful drain or kill restores a pool of three.

Fresh numeric replacement names avoid reviving an operator-drained registration.
Worker UDP addresses are never reused within the launcher lifetime.
The control binary's opt-in `-demo-register` adds a loopback-only
`POST /demo/register` bootstrap hook; it is disabled by default. Historical
registrations remain in control-plane status, while `worker_pids` lists only
live launcher-owned workers. No media or lease policy is changed.

`GET /demo/status` proxies control-plane status and adds `worker_pids`, the
`pool_size`, `expected_pool_size`, the relay media address, and dedicated Redis address. `POST /demo/kill` and
`POST /demo/drain` take JSON `{"id":"call-id"}`; an omitted ID is accepted only
when exactly one call exists. The launcher also forwards WHIP `/calls`, call
moves, worker drains and `/status` for an external Pion caller. Replacement
work belongs to the launcher lifetime even if the browser disconnects.
Ctrl-C stops and reaps all owned children and the throwaway Redis; a second
signal forces cleanup. Explicitly supplied Redis instances remain running.
Logs remain in `bin/demo-cluster-*/`.

For real-camera and Android runs, follow the
[real-device checklist](docs/dev/real-device-checklist.md). Opt in with
`-local-network=192.168.1.50` (your local IPv4 address) to serve the page over HTTPS and
bind relay media to that IP. The launcher prints the URL and SHA-256
certificate fingerprint in plain hex and Chrome's format; compare it with the
browser's certificate viewer. The full printed URL selects camera and includes
a per-launch token required for non-GET call controls. Private APIs stay on
loopback. The page has a source
menu, a 10-move/10-kill checklist button, and **Save results** for version 5 JSON.
Both sources use the same counter reader. Each takeover records the time to a
first live frame after observed content recovery, measured conservatively from
kill-request issuance; moves and drains report `null` for this field. The
checklist explains the clock and replay limits. IPv6 local-network mode is
rejected because browser media did not connect in verification.

The page shows connection/ICE transitions, ownership, worker pool size and event
results. A short pool is a visible warning. Replacement startup is retried once;
failed startup children are stopped. A lost registration reply is confirmed
through control-plane status. Confirmed registrations keep their worker. If
confirmation is unavailable, the launcher keeps ownership, reports
`registration_error` and suppresses automatic retry. Later status reads clear the warning once
registration is confirmed. HTTP Host values are limited to localhost,
127.0.0.1 and [::1] in default mode, or the exact selected IP in local-network
mode. Mutations require a matching HTTP or HTTPS Origin when supplied. Throwaway Redis readiness comes from its own process log.

Both pattern and camera video carry a 16-bit counter in large black/white blocks. The receiver
reads those blocks after VP8 decoding. An inverse row detects unreadable blocks;
the reader follows receiver resolution changes. Each primary content interval
runs from the previous advancing frame's presentation time to the current
frame's pixel observation time, including an open freeze. Delayed callbacks
can enlarge gaps but cannot shorten them. Older callback metadata cannot
report a newer counter snapshot as early recovery. Replayed or old content
does not count as recovery. At issue, the page records the sender counter.
First new content must exceed that counter, so content already in flight
cannot claim recovery. `contentResumedMs` separately reports recovery from the
largest gap ending after issue, even if a larger gap precedes issue (unverified
if still open). Presentation gaps remain a secondary metric. Camera counters
advance only when a camera frame arrives, and use the same reader and verdicts
as the pattern source. Source draw timestamps provide the observed cadence;
only source delay beyond normal cadence joins starvation diagnostics. A
baseline is excluded from calibration when that excess overlaps its largest
video gap.
Audio judges total `concealedSamples` at the receiver sample rate. Non-silent
concealment remains a diagnostic. `concealmentEvents` bounds bursts when more
than one event occurs between stats. `totalSamplesReceived` must advance by at
least 90% of elapsed time times sample rate over the counter coverage interval;
otherwise audio playout is inconclusive even with zero concealment. Missing
counters remain `"unverified"`. Packet timestamp deltas remain diagnostics.
Stats use RTCStats timestamps. Windows close after these timestamps cover the
two-second settle; audio records its counter coverage boundaries.

Keep the tab and remote video visible and unobstructed. Visibility changes and
IntersectionObserver record hidden periods; any overlapping event or hold is
`invalid` with reason `page hidden`. Browser visibility tracking cannot detect
every form of window occlusion. Starvation, long tasks and unreadable counters
are diagnostics when measured gaps pass. A failing video gap becomes
inconclusive only if the union of overlapping diagnostic time could bring
its largest interval below the event threshold. A small overlap cannot hide a
larger failure. The hold uses the same rule with its 2 s threshold. This
adjustment cannot turn a measured failure into a pass. Audio concealment
counters remain cumulative and do not use this adjustment.

Both counter rows use one draw into a 16×2 canvas. Unreadable values are logged
at most once a second, with the 32 raw R-channel values. Results render at
most once a second. `runBaseline()` defaults to five null windows. `noiseFloor` reports
median/max gaps over the last `baselineCount` valid baselines and counts
excluded windows. A baseline is valid only when it passes and both gaps are
below 100 ms; freezes are excluded. New valid baselines let the floor recover.
Raw gaps below the threshold pass regardless of the floor; the floor is never subtracted. For move and drain gap
failures, fewer than the configured number of valid baselines makes the verdict
inconclusive (`too few valid baselines`). With enough baselines, either floor
maximum at or above threshold × headroom ratio makes a gap failure inconclusive
(`noise floor near threshold`). A low floor permits an authoritative gap failure.
Gross failures remain FAIL regardless of the gate: a proven gap at least the
threshold plus that media's floor maximum cannot be explained by the floor.
With no floor, use one recorded source frame interval as the noise allowance;
with neither available, retain the raw failure. Audio uses the proven lower
burst bound for this check, never an uncertain upper bound.
Configure both with `start({source:'test', baselineCount:5,
headroomRatio:0.8})`; the ratio must be greater than zero and at most one.
Kills and the 60 s hold need no baseline gate. Action, connection and negotiation
failures remain failures regardless of the floor.

`excessOverFloorMs.video` and `.audio` each report nonnegative `medianMs` and
`maxMs` excesses. These are diagnostics and never adjust thresholds.
`withinBaselineJitter` is a boolean: the video gap is at or below the video floor
max plus one source frame interval (1000/30 ms for the test pattern, or the
camera track's frame rate). `frameIntervalMs` records that interval. When the
floor or interval is unavailable, `baselineJitterAvailable` and
`withinBaselineJitter` are false.

```javascript
// Click Start call first (or unlock Chrome audio with a page click).
await window.relaisDemo.start({source: 'test'});
// Start long runs without holding a browser tool call open.
window.demoRun = window.relaisDemo.runBaseline(5);
// Poll in separate calls until running is false. Read results after each run.
window.relaisDemo.results(); // {running: true/false, baselines, noiseFloor, ...}
// After baseline completes:
window.demoRun = window.relaisDemo.runMoves(10, 3000);
// After moves complete, optional: runKills(10, 3000) or runDrains(10, 3000).
window.relaisDemo.results();
// Poll longHold.status until it is no longer pending, then save results.
// Optional interactive wait: await window.relaisDemo.waitForLongHold();
await window.relaisDemo.stop();
```

Run helpers serialize actions. Each window starts at the later of a 1 s
lookback and the previous window's end; it ends 2 s after the response. The
60 s hold starts after the last window. New actions supersede that hold.
The hold retains running maximum content/concealment gaps and freeze deltas
for its whole duration. A transient freeze cannot disappear from the hold
result merely because media recovers at the end.

`results()` is schema version 5. Each event has `windowVerdict` and `verdict`,
both `{status: "pass" | "fail" | "inconclusive" | "invalid", reasons: string[]}`.
`windowVerdict` covers the event window; `verdict` also requires the final hold.
Successful windows awaiting a hold are inconclusive. `windowPass` and `pass`
are true for pass, false for fail, null for inconclusive/invalid. Top-level
`verdict` and `pass` aggregate the event verdicts. `longHold` has `status`
(pending/superseded before completion, then the four verdict values), `verdict`
after completion, `runningMax: {video, audio}`, `largestGaps`,
`gapDiagnostics`, `freezeDelta` and
`freezeDurationDelta`. Planned gaps must be below 100 ms; kill gaps below 2 s.
The hold requires continued media below 2 s, with no connection change, ICE
restart or renegotiation. Baselines have window verdicts and no hold requirement.

Browser SRTP failure counts remain unverified. Worker logs record the first
failure and powers of two outside the session lock, plus a final count on normal
close/handover. SIGKILL cannot emit a final count. Browser acceptance recordings
remain separate from native smoke verification. Safari is outside scope.

For the opt-in headless smoke (external Pion caller, one move/drain, three kills and
post-kill echo/full VP8 decode; **not** browser presentation or the 60 s hold):

```bash
make build
RELAIS_TEST_REDIS_ADDR=127.0.0.1:1 RELAIS_DEMO_SMOKE=1 \
  go test -race -count=1 -run TestDemoExternalSmoke -v ./cmd/relais-demo
node --test cmd/relais-demo/metrics.test.cjs
```

## Real processes: WebRTC you can kill -9

`make crash-run` builds and starts `relais-relay`, `relais-control` and two
copies of `relais-worker` as separate OS processes. A real Pion caller sends
Opus audio and VP8 video continuously through the relay. The driver reads
`GET /status`, finds the call's owning worker, sends its PID **SIGKILL**, and
keeps the same call running for 60 seconds after automatic takeover. Recovery
uses missed 100 ms heartbeats (400 ms death threshold), encrypted snapshots and
fenced leases in a shared Redis session store. It never requests an ICE restart
or a second offer/answer exchange.

Prerequisites: Go 1.26+, `redis-server` and `ffmpeg` on PATH. On macOS, Redis
and ffmpeg can be installed with `brew install redis ffmpeg`. This run is
part of the full CI matrix (nightly, pushes to `main`, and manual runs). It stays
out of PR CI.

```bash
make crash-run                              # ten consecutive acceptance runs
make crash-run CRASH_FLAGS="-runs=1 -verbose" # one full run and caller report
make crash-run CRASH_FLAGS="-runs=1 -after=3s" # developer smoke test only
```

The default takes about eleven minutes. Each run requires a media gap below
2 seconds, zero decryption failures after the first resumed packet, zero ICE
restarts/reconnects or renegotiations, resumed audio and video, clean full VP8
decode with ffmpeg, and a connected caller with answered consent checks for
at least 60 seconds after takeover. It exits nonzero on failure. The summary
reports first decoded video **and first decoded live video from SIGKILL**, plus
detection time from SIGKILL. Total decryption failures are shown separately from
those after resume. `-after` below 60 seconds explicitly marks the consent
acceptance check `NOT-RUN`.

The driver creates its own Redis on a free loopback port, with persistence
turned off (`--save '' --appendonly no`) and a working directory under `bin/`.
It creates a fresh shared encryption key and a unique Redis namespace for each
run, and stops every child and its Redis on completion, failure or SIGINT/SIGTERM.
Process logs stay under `bin/crash-run-*/`. To use an existing **dedicated**
instance, set `RELAIS_REDIS_ADDR=127.0.0.1:16379`, or pass `-redis=...`; it will
not stop that instance or flush its data. These binaries and this target refuse
port **6379** and have no Redis default address.

Example summary from ten local acceptance runs (Go 1.26.9, Redis 7.0.11,
ffmpeg 7.1.1 on macOS arm64; timings vary):

```text
run  gap_ms  decrypt(total/after)  reconnects  renegotiations  decoded_ms  live_ms  detection_ms  result
  1   400.2        0/0             0          0            401.1      401.1    374.4       PASS
  2   400.0        0/0             0          0            401.9      401.9    380.1       PASS
  3   402.1        0/0             0          0            404.0      404.0    380.6       PASS
  4   393.2        0/0             0          0            393.5      393.5    368.2       PASS
  5   400.1        0/0             0          0            401.2      401.2    377.3       PASS
  6   399.8        0/0             0          0            400.3      400.3    376.6       PASS
  7   399.9        0/0             0          0            401.1      401.1    376.2       PASS
  8   400.4        0/0             0          0            402.2      402.2    380.8       PASS
  9   399.5        0/0             0          0            399.8      399.8    375.8       PASS
 10   400.0        0/0             0          0            402.3      402.3    380.6       PASS
PASS: 10 consecutive real-process SIGKILL trials
```

External call-harness placement uses literal registration names: the zero value
`CallOptions{}` pins to `"0"`, so that registration must exist. Use
`CallOptions{Worker: -1}` for unpinned control-plane placement. Owned topologies
keep their existing worker-index semantics.

`make build` also produces the individual binaries for manual process runs.
All three require `-redis` (or `RELAIS_REDIS_ADDR`), with an optional shared
`-redis-prefix` / `RELAIS_REDIS_PREFIX`. Only workers and the control plane need
the same `RELAIS_SESSIONSTORE_KEY` (32 bytes encoded as hex or base64). The relay
uses an ownership-only store with no snapshot key or decryption API; start it
without that environment variable (`env -u RELAIS_SESSIONSTORE_KEY ...`). The
driver strips the key from the relay environment. Run each with `-h` for flags:

- `relais-relay`: `-media` (public UDP), `-leg` (worker UDP), `-http` (private
  control HTTP), `-hold-timeout` (default 3 s).
- `relais-worker`: `-name`, `-media` (private UDP), `-http`, `-relay-leg`,
  `-relay-media`, `-control` (control-plane HTTP URL), `-drain-timeout` (5 s).
- `relais-control`: `-http`, `-relay` (relay control URL), and a repeated
  `-worker name=http://worker-address` for each worker. Start the relay and
  worker HTTP listeners before the control plane. Workers retry heartbeats
  while it starts. Each process prints one JSON readiness line with its PID
  and bound addresses; ephemeral loopback ports are the defaults.

The loopback control listener serves WHIP-style `POST /calls`, `DELETE /calls/{id}`,
move/drain endpoints and `GET /status`, plus a private heartbeat endpoint. The
worker API wraps create/end/export/resume/status; the relay API wraps worker
registration/removal and move/forget/hold/release. Private HTTP listeners refuse
non-loopback binds. These unauthenticated APIs are a local prototype limit:
loopback is not a security boundary on a multi-user host. Any local user able
to connect can invoke mutations and read worker snapshot key material.
Snapshots containing key material travel over this private HTTP leg.
Remote requests have a two-second client bound and never automatically retry
media mutations. Resume uses the caller's coordination context when provided.
A lost transport reply is an uncertain outcome. Takeover retries the same
resume payload and lease; an already running matching tenure acknowledges it
without applying another margin. If that registration dies, drains or is
replaced, a different eligible target gets a new epoch and resumes from the
latest stored counters with a fresh margin. An uncertain export falls back to a fenced
single-call crash takeover from the stored snapshot. Planned rollback after
an uncertain resume uses crash margins. The relay private move API requires
`from`, unless `repair: true` explicitly authorizes repairing every authenticated
route of that session when its prior route is uncertain. A missing or empty
`from` without the flag returns HTTP 400.

A returning worker receives a rejoin token only after recovery of its old
leases settles. It fences and drops its old sessions before echoing that token;
an ordinary heartbeat cannot acknowledge rejoin. Repeating the same token is
safe if its acknowledgement response was lost. Placement and lease listing do
not hold the plane lock across Redis I/O; eligibility and reservations are
rechecked under the lock after listing.

Planned handovers resend the same UDP drain barrier every **75 ms** until its
ACK, without extending the **1 s** barrier deadline. After ACK, an abandoned
hold auto-releases in **3 s**, below the shortest 5 s caller connectivity window.
Healthy local coordination finishes in milliseconds. Slow coordination and
worst-case rollback can exceed the 3 s backstop and report an expired hold. It bounds packet holding, and does
not make a crashed control plane durably resumable.

On SIGTERM (or SIGINT), `relais-worker` asks the control plane to drain its
calls through the existing planned move path. Its HTTP listener and heartbeats
remain live until sessions are gone or `-drain-timeout` expires (default 5 s).
A second SIGINT or SIGTERM during drain uses the default immediate exit.
It then closes. An unreachable control plane or failed drain is logged; the
worker closes remaining sessions after the bounded attempt. Verify this
operator path with `make crash-run CRASH_FLAGS="-runs 1 -after 3s -sigterm -verbose"`;
this short run checks the planned gap below 100 ms, not the 60 s crash window.

Each driver's Redis, relay, worker and control child has its own process group.
Cleanup sends group signals only while the child leader remains unreaped;
reaping and group signals share a lock so a reused PID cannot be targeted.
Normal completion, errors, SIGINT and SIGTERM terminate live child groups.
The driver keeps signal handling active during cleanup: the first SIGINT or
SIGTERM cancels the run; a second kills and reaps every owned child group,
including its throwaway Redis, before exiting. This driver policy differs from
the worker’s immediate second-signal exit during drain.

macOS has no `Pdeathsig`: if the driver itself is SIGKILLed, cleanup
cannot run and children (including Redis) survive. Find the driver’s printed
`bin/crash-run-*` directory, take the child PIDs from readiness lines and the
Redis startup log, and verify each with `lsof -p <pid>` (command and `cwd` must
match that run). Terminate only those groups with `kill -TERM -- -<child-pid>`;
use `kill -KILL -- -<child-pid>` only for a group that does not stop. Redis uses
no persistence. Do not target another run or the reserved Redis on port 6379.

The frame cache is still memory-local to each worker process. Cross-process
replay misses and requests a live keyframe (PLI); Redis frame caching belongs
to #12. Session snapshots have an explicit format version. A mixed-version
rollout must first deploy decoders accepting **both old and new snapshot
versions**, then upgrade writers, and retain old-version decoding until old
snapshots are gone. The current single-version decoder does not provide that
rollout compatibility by itself.

## Getting Started

### Prerequisites

- Go 1.26 or higher
- Redis (optional, for distributed storage)
- golangci-lint (for development)

### Installation

```bash
# Clone the repository
git clone https://github.com/yourusername/relais.git
cd relais

# Install dependencies
make deps

# Build plugin runners and media-process binaries
make build
```

### Running Tests

```bash
# Run all tests
make test

# Run the call harness (audio+video echo calls, race detector)
make test-harness

# Run benchmarks
make bench

# Generate coverage report
make coverage
```

PR checks run in parallel: lint, build, vet, race unit tests with Redis,
the short memory call harness, a Redis uncertainty regression under race,
and a strict memory timing smoke (20 planned moves, 20 hard kills, and planned
sequence wrap). The goal is about 10 minutes
of wall time. The full race harness and timing gates on both stores, benchmarks,
and ten real-process SIGKILL trials run nightly at 08:00 UTC and on every push
to `main`. Trigger them on demand with **Actions → CI → Run workflow**
(`workflow_dispatch`). The optional Redis Cluster gate is unchanged.

Local equivalents (ffmpeg required for the harness; use a dedicated Redis):

```bash
export RELAIS_TEST_REDIS_ADDR=127.0.0.1:16379
export RELAIS_TEST_REDIS_REQUIRE=1
export RELAIS_HARNESS_REQUIRE_FFMPEG=1
make test-unit
make test-harness-short
go test -race -v -count=1 -timeout 5m -run '^TestRelayUncertainTransferSurvives$' ./pkg/callharness/
make test-harness-smoke
make test-full # full in-process race/timing matrix with both stores
make crash-run # ten real-process trials, also requires redis-server
```

Short mode uses 5 s baseline calls. Longer fault scenarios retain their fixed
duration. The timing smoke retains full consent windows and product thresholds.
Both short harness and timing smoke clear the Redis test address and disable
its requirement, so all Redis subtests skip in those targets. PR CI restores
Redis uncertainty coverage in a separate race step. Short mode also skips
`TestPlannedHandoverKeepsConsent`; the full matrix runs it.
`make bench` always runs memory benchmarks. Its Redis variants skip unless
`RELAIS_TEST_REDIS_ADDR` names a dedicated, disposable Redis instance. Redis
benchmarks use random prefixes and clean up their own keys. Required-Redis mode
fails on missing or unreachable endpoints. The `build` job is an aggregate
gate; making it required in GitHub settings is a separate admin decision.
Superseded PR runs are cancelled; full-matrix runs are not cancelled by this
policy. Failed crash runs retain `bin/crash-run-*` as a workflow artifact.

### Development

```bash
# Format code
make fmt

# Install/update linting tools
make lint-install

# Run comprehensive linter (golangci-lint)
make lint

# Run the media worker echo demo on http://localhost:9101
make demo
```

### Configuration

Configuration is loaded from environment variables:

```env
RELAIS_STORAGE_TYPE=redis
RELAIS_STORAGE_REDIS_URL=localhost:6379
RELAIS_LOGGING_LEVEL=info
```

#### Redis Streams and Consumer Groups

Relais can optionally mirror frames to Redis Streams for low-latency tailing and support Redis Stream Consumer Groups for distributed consumption.

Enable and configure via environment variables (prefix RELAIS_):

```env
# Enable writing frames to Redis Streams and approximate trimming length
RELAIS_STORAGE_REDIS_STREAMS_ENABLE=true
RELAIS_STORAGE_REDIS_STREAMS_MAXLEN=10000

# Enable Consumer Groups and configure defaults
RELAIS_STORAGE_REDIS_STREAMS_GROUP_ENABLE=true
RELAIS_STORAGE_REDIS_STREAMS_GROUP=relais            # group name (default: relais)
RELAIS_STORAGE_REDIS_STREAMS_CONSUMER=               # optional default consumer name

# Redis connection (single node)
RELAIS_STORAGE_REDIS_URL=localhost:6379
RELAIS_STORAGE_REDIS_PASSWORD=
RELAIS_STORAGE_REDIS_DB=0
RELAIS_STORAGE_REDIS_PREFIX=

# Redis Cluster (optional)
RELAIS_STORAGE_REDIS_CLUSTER_ENABLE=false
RELAIS_STORAGE_REDIS_CLUSTER_ADDRS=                  # e.g. host1:6379,host2:6379,host3:6379
```

Notes:

- These settings configure `RedisStorage` (`EnableStreams`, `EnableStreamGroups`, `SetRetention`). The retired `relais-core` applied them; the plugin runners do not apply them yet.
- The group is lazily created on first read using `XGROUP CREATE MKSTREAM` (auto-creation), so you don't need a separate migration step.
- `ReadStreamGroup()` uses non-blocking reads by default. Set a positive block duration to use blocking reads.
- For per-track consumption, `ReadTrackStreamGroup()` reads from track-specific streams that mirror session events by `TrackID`.

## Plugin Development

### Creating a New Plugin

1. Implement one of the plugin interfaces:
   - `IngressPlugin`
   - `EgressPlugin`
   - `TransformPlugin`

2. Register your plugin:

```go
func init() {
    registry.Register(plugins.PluginTypeIngress, "my-plugin", NewMyPlugin)
}
```

### Example Plugin

```go
type MyPlugin struct {
    // Plugin state
}

func (p *MyPlugin) Initialize(ctx context.Context, config map[string]interface{}) error {
    // Initialize plugin
    return nil
}

func (p *MyPlugin) Run(ctx context.Context, store storage.Storage) error {
    // Plugin logic
    return nil
}

func (p *MyPlugin) Stop() error {
    // Cleanup
    return nil
}

### Testing & Quality Assurance

The project includes comprehensive testing and quality tools:

```bash
# Run all tests (integration and unit)
make test

# Run benchmarks
make bench

# Run with profiling
make profile

# Run linting (50+ checks including errcheck, unused, typecheck)
make lint

# Generate test coverage report
make coverage
```

#### Running Redis Streams Integration Tests

The Redis integration tests skip unless `RELAIS_TEST_REDIS_ADDR` is set.
Use a reachable **dedicated, disposable** Redis instance. The stream and group
tests write data to it. There is no default connection to port 6379:

```bash
export RELAIS_TEST_REDIS_ADDR=127.0.0.1:16379
```

Run only the Stream Group focused tests:

```bash
go test -run StreamGroup -v -count=1 ./...
```

Makefile shortcuts for convenience:

```bash
# All Streams-related tests (non-cluster) in storage
make test-streams

# Only Streams Consumer Group tests (non-cluster)
make test-stream-groups

# Redis Cluster tests (requires RELAIS_TEST_REDIS_CLUSTER_ADDRS)
make test-cluster
```

Cluster tests (optional) require a Redis Cluster and the following environment variable with a comma-separated list of node addresses:

```bash
export RELAIS_TEST_REDIS_CLUSTER_ADDRS="127.0.0.1:7000,127.0.0.1:7001,127.0.0.1:7002"
go test -run Cluster -v -count=1 ./...
```

What the tests cover:

- Consumer group read and acknowledgment (`XREADGROUP`, `XACK`).
- Multi-consumer distribution without duplicates.
- Redelivery and claiming pending entries (`XPENDING`, `XCLAIM`).
- Edge case for `XCLAIM` MinIdle (too-early claim returns 0; after MinIdle succeeds).
- Auto-creation of consumer groups on first read.

### Metrics Guide

Relais defines its Prometheus metrics in `pkg/metrics`. No binary serves `GET /metrics` at the moment: the retired `relais-core` server did. A process that needs scraping can mount `promhttp.Handler()` from `github.com/prometheus/client_golang/prometheus/promhttp`.

Key metric families:

- Redis Streams consumer groups (from `pkg/metrics/metrics.go`):
  - `relais_redis_group_reads_total{stream=session|track}`
  - `relais_redis_group_read_messages_total{stream=session|track}`
  - `relais_redis_group_acks_total{stream=session|track}`
  - `relais_redis_group_ack_messages_total{stream=session|track}`
  - `relais_redis_group_ensure_total{result=created|exists|error}`
  - `relais_redis_group_claims_total{result=success|empty|error}`
  - `relais_redis_group_claim_messages_total`

- Redis operation reliability:
  - `relais_redis_op_seconds{op}` (histogram)
  - `relais_redis_errors_total{op,type}`
  - `relais_redis_retries_total{op}`

- Egress tailer (no current emitter; the retired `relais-core` recorded these):
  - `relais_egress_reads_total{mode=session|track}`
  - `relais_egress_empty_polls_total`
  - `relais_egress_errors_total{stage}`
  - `relais_egress_read_seconds` (histogram)

Prometheus scrape configuration example:

```yaml
scrape_configs:
  - job_name: 'relais'
    metrics_path: /metrics
    static_configs:
      - targets: ['relais-host:8080']
```

Operational notes:

- Consumer groups are created lazily; monitor `relais_redis_group_ensure_total{result="error"}` for provisioning issues.
- Claims telemetry comes from `ClaimPending()` in `pkg/storage/redis.go`; monitor `relais_redis_group_claims_total{result}` and `relais_redis_group_claim_messages_total` for redelivery behavior.

#### Dashboards and Alerts

- Grafana dashboard: `docs/observability/grafana/redis-groups.json`
- Prometheus alert rules: `docs/observability/alerts/redis-groups.yml`
- Observability setup guide: `docs/observability/README.md`

### Key Metrics
- Ingress throughput (frames/sec)
- Storage read/write performance  
- Maximum concurrent clients
- Transform plugin processing time
- Code coverage and quality scores

### Code Quality
- **golangci-lint**: 50+ linters including errcheck, unused, typecheck
- **Integration tests**: End-to-end plugin pipeline testing
- **Benchmark tests**: Performance regression detection
- **Race condition detection**: Context-based error handling

## Contributing

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Install development tools (`make lint-install`)
4. Make your changes with proper error handling
5. Run tests and linting (`make test && make lint`)
6. Commit your changes (`git commit -m 'Add amazing feature'`)
7. Push to the branch (`git push origin feature/amazing-feature`)
8. Open a Pull Request

### Code Standards
- All code must pass `make lint` (golangci-lint)
- Integration tests required for new plugins
- Proper error handling and context cancellation
- No race conditions in concurrent code

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
