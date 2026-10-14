# Real-device demo checklist

Run desktop Chrome with a real camera, then Android Chrome on the same local
network. Each measurement run takes about three minutes, plus setup. Keep the
echo video visible and the device awake throughout. Use headphones for the tone.

## Start the local-network demo

Requires Go 1.26+, `redis-server`, and desktop Chrome. Nothing is installed
machine-wide. The launcher builds and starts its own relay, control plane,
three workers, and throwaway Redis. It never uses Redis port 6379.

Find the Mac's local Wi-Fi IP in System Settings → Wi-Fi → Details → TCP/IP
(for example `192.168.1.50`). From the repository root, replace the example IP:

```sh
make demo-cluster DEMO_CLUSTER_FLAGS="-http=127.0.0.1:9201 -local-network=192.168.1.50"
```

The page and relay UDP media bind only to that IP. Private control, worker,
relay HTTP APIs, relay worker leg, and Redis remain on `127.0.0.1`.
The terminal prints a warning, an HTTPS URL, and the certificate SHA-256
fingerprint in plain hex and Chrome's uppercase, colon-separated format.
Call controls require a random per-launch token from the printed URL. The page
sends it as `X-Relais-Demo-Token` on every non-GET request. Keep the full URL
private; it grants demo controls. Use a trusted network. Allow the owned demo processes through the Mac firewall if
prompted. Do not change router settings or open ports to the internet.

The self-signed ECDSA certificate and key are generated in memory at startup.
They expire after 24 hours and change every restart. The certificate starts
one hour before launch to tolerate device clock skew. Use the full printed
literal-IP URL, such as
`https://192.168.1.50:9201/?source=camera&autostart=0#token=[launch token]`.
The fragment token stays out of page requests and Referer headers. It changes
on restart. Opening a URL without it can load the page, but call controls return
403. Local-network URLs select camera; loopback-only URLs keep the pattern.
`-local-network` accepts a literal private or loopback IPv4 address, not a
wildcard or DNS name. IPv6 is rejected: HTTPS worked in the browser check,
but IPv6 media did not connect. Without the flag the demo remains HTTP and
media on loopback.

`make demo` runs the older shared-socket echo page. Use `make demo-cluster` for
this checklist, which requires separate processes and kill controls.

## Attach to an existing cluster

Run the page server on the operator's machine. It connects to an existing
control plane through a local forwarded port. Attach mode starts no relay,
control, workers or Redis. Stopping the page server leaves that cluster running.
The cluster owner supplies its lifecycle hook and tunnel separately.

Build with `make build`, then select free loopback ports for the page and
forwarded control. For example, with control forwarded to `localhost:19402`:

```sh
./bin/relais-demo -http=127.0.0.1:19401 \
  -attach-control=http://localhost:19402 \
  -fault-hook=/path/to/fault-hook -relay-buffer=on \
  -results-dir=bin/attached-results
```

`-attach-control` cannot be combined with `-redis` or `-local-network`. `-http`
must remain a literal loopback address. Open the printed
`http://localhost:<page-port>/?source=camera&autostart=0#token=…` URL in desktop
Chrome. It selects the camera without starting a call. The page captures the
per-launch token, then removes the fragment from the address bar. Keep the
printed URL private. Attach mode retains the token in `sessionStorage` for this
tab and launch, so reloading the page keeps controls working. A new launch
requires its new printed URL. Local-network mode keeps the fragment in the URL
as before. Host and Origin checks also remain active.

The current `internal/privateapi` and control plane have **no credential
HTTP authentication**. Keep control private through the tunnel. Omit
`-attach-auth-file`. The flag checks a supplied regular file for mode 0600 or
stricter and reads it once, but rejects non-empty credentials. It does not
invent a Bearer or Basic authentication format. No credential is logged.

The hook is optional for moves and required for kills. The page server executes
it directly, with no shell, and passes the server's environment unchanged.
Launch the server with only the environment the hook needs. The hook must keep
secrets out of stdout and stderr. Failure receipts retain at most 4 KiB of
stderr after redacting a 64 KiB raw tail and dropping the first partial line.
Credential fields and secret environment values of at least eight characters
are redacted. Each hook runs in its own process group. Cancellation kills that
whole group, including subprocesses. Remote work must be idempotent or
cancellable because killing the local hook cannot undo a remote action.
Hooks must be non-interactive (for example `ssh -o BatchMode=yes`): a prompt
from the background process group stalls until the action timeout.
The hook protocol is:

```text
<executable> kill --worker <current-owner> --session <session-id>
{"ok":true,"action":"kill","worker":"<current-owner>","pid":123,"killedAt":"2026-10-13T12:00:00.123456789Z"}

<executable> replace --worker <old-owner>
{"ok":true,"action":"replace","worker":"<old-owner>","replacement":"<new-name>","readyAt":"2026-10-13T12:00:01Z"}
```

Each invocation must exit 0 and emit exactly one JSON object, with no extra
stdout. Use a UTC RFC3339Nano `killedAt` for the actual kill. `readyAt` accepts
RFC3339Nano. Non-zero exit, `ok:false`, invalid output or timeout fails the
action. `-action-timeout=0` (the default) uses 90 seconds in attach mode and
15 seconds in local mode. A positive value overrides the mode default. The deadline covers
the complete kill, takeover, replacement and pool check. Hook stdout is bounded
to 64 KiB.

The page waits for control's completed takeover before invoking `replace`.
It then waits for the named replacement to be healthy and registered, and for
the pool to return to the healthy count observed at page startup. Two workers
support the checklist: moves choose the other worker and kills target the
current owner. Drain returns `unsupported in attach mode`. Status comes from
control registrations and heartbeat state; no local PIDs are inferred. The
hook must stamp `killedAt` at the actual signal time. That value is recorded as
`event.server.at`. Takeover matching uses a baseline of control's events before
the hook runs; it accepts a new terminal event for the session and killed owner.
It never compares hook and control clocks. Timeout errors include the last
matching candidate events.

Attach mode preserves the worker's SDP. The operator's network must reach the
relay's advertised UDP media port. Forwarding control alone does not forward
media. Attach mode requires `-relay-buffer=on` or `off` to match the existing
relay; this flag labels the measurement and does not reconfigure the attached cluster. The
current checklist uses the echo agent. `-agent=demo` is rejected in attach mode;
direct worker agent diagnostics are not available through attachment alone.

With `-results-dir`, the page automatically uploads the record after the
checklist ends (including a failed run) and on **Save results**. Save still
downloads the JSON. Uploads run independently of checklist results, with a
10-second timeout. Upload failures are shown without changing the checklist
result or delaying its return. Save serializes the record once for both the
upload and download. Token-protected `POST /demo/results` accepts one JSON object
with `Content-Type: application/json`, up to 8 MiB. Each successful request
creates `results-<launchId>-<n>.json` with mode 0600 and returns 201; existing
files are never replaced. Without the directory, the endpoint returns 404 and
the page quietly skips uploads. Retain failed and inconclusive records.

The process integration test uses two externally owned workers and a test hook:

```sh
RELAIS_TEST_REDIS_REQUIRE=1 RELAIS_HARNESS_REQUIRE_FFMPEG=1 \
  go test -race -count=1 -run '^TestDemoAttachExternalSmoke$' ./cmd/relais-demo
```

It builds its own binaries under `bin/attach-smoke-*/`, starts a separate
throwaway Redis on a free non-default loopback port, then performs two moves
and two owner kills. Builds have a separate three-minute budget, followed by
90 seconds for process actions. The test stops all processes it starts and
removes its temporary directory. CI runs all page JS tests on PRs; the attach
process test runs in the existing main/nightly/manual process lane until its
cold-build PR cost is measured. Desktop Chrome camera presentation and the full browser checklist still need a device run.

## Desktop Chrome: real camera at 720p

1. Open the printed HTTPS URL. On **Your connection is not private**, choose
   **Advanced**, then **Proceed to [your IP] (unsafe)**. This is a temporary
   exception for this demo. Do not install a root certificate. If Chrome policy
   prevents proceeding, record the block rather than changing managed settings.
   Open Chrome's certificate viewer from the page's security details. Compare
   its SHA-256 fingerprint with the terminal's **fingerprint (Chrome)** value.
   Stop if they differ. If the viewer does not expose that fingerprint, record
   this verification as NOT-RUN.
2. Confirm **Camera with counter and tone (720p)** in the Source menu. Click
   **Start call** and allow camera access. Microphone access is not requested.
   The page requests 1280×720 at 30 fps, draws camera frames into a 720p canvas,
   and stamps the same counter bands used by the pattern source.
3. Confirm that the local picture moves, the echo moves, and the tone plays.
   `cameraSettings` in the results records the actual camera dimensions and
   frame rate. If the camera cannot provide 1280×720, record that limitation.
4. Keep the echo visible for two seconds, then click
   **Run checklist (10 moves + 10 kills)**. It measures seven no-event
   baselines (the noise floor keeps the last five valid ones), ten moves, ten kills, then a continuous 60 s hold. Keep the echo
   visible and unobstructed. On a small screen the echo appears first; scroll to it immediately
   and stay there until the run finishes. Do not switch tabs or lock the device.
5. Once **60 s hold** is no longer pending, click **Save results**. Chrome
   downloads `relais-camera-[timestamp].json` to its Downloads folder (or asks
   where to save, according to browser settings). Retain failures and
   inconclusive results too. Click **Hang up** before switching devices.

Optional console equivalent, after clicking Start call to unlock audio:

```js
await relaisDemo.runChecklist();
relaisDemo.saveResults();
await relaisDemo.stop();
```

For separate scripted stages:

```js
await relaisDemo.runBaseline(7);
await relaisDemo.runMoves(10);
await relaisDemo.runKills(10);
await relaisDemo.waitForLongHold();
relaisDemo.saveResults();
```

`relaisDemo.start({source:'camera'})` selects the camera through the script API.
`source:'pattern'` (or the existing alias `'test'`) selects the default pattern.

## Android Chrome: same local network

1. Connect the phone to the same Wi-Fi. Open the exact printed HTTPS URL in
   Chrome. Transfer the full URL privately to the phone, for example in a
   private message, or type it exactly. Include the entire `#token=…` fragment.
   Do not use `localhost`, which refers to the phone.
2. On **Your connection is not private**, tap **Advanced**, then **Proceed to
   [your IP] (unsafe)**. Accept only the demo URL you started. If Android or a
   managed Chrome policy blocks the exception or camera permission, record it.
   Do not install certificates or disable browser security globally.
   Compare the SHA-256 fingerprint in Chrome's certificate viewer with the
   terminal value, as on desktop. Record NOT-RUN if that viewer is unavailable.
3. Repeat desktop steps 2–5. Keep the echo visible, the phone awake, and Chrome
   in the foreground. The same checklist button works without Android DevTools.
4. Save the JSON. Find it in Chrome → menu → **Downloads**, or the phone's
   Downloads folder. Keep the desktop and Android records separately.

## #45: compare relay buffer on and off on each device

Run both modes on desktop Chrome with the real camera at 720p, then run both
modes on Android Chrome. Replace `192.168.1.50` with the Mac's local Wi-Fi IP.
For each device:

1. Start with the buffer on (the product default):

   ```sh
   make demo-cluster DEMO_CLUSTER_FLAGS="-http=127.0.0.1:9201 -local-network=192.168.1.50 -relay-buffer=on"
   ```

2. Open the new printed URL. Confirm **Relay buffer: on** on the page.
   Run that device's checklist above. Click **Save results**, then **Hang up**.
   Keep the JSON as `desktop-camera-buffer-on.json` or
   `android-camera-buffer-on.json`.
3. Press Ctrl-C to stop the cluster. Restart with the buffer off:

   ```sh
   make demo-cluster DEMO_CLUSTER_FLAGS="-http=127.0.0.1:9201 -local-network=192.168.1.50 -relay-buffer=off"
   ```

4. Open the new printed URL; the token and certificate change on restart.
   Confirm **Relay buffer: off**. Run the same checklist again.
   Click **Save results**, then **Hang up**. Keep the JSON as
   `desktop-camera-buffer-off.json` or `android-camera-buffer-off.json`.
   Stop the cluster before starting the next device's pair of runs.

Each saved JSON labels its mode in the top-level `relayBuffer` field (`on` or
`off`), read from the server before the call starts. Compare
`events[].timeToFirstLiveFrameMs` for entries whose `kind` is `kill` in both
modes. Also record `events[].windowVerdict`, `events[].verdict`,
`longHold.status`, and the top-level `verdict`. Retain failures, invalid runs,
and inconclusive runs. The live-frame timing field does not replace verdicts.
Keep all four clearly named JSON files.

## Read the saved record and stop

Results version 5 preserves the existing media record structure and verdict rules.
`source` is `pattern` or `camera`; both use decoded counters. Each event retains
`timeToFirstNewContentMs` and `contentResumedMs`, and adds
`timeToFirstLiveFrameMs`. This field applies only to kill events (takeovers);
it is `null` for moves, drains, and baselines. Planned moves and drains have
no kill-to-live interval; read their media gap and new-content fields instead.
For kills, the new measure waits for content whose counter is beyond the
source counter sampled when the largest content gap recovers. Advancing replay
alone does not prove live content. Missing kill evidence is `"unverified"`.

The time starts at browser kill-request issuance. It is a conservative upper
bound on actual kill-to-live time, including request transit and owner lookup;
it does not subtract unsynchronized phone/server wall clocks. The server's
actual kill timestamp remains in `event.server.at`. The definition is also saved
in `measurement.timeToFirstLiveFrameMs`. This measurement is separate from the
media gap and does not change verdict thresholds.

`frameIntervalMs` comes from observed source draws, using the median of up to
60 intervals before the measurement window (or available intervals during
startup). For a source gap longer than 1.5 times that cadence, only the excess
beyond normal cadence appears in `sourceStarvedPeriods`, including an
unfinished stall. Only that excess can reduce the measured gap in starvation
diagnostics. Overlap can make a video failure inconclusive under the existing
starvation rule. A baseline is excluded from noise calibration only when
source excess overlaps its largest video gap. A steady source does not mask a
Relais media gap. Configured `cameraSettings.frameRate` remains a device setting,
not proof of the observed frame rate.

A hidden page is invalid. Missing evidence, noisy baselines, or a pending hold
can be inconclusive. Keep those records; do not interpret them as a pass.

Press Ctrl-C in the launcher terminal to stop all owned processes and Redis.
Logs remain under `bin/demo-cluster-*/`. A supplied dedicated Redis instance
stays running. No certificate or key is written to disk.
