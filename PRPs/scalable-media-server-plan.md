name: "Scalable Media Server Architecture Plan"
description: |

## Purpose
Create a comprehensive plan.md that outlines the evolution of Relais into a super scalable and easy-to-deploy media server solution with TUI management capabilities.

## Core Principles
1. **Context is King**: Include ALL necessary documentation, examples, and architectural patterns
2. **Validation Loops**: Provide executable tests for the TUI and architecture components
3. **Information Dense**: Use patterns from existing codebase and industry best practices
4. **Progressive Success**: Start with core architecture, then add management layer
5. **Global rules**: Follow all rules in CLAUDE.md

---

## Goal
Create a comprehensive plan.md document that serves as the roadmap for transforming Relais into a horizontally scalable, easy-to-deploy media server with TUI management capabilities.

## Why
- **Business value**: Enable 1-to-many live streaming at scale with minimal operational overhead
- **Integration**: Build upon existing plugin architecture while adding management and scaling capabilities
- **Problems solved**: Current architecture lacks orchestration, monitoring, and easy scaling mechanisms

## What
A strategic planning document that outlines:
- Architecture evolution from current state to target scalable system
- TUI management interface implementation using Charmbracelet Bubbles
- Deployment strategies using containerization and orchestration
- Scaling patterns for handling increased load
- Operational requirements for monitoring and management

### Success Criteria
- [ ] Complete architectural roadmap with phases
- [ ] TUI management system design with Bubbles framework
- [ ] Container orchestration strategy (Docker/Kubernetes)
- [ ] Redis clustering and storage scaling approach
- [ ] Monitoring and observability implementation plan
- [ ] Clear deployment and operational procedures

## All Needed Context

### Documentation & References
```yaml
# MUST READ - Include these in your context window
- url: https://github.com/charmbracelet/bubbletea
  why: Core TUI framework architecture and examples for management interface
  
- url: https://github.com/charmbracelet/bubbles  
  why: Pre-built components (list, table, spinner) for runners and system status
  
- file: cmd/relais-core/main.go
  why: Current server architecture pattern - context, config, graceful shutdown
  
- file: cmd/ingress-runner/main.go
  why: Plugin runner pattern - how runners are currently structured and managed
  
- file: pkg/plugins/interface.go
  why: Plugin architecture that needs to be extended for management and scaling
  
- file: pkg/storage/storage.go
  why: Storage abstraction that needs Redis clustering extensions
  
- file: pkg/config/config.go
  why: Configuration pattern using Viper that needs orchestration extensions
  
- file: README.md
  why: Current deployment and development patterns to build upon
  
- file: Makefile
  why: Build system that needs containerization and CI/CD integration
  
- doc: https://redis.io/docs/latest/operate/oss_and_stack/management/scaling/
  section: Redis Cluster scaling patterns
  critical: Horizontal scaling with hash slots for media frame distribution
  
- doc: https://pkg.go.dev/github.com/charmbracelet/bubbletea
  section: The Elm Architecture pattern
  critical: Model-Update-View pattern for TUI state management
```

### Current Codebase Tree
```bash
relais/
├── cmd/
│   ├── relais-core/main.go          # Core server with session management
│   ├── ingress-runner/main.go       # Plugin runner for media input
│   ├── egress-runner/main.go        # Plugin runner for media output
│   └── transform-runner/main.go     # Plugin runner for media processing
├── pkg/
│   ├── config/config.go             # Viper-based configuration
│   ├── plugins/interface.go         # Plugin system interfaces
│   ├── storage/storage.go           # Storage abstraction (Redis/Memory)
│   ├── server/                      # WebRTC signaling and session management
│   └── webrtc/pion_adapter.go      # WebRTC implementation
├── plugins/
│   ├── ingress/camera/             # Example ingress plugin
│   ├── egress/webrtc_egress/       # Example egress plugin
│   └── transforms/watermark/       # Example transform plugin
└── test/                           # Benchmarks and integration tests
```

### Target Codebase Tree with Enhanced Components
```bash
relais/
├── cmd/
│   ├── relais-core/main.go         # Enhanced with metrics endpoints  
│   ├── relais-manager/main.go      # NEW: TUI management application
│   ├── ingress-runner/main.go      # Enhanced with remote management
│   ├── egress-runner/main.go       # Enhanced with remote management
│   └── transform-runner/main.go    # Enhanced with remote management
├── pkg/
│   ├── tui/                       # NEW: Bubble Tea TUI components
│   │   ├── models/                # TUI models for runners, metrics
│   │   ├── views/                 # TUI view components
│   │   └── app.go                 # Main TUI application
│   ├── storage/                   # ENHANCED: Existing files with cluster support
│   │   ├── redis.go               # Enhanced with Redis Cluster support
│   │   ├── mem.go                 # Existing memory storage
│   │   └── storage.go             # Existing interface
│   ├── plugins/                   # ENHANCED: Existing plugin system  
│   │   ├── interface.go           # Existing interfaces
│   │   ├── registry.go            # Existing registry
│   │   └── pluginutil.go          # Enhanced with remote management
│   ├── server/                    # ENHANCED: Existing server with more endpoints
│   │   ├── controlplane.go        # Enhanced with runner management APIs
│   │   ├── session_mgr.go         # Existing session management
│   │   └── signaling.go           # Existing WebRTC signaling
│   └── metrics/                   # NEW: Prometheus metrics collection
├── deployments/                   # NEW: Container and orchestration configs
│   ├── docker/
│   ├── kubernetes/
│   └── compose/
├── docs/
│   └── plan.md                    # THE TARGET FILE: Scaling architecture plan
└── monitoring/                    # NEW: Grafana/Prometheus configs
```

### Known Gotchas & Library Quirks
```go
// CRITICAL: Bubble Tea requires specific architecture
// The Elm Architecture: Model + Init + Update + View
type model struct {
    runners []RunnerStatus
    selected int
}

// CRITICAL: Existing RedisStorage uses single client
// Need to enhance NewRedisStorage to accept cluster nodes:
// config := RedisConfig{
//     Addr: "node1:6379,node2:6379,node3:6379", // Comma-separated
//     ClusterMode: true,
// }

// CRITICAL: Existing PluginManager is local only
// Need to add HTTP client for remote runner management
// Existing pattern: pm.StartPlugin(ctx, pType, name, config)
// New pattern: pm.StartRemotePlugin(endpoint, pType, name, config)

// CRITICAL: Existing ControlPlane has partial API endpoints
// pkg/server/controlplane.go already has /api/v1/sessions and /api/v1/plugins
// Need to complete implementation and add runner management

// CRITICAL: Context cancellation already implemented correctly
// All existing code properly uses context.Done() channels
// Follow existing patterns in cmd/relais-core/main.go

// CRITICAL: Viper configuration pattern already established
// Follow existing pattern in pkg/config/config.go
// Environment variables: RELAIS_* prefix already used
```

## Implementation Blueprint

### Data Models and Structure

Extend existing data models rather than create new ones:
```go
// REUSE: pkg/plugins/pluginutil.go already has PluginStatus
// ENHANCE: Add remote runner information to existing PluginStatus
type EnhancedPluginStatus struct {
    plugins.PluginStatus // Embed existing status
    Host        string   // Remote runner host
    Port        int      // Remote runner port  
    Endpoint    string   // HTTP endpoint for management
    LastSeen    time.Time // Health check timestamp
}

// REUSE: pkg/storage/redis.go already has RedisConfig
// ENHANCE: Add cluster mode to existing RedisConfig
type EnhancedRedisConfig struct {
    Addr        string // Existing: single node or comma-separated cluster nodes
    Password    string // Existing
    DB          int    // Existing  
    Prefix      string // Existing
    ClusterMode bool   // NEW: enable cluster mode
}

// NEW: TUI-specific models for display
type TUIState struct {
    View     string              // "runners", "sessions", "metrics"
    Runners  []EnhancedPluginStatus
    Sessions []string            // From existing storage.ListSessions()
    Selected int                 // Currently selected item
}
```

### List of Tasks to be Completed (in order)

```yaml
Task 1: Enhance Redis Storage for Clustering
MODIFY pkg/storage/redis.go:
  - EXTEND RedisConfig struct with ClusterMode field
  - UPDATE NewRedisStorage to detect cluster mode from Addr field
  - ADD redis-cluster client support when ClusterMode=true
  - MAINTAIN backward compatibility with single-node Redis

Task 2: Create TUI Management Application
CREATE cmd/relais-manager/main.go:
  - FOLLOW existing pattern from cmd/relais-core/main.go
  - IMPLEMENT Bubble Tea application structure
  - USE existing APIs from pkg/server/controlplane.go
  - CREATE HTTP client to call /api/v1/sessions and /api/v1/plugins endpoints

Task 3: Enhance Plugin Manager for Remote Runners
MODIFY pkg/plugins/pluginutil.go:
  - EXTEND PluginManager with HTTP client capability
  - ADD methods: RegisterRemoteRunner, StartRemotePlugin, StopRemotePlugin
  - ENHANCE PluginStatus with host, port, endpoint fields
  - IMPLEMENT health checking for remote runners

Task 4: Complete Control Plane API Implementation
MODIFY pkg/server/controlplane.go:
  - COMPLETE existing handlePlugins and handleSession functions
  - ADD endpoints: GET /api/v1/runners, POST /api/v1/runners/{id}/start
  - ADD metrics endpoint: GET /api/v1/metrics
  - INTEGRATE with enhanced PluginManager

Task 5: Create TUI Components Package
CREATE pkg/tui/ directory:
  - CREATE pkg/tui/app.go with main Bubble Tea application
  - CREATE pkg/tui/models/ with TUI state management
  - CREATE pkg/tui/views/ with runner list, session list, metrics dashboard
  - USE existing Bubbles components (list, table, spinner)

Task 6: Add Monitoring and Metrics
CREATE pkg/metrics/ directory:
  - CREATE metrics collection for existing components
  - ADD Prometheus endpoint to existing HTTP server
  - INTEGRATE with existing storage and plugin systems
  - FOLLOW existing server patterns in pkg/server/

Task 7: Create Deployment Configurations
CREATE deployments/ directory:
  - CREATE Docker container definitions using existing Makefile patterns
  - CREATE Kubernetes manifests for core, runners, and manager
  - CREATE docker-compose.yml for local development
  - USE existing configuration patterns from pkg/config/
```

### Per Task Pseudocode

```go
// Task 1: Enhance Redis Storage Pseudocode
func enhanceRedisStorage() {
    // PATTERN: Extend existing RedisConfig in pkg/storage/redis.go
    type RedisConfig struct {
        Addr        string // EXISTING: "host:port" or "host1:port1,host2:port2,host3:port3"
        Password    string // EXISTING
        DB          int    // EXISTING
        Prefix      string // EXISTING
        ClusterMode bool   // NEW: auto-detect from comma-separated Addr
    }
    
    // ENHANCEMENT: NewRedisStorage function
    func NewRedisStorage(config interface{}) (*RedisStorage, error) {
        // DETECT: if Addr contains commas, enable cluster mode
        if strings.Contains(cfg.Addr, ",") {
            // USE: redis.NewClusterClient() instead of redis.NewClient()
            client = redis.NewClusterClient(&redis.ClusterOptions{
                Addrs: strings.Split(cfg.Addr, ","),
            })
        }
        // MAINTAIN: existing single-node logic for backward compatibility
    }
}

// Task 2: TUI Application Pseudocode  
func createTUIManager() {
    // PATTERN: Follow cmd/relais-core/main.go structure
    func main() {
        // REUSE: config.LoadConfig() pattern
        // CREATE: HTTP client for API calls
        apiClient := &http.Client{Timeout: 10 * time.Second}
        
        // PATTERN: Bubble Tea application
        program := tea.NewProgram(initialModel(apiClient))
        program.Run()
    }
    
    // PATTERN: Elm Architecture Model-Update-View
    type model struct {
        apiClient *http.Client                    // Call existing APIs
        runners   []plugins.PluginStatus          // From GET /api/v1/runners
        sessions  []string                        // From GET /api/v1/sessions
        list      list.Model                      // Bubbles list component
    }
}

// Task 3: Remote Plugin Management Pseudocode
func enhancePluginManager() {
    // EXTEND: existing PluginManager in pkg/plugins/pluginutil.go
    type PluginManager struct {
        mu       sync.RWMutex
        registry *Registry                        // EXISTING
        status   map[string]*PluginStatus         // EXISTING
        clients  map[string]*http.Client          // NEW: HTTP clients for remote runners
    }
    
    // NEW: Remote runner registration
    func (pm *PluginManager) RegisterRemoteRunner(endpoint string) error {
        // VALIDATE: endpoint connectivity
        // STORE: HTTP client for this endpoint
        pm.clients[endpoint] = &http.Client{Timeout: 5 * time.Second}
    }
    
    // NEW: Remote plugin operations
    func (pm *PluginManager) StartRemotePlugin(endpoint, pType, name string) error {
        // HTTP POST to endpoint/api/v1/plugins/start
        // UPDATE: local status tracking
    }
}
```

### Integration Points
```yaml
CONFIGURATION:
  - enhance: pkg/config/config.go
  - pattern: "RELAIS_STORAGE_REDIS_CLUSTER_NODES environment variable"
  - reuse: Existing Viper configuration patterns

STORAGE:
  - enhance: pkg/storage/redis.go (NOT create new file)
  - pattern: "Extend NewRedisStorage to support cluster mode"
  - maintain: "Interface compatibility with existing Storage"

TUI_MANAGEMENT:
  - create: cmd/relais-manager/main.go
  - pattern: "Follow cmd/relais-core/main.go graceful shutdown"
  - integrate: "HTTP client calling existing /api/v1/* endpoints"

PLUGIN_MANAGEMENT:
  - enhance: pkg/plugins/pluginutil.go (NOT create new orchestration package)
  - pattern: "Extend PluginManager with HTTP clients"
  - integrate: "Remote runner registration and health checking"

API_ENDPOINTS:
  - enhance: pkg/server/controlplane.go (NOT create new API server)
  - pattern: "Complete existing handlePlugins implementation"
  - integrate: "Enhanced PluginManager for remote operations"

METRICS:
  - create: pkg/metrics/ (NEW package)
  - pattern: "HTTP endpoint integrated into existing server"
  - integrate: "Add to existing mux.HandleFunc in controlplane.go"
```

## Validation Loop

### Level 1: Document Structure & Completeness
```bash
# Validate plan.md structure and content
make lint  # Ensure markdown formatting
wc -l docs/plan.md  # Should be comprehensive (500+ lines)

# Check all required sections are present:
grep -E "^## (Architecture|TUI|Storage|Orchestration|Deployment|Monitoring)" docs/plan.md

# Expected: All 6 major sections present with detailed content
```

### Level 2: Technical Feasibility Validation
```bash
# Validate external dependencies
go get github.com/charmbracelet/bubbletea
go get github.com/charmbracelet/bubbles
go get github.com/go-redis/redis/v8

# Validate Redis cluster connectivity (if available)
redis-cli --cluster info localhost:7000

# Expected: All dependencies installable, Redis cluster concepts verified
```

### Level 3: Architecture Consistency Check
```bash
# Check plan aligns with existing codebase patterns
grep -r "context.Context" cmd/ pkg/  # Verify context usage patterns
grep -r "graceful shutdown" docs/plan.md  # Ensure graceful shutdown planned
grep -r "plugin" docs/plan.md  # Verify plugin system integration

# Expected: Plan follows established patterns from existing code
```

## Final Validation Checklist
- [ ] All architecture sections comprehensive and detailed
- [ ] TUI design follows Bubble Tea patterns from documentation
- [ ] Redis clustering approach technically sound
- [ ] Container orchestration strategy practical and deployable
- [ ] Implementation phases clearly defined with success criteria
- [ ] Monitoring and observability comprehensive
- [ ] Plan builds upon existing codebase patterns
- [ ] All external dependencies researched and validated

---

## Anti-Patterns to Avoid
- ❌ Don't redesign existing plugin architecture - extend it
- ❌ Don't ignore existing configuration patterns - build upon Viper
- ❌ Don't create incompatible storage interfaces - extend Storage
- ❌ Don't skip graceful shutdown patterns - critical for media servers
- ❌ Don't hardcode cluster configuration - use environment variables
- ❌ Don't ignore existing build system - enhance Makefile approach

## Confidence Score: 9/10

This PRP provides comprehensive context including:
- Complete understanding of existing codebase architecture and patterns
- Extensive research on Charmbracelet Bubbles TUI framework
- Deep knowledge of Redis clustering and distributed storage patterns
- Clear implementation phases with specific technical approaches
- Validation gates that ensure technical feasibility
- Integration points that respect existing codebase patterns

The high confidence score reflects the thorough research, clear implementation path, and comprehensive context provided to enable successful one-pass implementation of the scalable media server plan.