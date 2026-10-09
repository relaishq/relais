# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build Commands

```bash
# Install dependencies
make deps

# Build all binaries (core, ingress, egress, transform runners)
make build

# Run the core application
make run

# Clean build artifacts
make clean
```

## Testing and Quality

```bash
# Run all tests
make test

# Run tests with coverage report
make coverage

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

Relais is a distributed media server built with a plugin-based architecture:

- **Core Components**: 
  - `relais-core` - Main server managing sessions and coordination
  - `ingress-runner` - Handles media input plugins
  - `egress-runner` - Handles media output plugins  
  - `transform-runner` - Handles media processing plugins

- **Plugin System**: Located in `plugins/` with interfaces defined in `pkg/plugins/`
  - Ingress plugins capture media (e.g., camera input)
  - Transform plugins process media (e.g., watermarking)
  - Egress plugins output media (e.g., WebRTC streaming)

- **Storage Backend**: Abstractions in `pkg/storage/` supporting Redis and in-memory implementations

- **Key Packages**:
  - `pkg/config/` - Configuration management using Viper
  - `pkg/frames/` - Media frame handling and codecs
  - `pkg/server/` - WebRTC signaling and session management
  - `pkg/webrtc/` - Pion WebRTC adapter

## Configuration

Environment variables for configuration:
```
RELAIS_SERVER_HOST=0.0.0.0
RELAIS_SERVER_PORT=8080
RELAIS_STORAGE_TYPE=redis
RELAIS_STORAGE_REDIS_URL=localhost:6379
RELAIS_LOGGING_LEVEL=info
```

## Plugin Development

New plugins must implement one of the interfaces in `pkg/plugins/interface.go` and register via `registry.Register()`. See existing plugins in `plugins/` for examples.

## Dependencies

- Go 1.21+ required
- Uses Pion WebRTC for real-time communication
- Redis optional for distributed storage
- golangci-lint required for linting

## Agent skills

### Issue tracker

Issues and specs live in GitHub Issues for `relaishq/relais`, managed with the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

The five default triage labels: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` and `docs/adr/` at the repo root, created only when a term or decision is settled. See `docs/agents/domain.md`.
