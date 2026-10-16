# Stored-frame plugin pipeline

Legacy stored-frame pipeline (experimental, unmaintained). Not used by live calls. The camera ingress is simulated and the WebRTC egress is a storage-polling placeholder.

For current media, see [the architecture](../architecture.md). Examples such as
RTSP and S3 below describe extension points, not shipped integrations. Use a
dedicated throwaway Redis, never port 6379, even though the legacy configuration
code still defaults to that port.

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

Each plugin type has its own runner: `cmd/ingress-runner`,
`cmd/egress-runner`, and `cmd/transform-runner`. Storage has Redis and in-memory
implementations. Ingress writes stored frames, transforms read and change them,
and egress reads them. This is separate from live WebRTC.

### Configuration

The legacy bootstrap loads configuration from environment variables. These
settings do not configure the live relay, worker, or control processes:

```env
RELAIS_STORAGE_TYPE=redis
RELAIS_STORAGE_REDIS_URL=127.0.0.1:16379
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
RELAIS_STORAGE_REDIS_URL=127.0.0.1:16379
RELAIS_STORAGE_REDIS_PASSWORD=
RELAIS_STORAGE_REDIS_DB=0
RELAIS_STORAGE_REDIS_PREFIX=

# Redis Cluster (optional)
RELAIS_STORAGE_REDIS_CLUSTER_ENABLE=false
RELAIS_STORAGE_REDIS_CLUSTER_ADDRS=                  # e.g. host1:7000,host2:7001,host3:7002
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

2. Register your plugin in an embedding application (the shipped runners select
   built-ins with a switch; registration alone does not add a runner option):

```go
registry := plugins.NewRegistry()
plugins.MustRegister(registry, plugins.PluginTypeIngress, "my-plugin", func() plugins.Plugin {
    return &MyPlugin{}
})
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
```

### Testing & Quality Assurance

The project includes comprehensive testing and quality tools:

```bash
# Run all tests (integration and unit)
make test

# Run benchmarks
make bench

# Run with profiling
make profile

# Run configured lint checks (including errcheck, unused, typecheck)
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

Legacy metric definitions live in `pkg/metrics/metrics.go`. The plugin runners
do not expose a scrape endpoint. An embedding process can mount
`promhttp.Handler()` from `github.com/prometheus/client_golang/prometheus/promhttp`
to expose the global registry. Live `relais-relay`, `relais-worker` and
`relais-control` each serve a separate process registry at private `GET /metrics`;
that registry does not expose these legacy metric families.

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

Scrape example for an embedding process that actually mounts the global
registry on port 8080 (no shipped plugin runner does this):

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

- Grafana dashboard: [Grafana dashboard](../observability/grafana/redis-groups.json)
- Prometheus alert rules: [Prometheus alert rules](../observability/alerts/redis-groups.yml)
- Observability setup guide: [Observability setup](../observability/README.md)

### Key Metrics
- Ingress throughput (frames/sec)
- Storage read/write performance  
- Maximum concurrent clients
- Transform plugin processing time
- Code coverage and quality scores

### Code Quality
- **golangci-lint**: configured checks including errcheck, unused, typecheck
- **Integration tests**: End-to-end plugin pipeline testing
- **Benchmark tests**: Performance regression detection
- **Race condition detection**: Go race detector; context cancellation and error handling are separate requirements

