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
fingerprint. Call controls are reachable from the local network, so use a
trusted network. Allow the owned demo processes through the Mac firewall if
prompted. Do not change router settings or open ports to the internet.

The self-signed ECDSA certificate and key are generated in memory at startup.
They expire after 24 hours and change every restart. Use the printed literal-IP
URL, such as `https://192.168.1.50:9201/?source=camera&autostart=0`.
`-local-network` accepts a literal private or loopback IPv4 address, not a
wildcard or DNS name. IPv6 is rejected: HTTPS worked in the browser check,
but IPv6 media did not connect. Without the flag the demo remains HTTP and
media on loopback.

`make demo` runs the older shared-socket echo page. Use `make demo-cluster` for
this checklist, which requires separate processes and kill controls.

## Desktop Chrome: real camera at 720p

1. Open the printed HTTPS URL. On **Your connection is not private**, choose
   **Advanced**, then **Proceed to [your IP] (unsafe)**. This is a temporary
   exception for this demo. Do not install a root certificate. If Chrome policy
   prevents proceeding, record the block rather than changing managed settings.
2. Confirm **Camera with counter and tone (720p)** in the Source menu. Click
   **Start call** and allow camera access. Microphone access is not requested.
   The page requests 1280×720 at 30 fps, draws camera frames into a 720p canvas,
   and stamps the same counter bands used by the pattern source.
3. Confirm that the local picture moves, the echo moves, and the tone plays.
   `cameraSettings` in the results records the actual camera dimensions and
   frame rate. If the camera cannot provide 1280×720, record that limitation.
4. Keep the echo visible for two seconds, then click
   **Run checklist (10 moves + 10 kills)**. It measures five no-event
   baselines, ten moves, ten kills, then a continuous 60 s hold. Keep the echo
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
await relaisDemo.runBaseline(5);
await relaisDemo.runMoves(10);
await relaisDemo.runKills(10);
await relaisDemo.waitForLongHold();
relaisDemo.saveResults();
```

`relaisDemo.start({source:'camera'})` selects the camera through the script API.
`source:'pattern'` (or the existing alias `'test'`) selects the default pattern.

## Android Chrome: same local network

1. Connect the phone to the same Wi-Fi. Open the exact printed HTTPS URL in
   Chrome. Do not use `localhost`, which refers to the phone.
2. On **Your connection is not private**, tap **Advanced**, then **Proceed to
   [your IP] (unsafe)**. Accept only the demo URL you started. If Android or a
   managed Chrome policy blocks the exception or camera permission, record it.
   Do not install certificates or disable browser security globally.
3. Repeat desktop steps 2–5. Keep the echo visible, the phone awake, and Chrome
   in the foreground. The same checklist button works without Android DevTools.
4. Save the JSON. Find it in Chrome → menu → **Downloads**, or the phone's
   Downloads folder. Keep the desktop and Android records separately.

## Read the saved record and stop

Results version 4 preserves the existing record structure and verdict rules.
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

A hidden page is invalid. Missing evidence, noisy baselines, or a pending hold
can be inconclusive. Keep those records; do not interpret them as a pass.

Press Ctrl-C in the launcher terminal to stop all owned processes and Redis.
Logs remain under `bin/demo-cluster-*/`. A supplied dedicated Redis instance
stays running. No certificate or key is written to disk.
