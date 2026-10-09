# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build Commands

```bash
# Install dependencies
make deps

# Build the plugin runner binaries (ingress, egress, transform)
make build

# Run media workers A and B on one UDP socket plus the browser echo page on
# http://localhost:9101 ("Move call" hands the live call to the other worker)
make demo

# Clean build artifacts
make clean
```

## Testing and Quality

```bash
# Run all tests
make test

# Run tests with coverage report
make coverage

# Run the call harness (audio+video echo calls, race detector)
make test-harness

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

## Architecture Overview

Relais is a distributed media server. Live WebRTC media runs in the media worker prototype; stored frames flow through a plugin-based pipeline:

- **Media path (prototype)**:
  - `pkg/mediaworker/` - Media worker: a minimal WebRTC endpoint built from Pion v4 component libraries (ICE-lite, DTLS-SRTP, RTP) with WHIP-style signaling; it currently echoes the caller's audio and video. A live session can be exported to bytes and resumed on another worker that shares the same UDP socket (`Socket.Handover`, a planned handover)
  - `pkg/callharness/` - Call harness: the test seam for media; a Pion WebRTC client plays the caller in-process, and `Call.Handover` moves a call between workers
  - `pkg/relay/` - Relay: the one public UDP address; routes each caller's packets to the worker that owns the session, and holds them during a move
  - `pkg/sessionstore/` - Session store: fenced ownership leases (owner, epoch, expiry); in-memory for now
  - `pkg/controlplane/` - Control plane: places calls, moves live calls between workers behind the relay, drains workers, and serves `GET /status`
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

Environment variables for configuration:
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
- Redis optional for distributed storage
- golangci-lint required for linting

## Agent skills

### Issue tracker

Issues and specs live in GitHub Issues for `relaishq/relais`, managed with the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

The five default triage labels: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` and `docs/adr/` at the repo root, created only when a term or decision is settled. See `docs/agents/domain.md`.
