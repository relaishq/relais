# Relais Media Server

Relais is a distributed media server built in Go that supports flexible ingress and egress of media streams through a plugin system. It uses Pion WebRTC for real-time communication and supports horizontal scaling.

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
  - Load balancing across core servers

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
       └───────────┬───────┴───────────┬──────┘
                   │                   │
            ┌─────────────┐     ┌─────────────┐
            │   Storage   │     │   Relais    │
            │   Backend   │ ◄─► │    Core     │
            └─────────────┘     └─────────────┘
```

## Getting Started

### Prerequisites

- Go 1.23 or higher
- Redis (optional, for distributed storage)
- golangci-lint (for development)

### Installation

```bash
# Clone the repository
git clone https://github.com/yourusername/relais.git
cd relais

# Install dependencies
make deps

# Build the binaries
make build
```

### Running Tests

```bash
# Run all tests
make test

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

# Run with hot reload
make run
```

### Configuration

Configuration is loaded from environment variables:

```env
RELAIS_SERVER_HOST=0.0.0.0
RELAIS_SERVER_PORT=8080
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

Relais exposes Prometheus metrics at `GET /metrics` from the core server (see `cmd/relais-core/main.go`). By default the server listens on `RELAIS_SERVER_HOST:RELAIS_SERVER_PORT`.

- Example: `http://localhost:8080/metrics`

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

- Egress tailer:
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