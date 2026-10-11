# Planned-move latency diagnostics (#87)

This round measures the suspected TCP loss-recovery cost. It does not change
move retries, fencing, rollback, connection pools, or the profile verdict.
The next native Linux run must confirm or refute the hypothesis.

## Evidence so far

[Issue #87](https://github.com/relaishq/relais/issues/87) reports four slow
moves in twenty under lossy-dc. The provided saved report shows controller
`event_result.Duration` of 226.7, 231.8, 237.2 and 232.7 ms in trials 3, 9,
10 and 15. Trials 3 and 9 spend 211.9 and 216.3 ms in resume. Trials 10 and
15 spend about 221 and 217 ms outside the existing barrier/export/resume
timers. Export stays near 2–3 ms. These are controller paused intervals. The new root move trace also includes
preparation. Existing successful caller move records use the controller's
paused timestamps. The earlier
export-only spike is not enough to identify the phase in later runs.

No provided artifact has TCP counters. TCP recovery is still a hypothesis.

## What the report records

Every measured profile move automatically enables `RELAIS_MOVE_DIAGNOSTICS=1`
in its disposable child processes. Normal process runs leave it disabled.
A bounded in-memory recorder stores metadata, with no file or network writes
on the measured path. The existing private listeners expose a read-only
`GET /private/move-diagnostics` endpoint only while diagnostics are enabled.
Collection happens after the move and after its ending TCP snapshot.

Each `Rows[].move_diagnostics` contains:

- A random `trace_id`, propagated through the real private HTTP adapters.
  The root starts inside `Plane.Move`; the driver-to-control trigger request is
  excluded from these private coordination call counts.
- `phase_calls` for `prepare`, `barrier`, `export`, `transfer`, `resume` and
  `release`. Preparation includes the owner read and target load lookup before
  the barrier. Transfer includes both Redis ownership change and relay routing.
- An event for each client HTTP call, including start/end wall timestamps,
  elapsed nanoseconds, JSON request/response payload bytes, connection checkout
  count and reused connection count. Timing includes the complete response body.
- Separate `http_proxy` events and `proxy_http_requests` for the second TCP leg
  through the driver-owned lossy-dc fault proxy. They are not double-counted as
  controller RPCs. Its response timing also includes reading the body.
- Each client Redis command's name and timing, including script fallback and
  pipeline commands. Arguments, keys, session identifiers, payloads, passwords
  and state bytes are not recorded. Commands inside Lua and Redis connection
  initialization are excluded. A pipeline's commands share its round-trip
  interval, so summing those durations would double-count elapsed time.
- `tcp_before`, `tcp_after` and `tcp_deltas`, with the requested seven counters:
  `TcpRetransSegs`, `TCPLossProbes`, `TCPLossProbeRecovery`, `TCPTimeouts`,
  `TCPSynRetrans`, `TCPFastRetrans`, `TCPSpuriousRTOs`. Kernel files are read
  inside every owned namespace, including caller and fabric. The driver
  namespace is also read because it sends the proxy's upstream requests.

TCP counters include background heartbeats, periodic snapshots, consent and
metrics. Namespace samples are sequential; each has its own start/end time.
A slow call plus a matching namespace delta supports attribution, but does
not prove which socket lost a packet. Packet capture or socket-level tracing
would be the next diagnostic if multiple calls overlap. HTTP/Redis event
counts are client operations, not TCP packets. HTTP bytes exclude headers and
TCP overhead; Redis wire bytes are not attributed to individual commands.

The measured move's returned trace ID separates it from deliberate fault
probes before the baseline. If an error prevents returning that ID, one root
move event fully inside the snapshot window can recover the ID. A failed or
rolled-back move is marked incomplete: detached recovery operations can leave
that scope. Missing counters, resets, read/collection errors and buffer drops
remain explicit. Unavailable counter totals are not printed as zero. Diagnostic
completeness does not change the existing media acceptance rules.

`PROFILE_RESULT` preserves the existing largest-media-gap trial and adds
`slowest_move_trial`, its diagnostics, the longest call and namespace recovery
counter totals. `kernel_release` and optional `private_routes` are top-level
report fields. Reports still encode durations in nanoseconds. Kernel provenance
is also uploaded as `network-profile-kernel.log`.

## Native Linux A/B dispatch

Workflow: `.github/workflows/ci.yml` (`CI`). Job: `netns-network-profiles`
(`netns-network-profiles (lossy-dc, move)`). After the orchestrator commits and
pushes the branch, dispatch these separately:

```sh
gh workflow run ci.yml --repo relaishq/relais --ref investigate/87-move-latency \
  -f network_profile_lane=lossy-dc-move -f private_route_tuning=false

gh workflow run ci.yml --repo relaishq/relais --ref investigate/87-move-latency \
  -f network_profile_lane=lossy-dc-move -f private_route_tuning=true
```

Each dispatch runs twenty move trials, with 60 seconds of post-event consent.
The input narrows the network-profile matrix to this one lane; other existing
manual CI jobs still run. Scheduled runs retain the whole matrix and default
routes. Compare the `network-profile-lossy-dc-move-results` artifacts, especially
longest calls, connection reuse, per-phase command counts, recovery counters,
raw rows and p95. Inspect failures too; the existing verdict remains strict.
The same Linux commands outside CI are:

```sh
make build
sudo -E env RELAIS_REDIS_ADDR= ./bin/crash-run \
  -topology=netns -profile=lossy-dc -scenario=move -runs=20 -after=60s
sudo -E env RELAIS_REDIS_ADDR= ./bin/crash-run -private-route-tuning \
  -topology=netns -profile=lossy-dc -scenario=move -runs=20 -after=60s
```

The opt-in flag changes only existing connected DC routes inside owned relay,
control, store and worker namespaces: `rto_min 5ms quickack 1`. The driver checks
actual route output, including the RTO metric lock. It does not change host,
caller or fabric routes, sysctls or network impairment. Namespace removal
removes the settings. The namespace integration test checks observed settings,
TCP reads, host/caller route preservation and cleanup on native Linux.

The host-side proxy sends responses and upstream requests with the host's
unchanged TCP settings. Thus this safe A/B cannot promise to eliminate every
spike. A driver-counter spike with a slow proxy call can reveal that limit.
Do not silently tune the host to obtain a green result.

## Phase 2 options, smallest first

| Rank | Change | Expected effect | Main risk or limit |
| --- | --- | --- | --- |
| 1 | Opt-in private route RTO/quickack experiment (implemented only in the test topology) | If one outstanding data segment drives recovery, reduce its roughly 200 ms floor toward single-digit to tens of milliseconds on a short DC path. | Spurious retransmission under jitter/load; does not lower initial SYN timeout; host-side proxy remains untuned. Needs native A/B evidence before deployment guidance. |
| 2 | Reduce critical-path Redis/HTTP round trips; retain or warm pools where measurements show cold connections | Fewer packets and fewer serial loss opportunities; warming can avoid a roughly 1 s initial SYN-loss timeout. | Requires an ownership/uncertainty audit before batching mutations. Existing HTTP transport already pools; warming alone cannot remove data-loss recovery. Not implemented. |
| 3 | Retry or hedge RPCs using operation IDs and replayable results | Recover sooner from a missing response without waiting for TCP's full recovery delay. | Export fences/removes its source; duplicate operations and uncertain outcomes require durable identity and exact replay semantics. A shorter timeout alone is unsafe. Not implemented. |

### HTTP connection reuse

`privateapi.Do` substitutes the process-wide singleton `metrics.HTTPTransport`
when the client has no transport. That singleton clones Go's standard transport
once. Although `remoteClient(nil)` creates an HTTP client each time, it uses
that shared pool. `Do` consumes and closes every normal response body. The
fault proxy also retains one transport. Process servers have a 10-second idle
connection timeout, so an idle pool is not evidence that every move begins on
an existing connection. Per-call connection checkout/reuse fields distinguish
this on the runner. Local real HTTP tests verify reuse and full-body timing.

### Kernel source check and its limit

In upstream [Linux v6.8 tcp_output.c](https://github.com/torvalds/linux/blob/v6.8/net/ipv4/tcp_output.c#L2829)
and [v6.11 tcp_output.c](https://github.com/torvalds/linux/blob/v6.11/net/ipv4/tcp_output.c#L2866),
`tcp_schedule_loss_probe` adds the effective minimum RTO to twice the measured
RTT when exactly one packet is outstanding. It caps that probe timer by the
remaining RTO. With no RTT sample it uses the initial TCP timeout.
[Linux v6.11 tcp.h](https://github.com/torvalds/linux/blob/v6.11/include/net/tcp.h#L783)
resolves a locked route `RTAX_RTO_MIN` before the socket default.
[iproute2 v6.11 iproute.c](https://github.com/iproute2/iproute2/blob/v6.11.0/ip/iproute.c#L1318)
automatically locks `rto_min`. These source paths support the expectation that
route tuning changes the single-packet TLP floor, not just ordinary RTO.
Quick ACK reduces delayed-ACK waiting; the route RTO metric changes the floor.

**Exact runner kernel check: NOT-RUN.** Run 38164682086 identifies Ubuntu
24.04.5 and image 20261004.327.1, but its available logs do not record `uname`
or a kernel package version. Those OS/image names cannot prove the active
kernel revision. The next run records `uname -a`, `/proc/version_signature`
when available, and `kernel_release` in the report. Match that vendor version
against its kernel source before claiming this formula applies to that runner.
Do not treat the checked upstream versions as proof of its Ubuntu backports.

## Local verification

Use the pinned golangci-lint v2.14.0, `gofmt`, `go vet ./...`,
`GOOS=linux go vet ./...`, `make build`, `actionlint .github/workflows/ci.yml`,
and changed-package race tests against an owned disposable Redis on a non-6379
port. The real HTTP topology test exercises both memory and Redis, including
all five private move RPC phases and synchronous Redis work in prepare,
transfer and resume. Metadata propagation and connection reuse have local
runtime evidence. One local Redis move recorded five private HTTP calls and
nine Redis commands: two in prepare, three in transfer and four in resume.
This is local evidence; retries and script-cache state can change runner counts.
Linux namespace sampling, route settings and lossy latency
mitigation are NOT-RUN on macOS.
