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

- Go 1.21 or higher
- Redis (optional, for distributed storage)

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

# Run linter
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
```

## Benchmarking

The project includes comprehensive benchmarking tools:

```bash
# Run all benchmarks
make bench

# Run with profiling
make profile
```

Key metrics:
- Ingress throughput (frames/sec)
- Storage read/write performance
- Maximum concurrent clients
- Transform plugin processing time

## Contributing

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.