# Relais Scalable Media Server Architecture Plan

## Executive Summary

This document outlines the roadmap for transforming Relais into a horizontally scalable, easy-to-deploy media server solution with Terminal User Interface (TUI) management capabilities. The evolution maintains backward compatibility while adding distributed storage, remote runner management, and comprehensive operational tooling.

## Current Architecture Analysis

### Existing Components

Relais currently implements a solid foundation with the following architecture:

```
┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
│  Ingress Runner │    │ Transform Runner│    │  Egress Runner  │
│   (camera)      │───▶│  (watermark)    │───▶│   (webrtc)      │
└─────────────────┘    └─────────────────┘    └─────────────────┘
         │                       │                       │
         └───────────────────────┼───────────────────────┘
                                 │
                    ┌─────────────────┐
                    │  Relais Core    │
                    │ ┌─────────────┐ │
                    │ │Session Mgr  │ │
                    │ │Control Plane│ │
                    │ │WebRTC Signal│ │
                    │ └─────────────┘ │
                    └─────────────────┘
                                 │
                    ┌─────────────────┐
                    │    Storage      │
                    │ (Redis/Memory)  │
                    └─────────────────┘
```

### Current Strengths

1. **Robust Plugin System**: Well-designed interfaces with registry and lifecycle management
   - Located in `pkg/plugins/` with comprehensive interfaces for ingress, egress, and transform plugins
   - Plugin manager in `pkg/plugins/pluginutil.go` handles local plugin lifecycle
   - Factory pattern enables easy plugin registration and creation

2. **Storage Abstraction**: Clean interface supporting multiple backends
   - `pkg/storage/storage.go` defines unified Storage interface
   - Redis implementation in `pkg/storage/redis.go` with atomic operations
   - Memory implementation for development/testing

3. **Session Management**: Thread-safe session tracking and lifecycle
   - `pkg/server/session_mgr.go` provides centralized session management
   - Automatic cleanup of expired sessions
   - Metadata support for session configuration

4. **Configuration Management**: Viper-based config with environment variable support
   - `pkg/config/config.go` implements structured configuration
   - Environment variables with `RELAIS_*` prefix
   - Support for multiple storage backends

5. **WebRTC Integration**: Pion WebRTC adapter for real-time communication
   - `pkg/webrtc/pion_adapter.go` abstracts WebRTC operations
   - Signaling server in `pkg/server/signaling.go`

### Current Limitations

1. **Single Point of Failure**: Redis storage is single-node only
2. **Manual Management**: No TUI or automated runner management
3. **Local Plugins Only**: Plugin manager cannot handle remote runners
4. **Incomplete APIs**: Control plane endpoints are partially implemented
5. **No Monitoring**: Lack of metrics and observability
6. **Manual Deployment**: No containerization or orchestration configs
7. **No Auto-scaling**: Static runner deployment only

## Architecture Evolution Strategy

### Target Architecture

The target architecture maintains the existing plugin-based design while adding horizontal scaling, remote management, and operational capabilities:

```
                               ┌─────────────────┐
                               │  TUI Manager    │
                               │   (Bubble Tea)  │
                               └─────────────────┘
                                         │ HTTP API
                                         ▼
┌─────────────────┐              ┌─────────────────┐              ┌─────────────────┐
│  Runner Cluster │              │  Relais Core    │              │  Runner Cluster │
│ ┌─────────────┐ │              │    Cluster      │              │ ┌─────────────┐ │
│ │Ingress-1    │ │              │ ┌─────────────┐ │              │ │Egress-1     │ │
│ │Ingress-2    │ │◀────────────▶│ │Enhanced API │ │◀────────────▶│ │Egress-2     │ │
│ │Ingress-N    │ │              │ │Session Mgr  │ │              │ │Egress-N     │ │
│ └─────────────┘ │              │ │Plugin Mgr   │ │              │ └─────────────┘ │
└─────────────────┘              │ │Metrics      │ │              └─────────────────┘
                                 │ └─────────────┘ │
                                 └─────────────────┘
                                           │
                               ┌─────────────────┐
                               │ Redis Cluster   │
                               │ ┌─────────────┐ │
                               │ │   Node-1    │ │
                               │ │   Node-2    │ │
                               │ │   Node-N    │ │
                               │ └─────────────┘ │
                               └─────────────────┘
```

### Evolution Principles

1. **Incremental Enhancement**: Build upon existing solid foundation
2. **Backward Compatibility**: Maintain existing APIs and patterns
3. **Horizontal Scaling**: Support multiple instances of each component
4. **Operational Excellence**: Add monitoring, metrics, and management tools
5. **Container-First**: Design for containerized deployment
6. **Self-Service**: Enable operators to manage the system via TUI

## TUI Management Interface Design

### Framework Selection: Charmbracelet Bubble Tea

Bubble Tea (https://github.com/charmbracelet/bubbletea) provides an excellent foundation for building terminal applications using The Elm Architecture pattern (Model-Update-View). This aligns well with Relais's structured approach.

The Bubbles component library (https://github.com/charmbracelet/bubbles) provides pre-built components including list, table, spinner, text input, and progress bar components that are perfect for the management interface.

#### Key Components

1. **Main Application Structure** (`cmd/relais-manager/main.go`)
```go
// Following existing pattern from cmd/relais-core/main.go
func main() {
    // Setup context with cancellation for graceful shutdown
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    // Load configuration using existing patterns
    cfg, err := config.LoadConfig()
    
    // Initialize HTTP client for API communication
    apiClient := &http.Client{Timeout: 10 * time.Second}
    
    // Handle graceful shutdown like existing relais-core
    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
    
    go func() {
        <-sigChan
        cancel() // Graceful shutdown of TUI
    }()
    
    // Start Bubble Tea application
    program := tea.NewProgram(initialModel(cfg, apiClient))
    if err := program.Start(); err != nil {
        log.Fatal(err)
    }
}
```

2. **TUI State Management** (`pkg/tui/models/`)
```go
// Main application model following Elm Architecture
type Model struct {
    // Configuration and API client
    config    *config.Config
    apiClient *http.Client
    
    // Current view state
    currentView string // "runners", "sessions", "metrics", "logs"
    
    // Data from APIs
    runners   []RunnerStatus
    sessions  []SessionInfo
    metrics   SystemMetrics
    
    // UI components from Bubbles
    runnerList  list.Model    // Runner management
    sessionList list.Model    // Active sessions
    table       table.Model   // Metrics display
    spinner     spinner.Model // Loading states
    
    // Navigation and selection
    selectedRunner int
    loading        bool
    error          string
}
```

3. **View Components** (`pkg/tui/views/`)

**Runner Management View:**
- List active runners with status (running/stopped/error)
- Show runner type (ingress/egress/transform)
- Display host, port, and last-seen timestamp
- Actions: start, stop, restart, view logs

**Session Management View:**
- List active media sessions
- Show session type, duration, and metadata
- Frame count and throughput metrics
- Actions: view details, terminate session

**Metrics Dashboard:**
- Real-time system metrics
- Storage usage and health
- Runner performance statistics
- Network throughput and latency

**Log Viewer:**
- Streaming logs from runners and core
- Filter by component and log level
- Search and navigation capabilities

#### TUI Implementation Details

1. **API Integration**
```go
// HTTP client calling existing control plane endpoints
func (m Model) fetchRunners() tea.Cmd {
    return tea.Exec(func() tea.Msg {
        resp, err := m.apiClient.Get(m.config.Server.Host + "/api/v1/runners")
        if err != nil {
            return errMsg{err}
        }
        
        var runners []RunnerStatus
        json.NewDecoder(resp.Body).Decode(&runners)
        return runnersMsg{runners}
    })
}
```

2. **Real-time Updates**
```go
// Periodic refresh of data using tea.Tick
func (m Model) Init() tea.Cmd {
    return tea.Batch(
        m.fetchRunners(),
        m.fetchSessions(),
        tea.Tick(time.Second*5, func(t time.Time) tea.Msg {
            return tickMsg{t}
        }),
    )
}
```

3. **Responsive Layout**
```go
// Using Lipgloss for styling and layout
func (m Model) View() string {
    switch m.currentView {
    case "runners":
        return runnerViewStyle.Render(m.runnerList.View())
    case "sessions":
        return sessionViewStyle.Render(m.sessionList.View())
    case "metrics":
        return metricsViewStyle.Render(m.table.View())
    default:
        return "Unknown view"
    }
}
```

## Storage Scaling with Redis Clustering

### Current Redis Implementation Analysis

The existing Redis storage in `pkg/storage/redis.go` uses a single Redis client with these features:
- Atomic operations using pipelines
- Session tracking with Redis Sets
- Frame storage using Redis Lists
- Configurable key prefixes for namespacing

### Redis Cluster Enhancement Strategy

#### 1. Enhanced Configuration

Extend the existing `RedisConfig` to support cluster mode:

```go
// Enhanced RedisConfig in pkg/storage/redis.go
type RedisConfig struct {
    Addr        string // "host:port" or "host1:port1,host2:port2,host3:port3"
    Password    string // Existing
    DB          int    // Existing (ignored in cluster mode)
    Prefix      string // Existing
    ClusterMode bool   // Auto-detected from comma-separated Addr
}
```

#### 2. Cluster Client Integration

Modify `NewRedisStorage` to support cluster mode:

```go
func NewRedisStorage(config interface{}) (*RedisStorage, error) {
    var client redis.Cmdable // Interface supporting both single and cluster
    
    switch cfg := config.(type) {
    case string:
        if strings.Contains(cfg, ",") {
            // Cluster mode: comma-separated addresses
            addrs := strings.Split(cfg, ",")
            client = redis.NewClusterClient(&redis.ClusterOptions{
                Addrs: addrs,
            })
        } else {
            // Single node mode
            client = redis.NewClient(&redis.Options{Addr: cfg})
        }
    case RedisConfig:
        if strings.Contains(cfg.Addr, ",") || cfg.ClusterMode {
            // Cluster mode
            addrs := strings.Split(cfg.Addr, ",")
            client = redis.NewClusterClient(&redis.ClusterOptions{
                Addrs:    addrs,
                Password: cfg.Password,
            })
        } else {
            // Single node mode (existing logic)
            client = redis.NewClient(&redis.Options{
                Addr:     cfg.Addr,
                Password: cfg.Password,
                DB:       cfg.DB,
            })
        }
    }
    
    // Test connectivity
    if err := client.Ping(context.Background()).Err(); err != nil {
        return nil, fmt.Errorf("redis connection failed: %v", err)
    }
    
    return &RedisStorage{client: client, prefix: prefix}, nil
}
```

#### 3. Frame Distribution Strategy

Redis Cluster (https://redis.io/docs/latest/operate/oss_and_stack/management/scaling/) uses hash slots (16384) for data distribution. Design frame keys for optimal distribution:

```go
// Frame key design for cluster distribution
func (s *RedisStorage) frameKey(sessionID string, frameIndex int64) string {
    // Use session ID as the hash key for affinity
    // All frames for a session will be on the same node
    return fmt.Sprintf("%s{%s}:frames:%d", s.prefix, sessionID, frameIndex)
}

func (s *RedisStorage) sessionKey(sessionID string) string {
    // Session metadata uses the same hash tag
    return fmt.Sprintf("%s{%s}:session", s.prefix, sessionID)
}
```

#### 4. Cluster-Aware Operations

Ensure operations work correctly across cluster nodes:

```go
// Enhanced PutFrame for cluster compatibility
func (s *RedisStorage) PutFrame(ctx context.Context, frame Frame) error {
    // Use hash tags to ensure session affinity
    frameKey := s.frameKey(frame.SessionID, frame.Index)
    sessionSetKey := s.prefix + "active_sessions"
    
    // Pipeline works within single hash slot
    pipe := s.client.TxPipeline()
    pipe.RPush(ctx, frameKey, frameJSON)
    pipe.SAdd(ctx, sessionSetKey, frame.SessionID)
    
    _, err := pipe.Exec(ctx)
    return err
}
```

### Migration Strategy

1. **Phase 1**: Deploy cluster-compatible code with single-node config
2. **Phase 2**: Set up Redis cluster in parallel
3. **Phase 3**: Migrate data using Redis migration tools
4. **Phase 4**: Switch configuration to cluster mode
5. **Phase 5**: Remove single-node Redis

### Configuration Examples

```bash
# Single Node (current)
RELAIS_STORAGE_TYPE=redis
RELAIS_STORAGE_REDIS_URL=localhost:6379

# Cluster Mode (target)
RELAIS_STORAGE_TYPE=redis
RELAIS_STORAGE_REDIS_URL=node1:6379,node2:6379,node3:6379
```

## Remote Runner Management

### Current Plugin Manager Analysis

The existing `PluginManager` in `pkg/plugins/pluginutil.go` provides:
- Local plugin lifecycle management
- Status tracking and health monitoring
- Thread-safe operations
- Integration with plugin registry

### Enhanced Plugin Manager Design

#### 1. Remote Runner Support

Extend `PluginManager` to handle remote runners:

```go
// Enhanced PluginManager with remote capabilities
type PluginManager struct {
    mu       sync.RWMutex
    registry *Registry              // Existing local plugin registry
    status   map[string]*PluginStatus // Existing local status
    
    // New remote runner support
    remoteRunners map[string]*RemoteRunner
    httpClients   map[string]*http.Client
    healthChecker *HealthChecker
}

type RemoteRunner struct {
    ID       string
    Endpoint string    // http://host:port
    Type     PluginType // ingress, egress, transform
    Status   string    // online, offline, error
    LastSeen time.Time
    Metadata map[string]interface{}
}
```

#### 2. Remote Operations API

Add methods for remote runner management:

```go
// Register a remote runner
func (pm *PluginManager) RegisterRemoteRunner(id, endpoint string, runnerType PluginType) error {
    pm.mu.Lock()
    defer pm.mu.Unlock()
    
    // Validate connectivity
    client := &http.Client{Timeout: 5 * time.Second}
    resp, err := client.Get(endpoint + "/api/v1/health")
    if err != nil {
        return fmt.Errorf("runner unreachable: %v", err)
    }
    defer resp.Body.Close()
    
    pm.remoteRunners[id] = &RemoteRunner{
        ID:       id,
        Endpoint: endpoint,
        Type:     runnerType,
        Status:   "online",
        LastSeen: time.Now(),
    }
    pm.httpClients[id] = client
    
    return nil
}

// Start plugin on remote runner
func (pm *PluginManager) StartRemotePlugin(runnerID, pluginName string, config map[string]interface{}) error {
    pm.mu.RLock()
    runner, exists := pm.remoteRunners[runnerID]
    client := pm.httpClients[runnerID]
    pm.mu.RUnlock()
    
    if !exists {
        return fmt.Errorf("runner not found: %s", runnerID)
    }
    
    // HTTP POST to remote runner
    payload := map[string]interface{}{
        "plugin": pluginName,
        "config": config,
    }
    
    jsonData, _ := json.Marshal(payload)
    resp, err := client.Post(
        runner.Endpoint+"/api/v1/plugins/start",
        "application/json",
        bytes.NewBuffer(jsonData),
    )
    
    if err != nil {
        pm.markRunnerOffline(runnerID)
        return err
    }
    defer resp.Body.Close()
    
    if resp.StatusCode != http.StatusOK {
        return fmt.Errorf("remote plugin start failed: %s", resp.Status)
    }
    
    return nil
}
```

#### 3. Health Monitoring

Implement continuous health checking:

```go
type HealthChecker struct {
    pm       *PluginManager
    interval time.Duration
    ctx      context.Context
    cancel   context.CancelFunc
}

func (hc *HealthChecker) Start() {
    hc.ctx, hc.cancel = context.WithCancel(context.Background())
    
    go func() {
        ticker := time.NewTicker(hc.interval)
        defer ticker.Stop()
        
        for {
            select {
            case <-hc.ctx.Done():
                return
            case <-ticker.C:
                hc.checkAllRunners()
            }
        }
    }()
}

func (hc *HealthChecker) checkAllRunners() {
    hc.pm.mu.RLock()
    runners := make(map[string]*RemoteRunner)
    for id, runner := range hc.pm.remoteRunners {
        runners[id] = runner
    }
    hc.pm.mu.RUnlock()
    
    for id, runner := range runners {
        if err := hc.checkRunner(id, runner); err != nil {
            hc.pm.markRunnerOffline(id)
        } else {
            hc.pm.markRunnerOnline(id)
        }
    }
}
```

### Runner Discovery

Implement service discovery for automatic runner registration:

```go
// Service discovery interface
type ServiceDiscovery interface {
    RegisterRunner(id, endpoint string, runnerType PluginType) error
    DiscoverRunners() ([]RemoteRunner, error)
    Watch(callback func(event DiscoveryEvent)) error
}

// Kubernetes service discovery implementation
type KubernetesDiscovery struct {
    namespace string
    selector  string
}

func (kd *KubernetesDiscovery) DiscoverRunners() ([]RemoteRunner, error) {
    // List Kubernetes services with label selector
    // Extract endpoints and runner types from annotations
    // Return discovered runners
}
```

## Control Plane API Enhancement

### Current API Analysis

The existing control plane in `pkg/server/controlplane.go` provides:
- Basic HTTP routing setup
- Partial session management endpoints
- Stub implementations for plugin management

### Complete API Implementation

#### 1. Enhanced Control Plane Structure

```go
// Enhanced ControlPlane with full functionality
type ControlPlane struct {
    sessionMgr   *SessionManager
    pluginMgr    *plugins.PluginManager
    storage      storage.Storage
    metrics      *metrics.Collector
    
    // Enhanced with remote management
    discovery    ServiceDiscovery
    healthChecker *HealthChecker
}

func NewControlPlane(sessionMgr *SessionManager, pluginMgr *plugins.PluginManager, storage storage.Storage) *ControlPlane {
    return &ControlPlane{
        sessionMgr:    sessionMgr,
        pluginMgr:     pluginMgr,
        storage:       storage,
        metrics:       metrics.NewCollector(),
        discovery:     NewKubernetesDiscovery("relais", "app=relais-runner"),
        healthChecker: NewHealthChecker(pluginMgr, 30*time.Second),
    }
}
```

#### 2. Complete API Endpoints

```go
func (cp *ControlPlane) RegisterRoutes(mux *http.ServeMux) {
    // Existing session management
    mux.HandleFunc("/api/v1/sessions", cp.handleSessions)
    mux.HandleFunc("/api/v1/sessions/", cp.handleSession)
    
    // Enhanced plugin management
    mux.HandleFunc("/api/v1/plugins", cp.handlePlugins)
    mux.HandleFunc("/api/v1/plugins/", cp.handlePlugin)
    
    // New runner management
    mux.HandleFunc("/api/v1/runners", cp.handleRunners)
    mux.HandleFunc("/api/v1/runners/", cp.handleRunner)
    
    // System metrics
    mux.HandleFunc("/api/v1/metrics", cp.handleMetrics)
    mux.HandleFunc("/api/v1/health", cp.handleHealth)
    
    // Prometheus metrics
    mux.Handle("/metrics", promhttp.Handler())
}
```

#### 3. Runner Management Endpoints

```go
// GET /api/v1/runners - List all runners (local and remote)
func (cp *ControlPlane) handleRunners(w http.ResponseWriter, r *http.Request) {
    switch r.Method {
    case http.MethodGet:
        runners := cp.pluginMgr.GetAllRunners()
        
        response := map[string]interface{}{
            "local_runners":  runners.Local,
            "remote_runners": runners.Remote,
            "total_count":    len(runners.Local) + len(runners.Remote),
        }
        
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(response)
        
    case http.MethodPost:
        // Register new remote runner
        var req struct {
            ID       string `json:"id"`
            Endpoint string `json:"endpoint"`
            Type     string `json:"type"`
        }
        
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
            http.Error(w, err.Error(), http.StatusBadRequest)
            return
        }
        
        runnerType := plugins.PluginType(req.Type)
        if err := cp.pluginMgr.RegisterRemoteRunner(req.ID, req.Endpoint, runnerType); err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        
        w.WriteHeader(http.StatusCreated)
        json.NewEncoder(w).Encode(map[string]string{"status": "registered"})
    
    default:
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
    }
}

// POST /api/v1/runners/{id}/start - Start plugin on specific runner
func (cp *ControlPlane) handleRunnerStart(w http.ResponseWriter, r *http.Request) {
    runnerID := extractRunnerID(r.URL.Path)
    
    var req struct {
        Plugin string                 `json:"plugin"`
        Config map[string]interface{} `json:"config"`
    }
    
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    
    if err := cp.pluginMgr.StartRemotePlugin(runnerID, req.Plugin, req.Config); err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    
    json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}
```

#### 4. Enhanced Session Management

```go
// Complete implementation of session endpoints
func (cp *ControlPlane) handleSession(w http.ResponseWriter, r *http.Request) {
    sessionID := extractSessionID(r.URL.Path)
    
    switch r.Method {
    case http.MethodGet:
        session, exists := cp.sessionMgr.GetSession(sessionID)
        if !exists {
            http.Error(w, "Session not found", http.StatusNotFound)
            return
        }
        
        // Enhance with storage statistics
        frames, err := cp.storage.ListFrames(r.Context(), sessionID)
        if err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        
        response := map[string]interface{}{
            "session":     session,
            "frame_count": len(frames),
            "storage_stats": map[string]interface{}{
                "latest_frame": getLatestFrameTimestamp(frames),
                "total_size":   calculateTotalSize(frames),
            },
        }
        
        json.NewEncoder(w).Encode(response)
        
    case http.MethodDelete:
        if err := cp.sessionMgr.CleanupSession(r.Context(), sessionID); err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        
        if err := cp.storage.DeleteSession(r.Context(), sessionID); err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        
        w.WriteHeader(http.StatusNoContent)
        
    default:
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
    }
}
```

## Monitoring and Observability

### Metrics Collection Framework

#### 1. Metrics Package Structure

Create comprehensive metrics collection in `pkg/metrics/`:

```go
// pkg/metrics/collector.go
type Collector struct {
    // Prometheus metrics
    runnerCount      prometheus.GaugeVec
    sessionCount     prometheus.Gauge
    frameCount       prometheus.CounterVec
    frameSize        prometheus.HistogramVec
    apiDuration      prometheus.HistogramVec
    storageOperations prometheus.CounterVec
    
    // Internal state
    registry *prometheus.Registry
}

func NewCollector() *Collector {
    registry := prometheus.NewRegistry()
    
    collector := &Collector{
        runnerCount: *prometheus.NewGaugeVec(
            prometheus.GaugeOpts{
                Name: "relais_runners_total",
                Help: "Total number of runners by type and status",
            },
            []string{"type", "status"},
        ),
        
        sessionCount: prometheus.NewGauge(
            prometheus.GaugeOpts{
                Name: "relais_sessions_active",
                Help: "Number of active media sessions",
            },
        ),
        
        frameCount: *prometheus.NewCounterVec(
            prometheus.CounterOpts{
                Name: "relais_frames_total",
                Help: "Total number of frames processed",
            },
            []string{"session_id", "media_type"},
        ),
        
        registry: registry,
    }
    
    // Register all metrics
    registry.MustRegister(collector.runnerCount)
    registry.MustRegister(collector.sessionCount)
    registry.MustRegister(collector.frameCount)
    
    return collector
}
```

#### 2. Storage Metrics Integration

Enhance storage operations with metrics:

```go
// Enhanced storage with metrics
type MetricsStorage struct {
    storage.Storage
    metrics *metrics.Collector
}

func (ms *MetricsStorage) PutFrame(ctx context.Context, frame storage.Frame) error {
    start := time.Now()
    err := ms.Storage.PutFrame(ctx, frame)
    
    // Record metrics
    ms.metrics.RecordStorageOperation("put_frame", err == nil)
    ms.metrics.RecordFrameSize(frame.MediaType, len(frame.Data))
    ms.metrics.RecordOperationDuration("put_frame", time.Since(start))
    
    return err
}
```

#### 3. Plugin Metrics

Add metrics to plugin operations:

```go
// Enhanced plugin manager with metrics
func (pm *PluginManager) StartRemotePlugin(runnerID, pluginName string, config map[string]interface{}) error {
    start := time.Now()
    err := pm.startRemotePluginImpl(runnerID, pluginName, config)
    
    // Record metrics
    status := "success"
    if err != nil {
        status = "error"
    }
    
    pm.metrics.RecordPluginOperation("start", pluginName, status)
    pm.metrics.RecordOperationDuration("plugin_start", time.Since(start))
    
    return err
}
```

### Logging Strategy

#### 1. Structured Logging

Enhance existing logging with structured format:

```go
// pkg/logging/logger.go enhancement
func (l *Logger) WithFields(fields map[string]interface{}) *Logger {
    return &Logger{
        logger: l.logger.WithFields(logrus.Fields(fields)),
    }
}

// Usage in components
func (pm *PluginManager) StartRemotePlugin(runnerID, pluginName string, config map[string]interface{}) error {
    logger := pm.logger.WithFields(map[string]interface{}{
        "runner_id":   runnerID,
        "plugin_name": pluginName,
        "operation":   "start_remote_plugin",
    })
    
    logger.Info("Starting remote plugin")
    
    err := pm.startRemotePluginImpl(runnerID, pluginName, config)
    if err != nil {
        logger.WithField("error", err.Error()).Error("Failed to start remote plugin")
        return err
    }
    
    logger.Info("Remote plugin started successfully")
    return nil
}
```

#### 2. Log Aggregation

Prepare for centralized logging:

```go
// JSON log format for aggregation
func NewJSONLogger(level string) *Logger {
    logger := logrus.New()
    logger.SetFormatter(&logrus.JSONFormatter{
        TimestampFormat: time.RFC3339,
        FieldMap: logrus.FieldMap{
            logrus.FieldKeyTime:  "@timestamp",
            logrus.FieldKeyLevel: "@level",
            logrus.FieldKeyMsg:   "@message",
        },
    })
    
    return &Logger{logger: logger}
}
```

### Health Monitoring

#### 1. Health Check Framework

```go
// pkg/health/checker.go
type HealthChecker struct {
    checks map[string]HealthCheck
    mu     sync.RWMutex
}

type HealthCheck interface {
    Name() string
    Check(ctx context.Context) error
}

type StorageHealthCheck struct {
    storage storage.Storage
}

func (shc *StorageHealthCheck) Check(ctx context.Context) error {
    // Test storage connectivity
    sessions, err := shc.storage.ListSessions(ctx)
    if err != nil {
        return fmt.Errorf("storage check failed: %v", err)
    }
    
    return nil
}
```

#### 2. Health API Endpoint

```go
func (cp *ControlPlane) handleHealth(w http.ResponseWriter, r *http.Request) {
    ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
    defer cancel()
    
    results := make(map[string]interface{})
    overall := "healthy"
    
    for name, check := range cp.healthChecker.GetChecks() {
        if err := check.Check(ctx); err != nil {
            results[name] = map[string]interface{}{
                "status": "unhealthy",
                "error":  err.Error(),
            }
            overall = "unhealthy"
        } else {
            results[name] = map[string]interface{}{
                "status": "healthy",
            }
        }
    }
    
    response := map[string]interface{}{
        "overall": overall,
        "checks":  results,
        "timestamp": time.Now(),
    }
    
    status := http.StatusOK
    if overall != "healthy" {
        status = http.StatusServiceUnavailable
    }
    
    w.WriteHeader(status)
    json.NewEncoder(w).Encode(response)
}
```

## Deployment and Orchestration

### Container Strategy

#### 1. Multi-Stage Docker Builds

Create efficient Docker images for each component:

```dockerfile
# deployments/docker/Dockerfile.base
FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Build all binaries
RUN make build

# Runtime image
FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/

# Common dependencies
COPY --from=builder /app/bin/* ./
```

```dockerfile
# deployments/docker/Dockerfile.core
FROM relais-base
EXPOSE 8080
CMD ["./relais-core"]
```

```dockerfile
# deployments/docker/Dockerfile.runner
FROM relais-base
# Can run any runner type via command argument
CMD ["./relais-ingress-runner"]
```

```dockerfile
# deployments/docker/Dockerfile.manager
FROM relais-base
# TUI manager for interactive use
CMD ["./relais-manager"]
```

#### 2. Docker Compose for Local Development

```yaml
# deployments/compose/docker-compose.yml
version: '3.8'

services:
  redis-cluster:
    image: redis:7-alpine
    command: redis-server --cluster-enabled yes --cluster-config-file nodes.conf --cluster-node-timeout 5000 --appendonly yes
    ports:
      - "7000-7005:7000-7005"
    deploy:
      replicas: 6

  relais-core:
    build:
      context: ../../
      dockerfile: deployments/docker/Dockerfile.core
    ports:
      - "8080:8080"
    environment:
      RELAIS_STORAGE_TYPE: redis
      RELAIS_STORAGE_REDIS_URL: redis-node-1:7000,redis-node-2:7001,redis-node-3:7002
      RELAIS_SERVER_HOST: 0.0.0.0
      RELAIS_SERVER_PORT: 8080
    depends_on:
      - redis-cluster

  ingress-runner:
    build:
      context: ../../
      dockerfile: deployments/docker/Dockerfile.runner
    command: ["./relais-ingress-runner", "--type", "camera"]
    environment:
      RELAIS_STORAGE_TYPE: redis
      RELAIS_STORAGE_REDIS_URL: redis-node-1:7000,redis-node-2:7001,redis-node-3:7002
    deploy:
      replicas: 2

  egress-runner:
    build:
      context: ../../
      dockerfile: deployments/docker/Dockerfile.runner
    command: ["./relais-egress-runner", "--type", "webrtc"]
    environment:
      RELAIS_STORAGE_TYPE: redis
      RELAIS_STORAGE_REDIS_URL: redis-node-1:7000,redis-node-2:7001,redis-node-3:7002
    deploy:
      replicas: 2

  manager:
    build:
      context: ../../
      dockerfile: deployments/docker/Dockerfile.manager
    environment:
      RELAIS_CORE_ENDPOINT: http://relais-core:8080
    stdin_open: true
    tty: true
```

### Kubernetes Orchestration

#### 1. Core Deployment

```yaml
# deployments/kubernetes/core-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: relais-core
  labels:
    app: relais-core
spec:
  replicas: 3
  selector:
    matchLabels:
      app: relais-core
  template:
    metadata:
      labels:
        app: relais-core
    spec:
      containers:
      - name: relais-core
        image: relais/core:latest
        ports:
        - containerPort: 8080
        env:
        - name: RELAIS_STORAGE_TYPE
          value: "redis"
        - name: RELAIS_STORAGE_REDIS_URL
          value: "redis-cluster:6379"
        - name: RELAIS_SERVER_HOST
          value: "0.0.0.0"
        - name: RELAIS_SERVER_PORT
          value: "8080"
        livenessProbe:
          httpGet:
            path: /api/v1/health
            port: 8080
          initialDelaySeconds: 30
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /api/v1/health
            port: 8080
          initialDelaySeconds: 5
          periodSeconds: 5
        resources:
          requests:
            memory: "256Mi"
            cpu: "250m"
          limits:
            memory: "512Mi"
            cpu: "500m"

---
apiVersion: v1
kind: Service
metadata:
  name: relais-core-service
spec:
  selector:
    app: relais-core
  ports:
  - protocol: TCP
    port: 8080
    targetPort: 8080
  type: LoadBalancer
```

#### 2. Runner Deployments

```yaml
# deployments/kubernetes/ingress-runner-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: relais-ingress-runner
  labels:
    app: relais-runner
    runner-type: ingress
spec:
  replicas: 5
  selector:
    matchLabels:
      app: relais-runner
      runner-type: ingress
  template:
    metadata:
      labels:
        app: relais-runner
        runner-type: ingress
      annotations:
        relais.io/plugin-type: "ingress"
        relais.io/auto-register: "true"
    spec:
      containers:
      - name: ingress-runner
        image: relais/runner:latest
        command: ["./relais-ingress-runner"]
        args: ["--type", "camera"]
        env:
        - name: RELAIS_STORAGE_TYPE
          value: "redis"
        - name: RELAIS_STORAGE_REDIS_URL
          value: "redis-cluster:6379"
        - name: RELAIS_CORE_ENDPOINT
          value: "http://relais-core-service:8080"
        - name: RUNNER_ID
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        resources:
          requests:
            memory: "128Mi"
            cpu: "100m"
          limits:
            memory: "256Mi"
            cpu: "200m"

---
apiVersion: v1
kind: Service
metadata:
  name: relais-ingress-runner-service
  labels:
    app: relais-runner
    runner-type: ingress
spec:
  selector:
    app: relais-runner
    runner-type: ingress
  ports:
  - protocol: TCP
    port: 8080
    targetPort: 8080
  clusterIP: None  # Headless service for individual pod discovery
```

#### 3. Auto-scaling Configuration

```yaml
# deployments/kubernetes/hpa.yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: relais-core-hpa
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: relais-core
  minReplicas: 3
  maxReplicas: 10
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 70
  - type: Resource
    resource:
      name: memory
      target:
        type: Utilization
        averageUtilization: 80

---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: relais-ingress-runner-hpa
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: relais-ingress-runner
  minReplicas: 2
  maxReplicas: 20
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 60
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 60
      policies:
      - type: Percent
        value: 100
        periodSeconds: 15
    scaleDown:
      stabilizationWindowSeconds: 300
      policies:
      - type: Percent
        value: 50
        periodSeconds: 60
```

### Service Discovery Integration

#### 1. Kubernetes Service Discovery

```go
// pkg/discovery/kubernetes.go
type KubernetesDiscovery struct {
    clientset kubernetes.Interface
    namespace string
    selector  string
}

func (kd *KubernetesDiscovery) DiscoverRunners() ([]RemoteRunner, error) {
    // List pods with runner labels
    pods, err := kd.clientset.CoreV1().Pods(kd.namespace).List(context.Background(), metav1.ListOptions{
        LabelSelector: kd.selector,
    })
    if err != nil {
        return nil, err
    }
    
    var runners []RemoteRunner
    for _, pod := range pods.Items {
        if pod.Status.Phase == corev1.PodRunning {
            runnerType := pod.Annotations["relais.io/plugin-type"]
            endpoint := fmt.Sprintf("http://%s:8080", pod.Status.PodIP)
            
            runners = append(runners, RemoteRunner{
                ID:       pod.Name,
                Endpoint: endpoint,
                Type:     plugins.PluginType(runnerType),
                Status:   "online",
                LastSeen: time.Now(),
                Metadata: map[string]interface{}{
                    "pod_name":  pod.Name,
                    "node_name": pod.Spec.NodeName,
                },
            })
        }
    }
    
    return runners, nil
}
```

#### 2. Auto-Registration

```go
// Runner auto-registration on startup
func (runner *Runner) registerWithCore() error {
    coreEndpoint := os.Getenv("RELAIS_CORE_ENDPOINT")
    runnerID := os.Getenv("RUNNER_ID")
    
    if coreEndpoint == "" || runnerID == "" {
        return nil // Skip registration if not configured
    }
    
    client := &http.Client{Timeout: 10 * time.Second}
    
    payload := map[string]interface{}{
        "id":       runnerID,
        "endpoint": fmt.Sprintf("http://%s:8080", getLocalIP()),
        "type":     runner.Type,
    }
    
    jsonData, _ := json.Marshal(payload)
    resp, err := client.Post(
        coreEndpoint+"/api/v1/runners",
        "application/json",
        bytes.NewBuffer(jsonData),
    )
    
    if err != nil {
        return fmt.Errorf("failed to register with core: %v", err)
    }
    defer resp.Body.Close()
    
    if resp.StatusCode != http.StatusCreated {
        return fmt.Errorf("registration failed: %s", resp.Status)
    }
    
    return nil
}
```

## Implementation Phases

### Phase 1: Foundation Enhancement (Weeks 1-2)

**Objective**: Enhance existing components to support clustering and remote management.

**Tasks**:
1. **Redis Storage Enhancement**
   - Add cluster mode detection to `pkg/storage/redis.go`
   - Implement Redis cluster client support
   - Test backward compatibility with single-node Redis
   - Update configuration to support cluster addresses

2. **Plugin Manager Enhancement**
   - Extend `PluginManager` with remote runner support
   - Add HTTP client capabilities for remote operations
   - Implement health checking framework
   - Create service discovery interfaces

3. **Control Plane API Completion**
   - Complete existing endpoint implementations
   - Add runner management endpoints
   - Add metrics and health endpoints
   - Integrate with enhanced plugin manager

**Success Criteria**:
- [ ] Redis cluster mode works with existing storage interface
- [ ] Plugin manager can register and manage remote runners
- [ ] Control plane APIs return proper responses
- [ ] All existing functionality remains working
- [ ] Health checks pass for all components

**Validation Commands**:
```bash
# Test Redis cluster compatibility
go test ./pkg/storage -v

# Test plugin manager enhancements
go test ./pkg/plugins -v

# Test API endpoints
curl http://localhost:8080/api/v1/health
curl http://localhost:8080/api/v1/runners
```

### Phase 2: TUI Management Interface (Weeks 3-4)

**Objective**: Create a comprehensive TUI for system management.

**Tasks**:
1. **TUI Package Creation**
   - Create `pkg/tui/` with Bubble Tea application structure
   - Implement Model-Update-View pattern
   - Create reusable UI components using Bubbles

2. **TUI Application Development**
   - Create `cmd/relais-manager/main.go`
   - Implement HTTP client for API communication
   - Create views for runners, sessions, metrics
   - Add real-time updates and navigation

3. **TUI Features**
   - Runner list with start/stop/restart actions
   - Session management and monitoring
   - Metrics dashboard with real-time updates
   - Log viewer with filtering and search

**Success Criteria**:
- [ ] TUI application starts and connects to core API
- [ ] All views render correctly and update in real-time
- [ ] User can manage runners through TUI
- [ ] Session information displays accurately
- [ ] Metrics update every 5 seconds
- [ ] Navigation between views works smoothly

**Validation Commands**:
```bash
# Build and run TUI manager
make build
./bin/relais-manager

# Test TUI API integration
curl http://localhost:8080/api/v1/runners | jq .
```

### Phase 3: Monitoring and Metrics (Weeks 5-6)

**Objective**: Implement comprehensive monitoring and observability.

**Tasks**:
1. **Metrics Collection**
   - Create `pkg/metrics/` with Prometheus metrics
   - Instrument storage operations
   - Add plugin and API metrics
   - Implement custom metrics for media processing

2. **Health Monitoring**
   - Create health check framework
   - Implement storage, plugin, and system health checks
   - Add health API endpoints
   - Integrate with existing components

3. **Logging Enhancement**
   - Add structured logging with fields
   - Implement JSON log format for aggregation
   - Add request tracing and correlation IDs
   - Create log aggregation strategy

**Success Criteria**:
- [ ] Prometheus metrics endpoint exports all key metrics
- [ ] Health checks accurately reflect system status
- [ ] Structured logs provide useful debugging information
- [ ] Metrics integrate with TUI dashboard
- [ ] Performance impact is minimal (<5% overhead)

**Validation Commands**:
```bash
# Check Prometheus metrics
curl http://localhost:8080/metrics

# Test health endpoint
curl http://localhost:8080/api/v1/health | jq .

# Validate metrics collection
go test ./pkg/metrics -v
```

### Phase 4: Containerization and Orchestration (Weeks 7-8)

**Objective**: Enable containerized deployment with Kubernetes orchestration.

**Tasks**:
1. **Container Images**
   - Create multi-stage Dockerfiles for each component
   - Optimize image sizes and security
   - Implement proper signal handling for containers
   - Create image build and push pipeline

2. **Docker Compose**
   - Create local development environment
   - Include Redis cluster setup
   - Configure service networking
   - Add development convenience features

3. **Kubernetes Manifests**
   - Create deployments for core and runners
   - Implement service discovery integration
   - Add auto-scaling configurations
   - Create monitoring and alerting setup

**Success Criteria**:
- [ ] All components build into secure, minimal containers
- [ ] Docker Compose environment starts and works locally
- [ ] Kubernetes deployments scale automatically
- [ ] Service discovery registers runners automatically
- [ ] Health checks integrate with Kubernetes probes
- [ ] Auto-scaling responds to load changes

**Validation Commands**:
```bash
# Test Docker builds
docker build -f deployments/docker/Dockerfile.core .

# Test local environment
docker-compose -f deployments/compose/docker-compose.yml up

# Test Kubernetes deployment
kubectl apply -f deployments/kubernetes/
kubectl get pods -l app=relais-core
```

### Phase 5: Production Readiness (Weeks 9-10)

**Objective**: Ensure system is ready for production deployment.

**Tasks**:
1. **Security Hardening**
   - Implement authentication and authorization
   - Add TLS/SSL support for all communications
   - Security scanning of container images
   - Network security policies

2. **Performance Optimization**
   - Optimize Redis cluster configuration
   - Tune connection pools and timeouts
   - Implement efficient frame batching
   - Add performance benchmarking

3. **Operational Procedures**
   - Create deployment runbooks
   - Implement backup and recovery procedures
   - Add monitoring alerts and escalation
   - Create troubleshooting guides

**Success Criteria**:
- [ ] All communications use TLS encryption
- [ ] Authentication required for administrative operations
- [ ] Performance meets or exceeds current single-node setup
- [ ] Backup and recovery procedures tested
- [ ] Monitoring alerts fire correctly for various failure scenarios
- [ ] Complete documentation available for operators

**Validation Commands**:
```bash
# Security scan
docker scan relais/core:latest

# Performance benchmark
make bench

# Test backup and recovery
./scripts/backup-test.sh
```

## Operational Procedures

### Deployment Guide

#### 1. Infrastructure Prerequisites

**Redis Cluster Setup**:
```bash
# Create Redis cluster with 6 nodes (3 masters, 3 replicas)
for port in {7000..7005}; do
  redis-server --port $port --cluster-enabled yes \
    --cluster-config-file nodes-${port}.conf \
    --cluster-node-timeout 5000 \
    --appendonly yes --daemonize yes
done

# Initialize cluster
redis-cli --cluster create 127.0.0.1:7000 127.0.0.1:7001 \
  127.0.0.1:7002 127.0.0.1:7003 127.0.0.1:7004 127.0.0.1:7005 \
  --cluster-replicas 1
```

**Kubernetes Cluster**:
```bash
# Minimum requirements
# - Kubernetes 1.20+
# - 3+ nodes with 4GB RAM each
# - LoadBalancer support
# - Persistent volume support

# Install required components
kubectl apply -f https://raw.githubusercontent.com/kubernetes/dashboard/v2.0.0/aio/deploy/recommended.yaml
helm install prometheus prometheus-community/kube-prometheus-stack
```

#### 2. Application Deployment

**Step 1: Deploy Redis Cluster**
```bash
kubectl apply -f deployments/kubernetes/redis-cluster.yaml
kubectl wait --for=condition=ready pod -l app=redis-cluster --timeout=300s
```

**Step 2: Deploy Relais Core**
```bash
kubectl apply -f deployments/kubernetes/core-deployment.yaml
kubectl wait --for=condition=available deployment/relais-core --timeout=300s
```

**Step 3: Deploy Runners**
```bash
kubectl apply -f deployments/kubernetes/ingress-runner-deployment.yaml
kubectl apply -f deployments/kubernetes/egress-runner-deployment.yaml
kubectl apply -f deployments/kubernetes/transform-runner-deployment.yaml
```

**Step 4: Verify Deployment**
```bash
# Check all pods are running
kubectl get pods -l app=relais-core
kubectl get pods -l app=relais-runner

# Check service discovery
kubectl logs -l app=relais-core | grep "runner registered"

# Test API endpoints
kubectl port-forward svc/relais-core-service 8080:8080 &
curl http://localhost:8080/api/v1/health
curl http://localhost:8080/api/v1/runners
```

### Monitoring and Alerting

#### 1. Key Metrics to Monitor

**System Health**:
- Pod/container health and restarts
- Redis cluster node availability
- API response times and error rates
- Resource utilization (CPU, memory, network)

**Application Metrics**:
- Active session count
- Frame processing rate
- Storage operations per second
- Runner availability and health

**Alert Thresholds**:
```yaml
# prometheus-alerts.yaml
groups:
- name: relais-alerts
  rules:
  - alert: RelaisCoreDown
    expr: up{job="relais-core"} == 0
    for: 1m
    labels:
      severity: critical
    annotations:
      summary: "Relais core is down"
      description: "Relais core has been down for more than 1 minute"

  - alert: RedisClusterNodeDown
    expr: redis_cluster_nodes{state="ok"} < 6
    for: 2m
    labels:
      severity: warning
    annotations:
      summary: "Redis cluster node down"

  - alert: HighFrameProcessingLatency
    expr: histogram_quantile(0.95, relais_frame_processing_duration_seconds) > 0.1
    for: 5m
    labels:
      severity: warning
    annotations:
      summary: "High frame processing latency"
```

#### 2. Grafana Dashboards

**System Overview Dashboard**:
- Cluster health status
- Active sessions and runners
- Frame processing throughput
- Resource utilization trends

**Performance Dashboard**:
- API response time percentiles
- Storage operation latencies
- Redis cluster performance
- Network throughput

### Troubleshooting Guide

#### 1. Common Issues

**Redis Cluster Connection Issues**:
```bash
# Check cluster status
redis-cli --cluster check localhost:7000

# Fix cluster if nodes are down
redis-cli --cluster fix localhost:7000

# Verify Relais can connect
kubectl logs -l app=relais-core | grep redis
```

**Runner Registration Problems**:
```bash
# Check service discovery
kubectl get pods -l app=relais-runner -o wide
kubectl logs -l app=relais-core | grep "runner registration"

# Manual runner registration
curl -X POST http://core-endpoint:8080/api/v1/runners \
  -H "Content-Type: application/json" \
  -d '{"id":"manual-runner","endpoint":"http://runner-ip:8080","type":"ingress"}'
```

**TUI Connection Issues**:
```bash
# Check core API accessibility
kubectl port-forward svc/relais-core-service 8080:8080
curl http://localhost:8080/api/v1/health

# Run TUI with debug logging
RELAIS_LOGGING_LEVEL=debug ./bin/relais-manager
```

#### 2. Performance Debugging

**High Memory Usage**:
```bash
# Check frame retention policies
kubectl exec -it deployment/relais-core -- \
  curl localhost:8080/api/v1/metrics | grep relais_frames

# Analyze session cleanup
kubectl logs -l app=relais-core | grep "session cleanup"
```

**Slow Frame Processing**:
```bash
# Check Redis performance
redis-cli --latency -h redis-cluster-endpoint

# Analyze storage metrics
kubectl exec -it deployment/relais-core -- \
  curl localhost:8080/api/v1/metrics | grep storage_operations
```

### Backup and Recovery

#### 1. Redis Cluster Backup

```bash
# Create consistent backup
for node in {7000..7005}; do
  redis-cli -p $node BGSAVE
done

# Copy RDB files to backup location
kubectl exec -it redis-cluster-0 -- cp /data/dump.rdb /backup/
```

#### 2. Configuration Backup

```bash
# Backup Kubernetes configs
kubectl get configmaps,secrets -o yaml > relais-config-backup.yaml

# Backup custom resource definitions
kubectl get deployment,service,hpa -l app=relais -o yaml > relais-resources-backup.yaml
```

#### 3. Recovery Procedures

**Redis Cluster Recovery**:
```bash
# Stop cluster
kubectl scale deployment redis-cluster --replicas=0

# Restore from backup
kubectl exec -it redis-cluster-0 -- cp /backup/dump.rdb /data/

# Restart cluster
kubectl scale deployment redis-cluster --replicas=6
```

**Application Recovery**:
```bash
# Rolling restart of core
kubectl rollout restart deployment/relais-core

# Restart runners (they will auto-register)
kubectl rollout restart deployment/relais-ingress-runner
kubectl rollout restart deployment/relais-egress-runner
```

## Security Considerations

### Authentication and Authorization

#### 1. API Security

**JWT Token Authentication**:
```go
// pkg/auth/jwt.go
type JWTAuth struct {
    secret []byte
    issuer string
}

func (ja *JWTAuth) ValidateToken(tokenString string) (*Claims, error) {
    token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
        return ja.secret, nil
    })
    
    if err != nil {
        return nil, err
    }
    
    if claims, ok := token.Claims.(*Claims); ok && token.Valid {
        return claims, nil
    }
    
    return nil, fmt.Errorf("invalid token")
}
```

**Role-Based Access Control**:
```go
// API middleware for authorization
func (cp *ControlPlane) requireRole(role string, handler http.HandlerFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        claims, err := cp.auth.ValidateToken(r.Header.Get("Authorization"))
        if err != nil {
            http.Error(w, "Unauthorized", http.StatusUnauthorized)
            return
        }
        
        if !claims.HasRole(role) {
            http.Error(w, "Forbidden", http.StatusForbidden)
            return
        }
        
        handler(w, r)
    }
}

// Usage
mux.HandleFunc("/api/v1/runners", cp.requireRole("admin", cp.handleRunners))
```

#### 2. TLS/SSL Configuration

**TLS for All Communications**:
```go
// Enhanced server with TLS
func (cp *ControlPlane) StartTLSServer(certFile, keyFile string) error {
    server := &http.Server{
        Addr:    ":8443",
        Handler: cp.mux,
        TLSConfig: &tls.Config{
            MinVersion: tls.VersionTLS12,
            CipherSuites: []uint16{
                tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
                tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
            },
        },
    }
    
    return server.ListenAndServeTLS(certFile, keyFile)
}
```

**Certificate Management**:
```yaml
# cert-manager integration for Kubernetes
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: relais-tls
spec:
  secretName: relais-tls-secret
  issuerRef:
    name: letsencrypt-prod
    kind: ClusterIssuer
  dnsNames:
  - relais.example.com
```

### Network Security

#### 1. Network Policies

```yaml
# deployments/kubernetes/network-policy.yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: relais-core-policy
spec:
  podSelector:
    matchLabels:
      app: relais-core
  policyTypes:
  - Ingress
  - Egress
  ingress:
  - from:
    - podSelector:
        matchLabels:
          app: relais-runner
    - podSelector:
        matchLabels:
          app: relais-manager
    ports:
    - protocol: TCP
      port: 8080
  egress:
  - to:
    - podSelector:
        matchLabels:
          app: redis-cluster
    ports:
    - protocol: TCP
      port: 6379
```

#### 2. Pod Security Standards

```yaml
# Pod security context
apiVersion: v1
kind: Pod
spec:
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
  containers:
  - name: relais-core
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop:
        - ALL
        add:
        - NET_BIND_SERVICE
```

## Conclusion

This comprehensive plan outlines the transformation of Relais from a single-node media server into a horizontally scalable, production-ready system with modern operational capabilities. The evolution maintains backward compatibility while adding:

1. **Horizontal Scaling**: Redis clustering and remote runner management
2. **Operational Excellence**: TUI management, comprehensive monitoring, and health checks  
3. **Container-First Design**: Full Kubernetes integration with auto-scaling
4. **Production Readiness**: Security, backup/recovery, and troubleshooting procedures

The phased implementation approach ensures incremental progress with validation at each step. The foundation built upon the existing solid architecture minimizes risk while maximizing the value of prior development work.

Key success factors:
- **Leverage Existing Strengths**: Build upon the well-designed plugin system and storage abstraction
- **Incremental Enhancement**: Avoid big-bang rewrites by extending existing components
- **Operational Focus**: Prioritize monitoring, management, and troubleshooting capabilities
- **Container Integration**: Design for cloud-native deployment from the start
- **Security by Design**: Implement authentication, authorization, and encryption throughout

The result will be a media server capable of handling enterprise-scale workloads while remaining easy to deploy and manage through both automated orchestration and interactive TUI management.