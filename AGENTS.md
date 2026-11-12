# AGENTS.md

This file provides guidance to coding agents when working with code in this repository.

Before handing over a change, run `make check-quality` and the tests of the packages you changed. CI on the exact revision (the PR head, or the batch head when PRs merge in a batch) is the final validation; run the full `make check` locally only when CI cannot run it. See [CODING_STANDARDS.md](CODING_STANDARDS.md) for the review checklist.

## Build Commands

```bash
# Install dependencies
make deps

# Build plugin runners plus relais-relay, relais-worker, relais-control
# and the opt-in crash-run, relais-demo and delayed-loopback binaries
make build

# Run media workers A and B on one UDP socket plus the browser echo page on
# http://localhost:9101 ("Move call" hands the live call to the other worker)
make demo

# Browser cluster demo: relay, control, 3 workers, dedicated throwaway Redis
# Move, drain and SIGKILL controls; Chrome scripted API in window.relaisDemo
make demo-cluster
# Optional loopback HTTP address / dedicated non-6379 Redis
make demo-cluster DEMO_CLUSTER_FLAGS="-http=127.0.0.1:9201"

# Prove real SIGKILL takeover: ten interleaved runs per mode, each with 60 s of media/consent
# Requires redis-server and ffmpeg; starts its own Redis, never port 6379
make crash-run
# Developer smoke run (does not prove the 60 s consent threshold)
make crash-run CRASH_FLAGS="-runs=1 -after=3s -verbose"

# Clean build artifacts
make clean
```

## Testing and Quality

```bash
# Run all tests (Redis tests skip unless a dedicated endpoint is set)
make test

# Run tests with coverage report
make coverage

# PR checks: race units with Redis, short race harness with memory only,
# Redis uncertainty under race, and memory timing smoke (requires ffmpeg)
# Set this to your own dedicated Redis; never use the user's port 6379.
export RELAIS_TEST_REDIS_ADDR=127.0.0.1:16379
export RELAIS_TEST_REDIS_REQUIRE=1
export RELAIS_HARNESS_REQUIRE_FFMPEG=1
make test-unit
make test-harness-short
go test -race -v -count=1 -timeout 5m -run '^TestRelayUncertainTransferSurvives$' ./pkg/callharness/
make test-harness-smoke

# Full in-process tests: race units, full race harness and timing gates
# on both stores (requires dedicated Redis and ffmpeg)
make test-full

# Run individual full harness gates
make test-harness
make test-harness-timing

# Run benchmarks
make bench

# Run benchmarks with profiling
make profile

# Run linter
make lint

# Run go vet
make vet

# Format code
make fmt
```

`make check` starts and cleans up its own throwaway Redis (see `scripts/dev/README.md`).
For other Redis tests, use a disposable Redis on a dedicated endpoint, never port 6379.
CI jobs, schedules and gates are described in [docs/dev/ci.md](docs/dev/ci.md).

## Architecture Overview

Relais is a distributed media server. Live WebRTC media runs in the media worker; stored frames flow through a plugin-based pipeline:

- **Media path**:
  - `pkg/mediaworker/` - Media worker: a minimal WebRTC endpoint built from Pion v4 component libraries (ICE-lite, DTLS-SRTP, RTP) with WHIP-style signaling; it currently echoes the caller's audio and video. A live session can be exported to bytes and resumed on another worker that shares the same UDP socket (`Socket.Handover`, a planned handover); relayed workers also persist fenced snapshots for automatic crash takeover
  - `pkg/callharness/` - Call harness: the test seam for media; a Pion WebRTC client plays the caller in-process, and `Call.Handover` moves a call between workers
  - `pkg/relay/` - Relay: the one public UDP address; routes each caller's packets to the worker that owns the session, and holds them during a move
  - `pkg/sessionstore/` - Session store: fenced ownership leases (owner, epoch, expiry) and resumable state blobs; in-memory or encrypted Redis
  - `pkg/controlplane/` - Control plane: places calls, moves live calls between workers behind the relay, drains workers, detects heartbeat failures, takes over crashed workers' calls, and serves `GET /status`
  - `cmd/relais-relay`, `cmd/relais-worker`, `cmd/relais-control` - Separate media processes using Redis session state, private HTTP coordination, UDP relay legs and explicit rejoin acknowledgement
  - `cmd/relais-signal` - Authenticated HTTPS test signaling frontend: call creation/deletion, sanitized status and frontend-host clock samples; forwards only the allowed requests to private control
  - `cmd/crash-run` - Starts the process topology and a dedicated throwaway Redis, runs the Pion caller and SIGKILLs the owning worker (`make crash-run`)
  - `cmd/echo-demo` - Browser echo demo: two media workers on one socket, with a "Move call" button (`make demo`)

- **Plugin runners**:
  - `ingress-runner` - Handles media input plugins
  - `egress-runner` - Handles media output plugins
  - `transform-runner` - Handles media processing plugins

- **Plugin System**: Located in `plugins/` with interfaces defined in `pkg/plugins/`
  - Ingress plugins capture media (e.g., camera input)
  - Transform plugins process media (e.g., watermarking)
  - Egress plugins output media (the WebRTC egress plugin is a storage-polling placeholder)

- **Storage Backend**: Abstractions in `pkg/storage/` supporting Redis and in-memory implementations

- **Key Packages**:
  - `pkg/config/` - Configuration management using Viper
  - `pkg/frames/` - Media frame handling and codecs

## Configuration

Environment variables for the legacy plugin pipeline (not the live media processes):
```
RELAIS_STORAGE_TYPE=redis
RELAIS_STORAGE_REDIS_URL=localhost:6379
RELAIS_LOGGING_LEVEL=info
```

## Plugin Development

New plugins must implement one of the interfaces in `pkg/plugins/interface.go` and register via `registry.Register()`. See existing plugins in `plugins/` for examples.

## Dependencies

- Go 1.26+ required
- Uses Pion v4 libraries for real-time communication
- Redis optional for in-process tests, required for the separate-process topology
- `make lint` runs the pinned golangci-lint version without installing it

## Agent skills

### Issue tracker

Issues and specs live in GitHub Issues for `relaishq/relais`, managed with the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

The five default triage labels: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` and `docs/adr/` at the repo root, created only when a term or decision is settled. See `docs/agents/domain.md`.
