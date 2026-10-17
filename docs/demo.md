# Try the browser demos

[Back to Relais](../README.md). Build first with `make build`; install Go 1.26+
and `redis-server` on PATH (`brew install redis` or `sudo apt install redis-server`).
The multi-process demo needs Redis and never uses port 6379. Current Chrome is
the tested browser; Safari is untested.

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

For a source-built custom worker, the launcher accepts `-worker-bin` and a
custom `-agent` name, including replacement workers. See
[Hear it in your browser](build-an-agent.md#hear-it-in-your-browser) for the
keyless delayed-loopback command. Its delayed audio does not satisfy echo gap
or tone/count checks; the page marks custom application continuity inconclusive.

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
when exactly one call exists. The launcher also forwards call creation, call moves, worker drains and
`/status`. Creating a call requires `X-Relais-Demo-Launch-ID` with the launch
identifier; it is not an unrestricted external signaling endpoint. Use
[relais-signal](connect-an-app.md) for an app or an external caller. Replacement
work belongs to the launcher lifetime even if the browser disconnects.
Ctrl-C stops and reaps all owned children and the throwaway Redis; a second
signal forces cleanup. Explicitly supplied Redis instances remain running.
Logs remain in `bin/demo-cluster-*/`.

For real-camera and Android runs, follow the
[real-device checklist](dev/real-device-checklist.md). Opt in with
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

## Chrome continuity script

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

## Older shared-socket demo

`make demo` runs two in-process workers on one UDP socket, without Redis.
Its Move call button tests planned handover, without process crashes.
`make demo DEMO_FLAGS="-relay"` instead runs one worker behind a relay; it has
no move control. This older demo uses the same default page port.

## Measurement details

SRTP is encrypted real-time media. VP8 is the video codec used here; Opus is the
audio codec. ICE is WebRTC network-path setup. A verdict marked inconclusive
lacks enough evidence; an invalid verdict means the measurement conditions failed.

These details explain contributor evidence, not extra setup steps. All timings
and thresholds below describe the demo or its checks. Published results and
their conditions live in [the guarantee matrix](guarantees.md).

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
duplicate window: that work is absent from the restored checkpoint. Relay takeover now replays selected buffered caller input and supplies duplicate
windows before agent callbacks start. Crash recovery can repeat uncheckpointed
tones. Buffer loss, expiry, overflow, and agent queue drops can still lose input.
See [the agent guarantee](guarantees.md#agent-continuity) and
[ADR 0006](adr/0006-agent-interface.md#resume-and-replay).

### Agent continuity verdicts
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

### Browser media measurements
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

