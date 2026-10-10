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
opt-in and is **not required in CI**.

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

The Redis integration tests require a reachable Redis instance. By default they use `localhost:6379`. Override via:

```bash
export RELAIS_TEST_REDIS_ADDR=localhost:6379
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
