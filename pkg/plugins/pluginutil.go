package plugins

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// PluginStatus represents the current state of a plugin
type PluginStatus struct {
	Running   bool
	StartTime time.Time
	Error     error
}

// PluginManager handles plugin lifecycle
type PluginManager struct {
	mu       sync.RWMutex
	registry *Registry
	status   map[string]*PluginStatus
}

// NewPluginManager creates a new plugin manager
func NewPluginManager(registry *Registry) *PluginManager {
	return &PluginManager{
		registry: registry,
		status:   make(map[string]*PluginStatus),
	}
}

// StartPlugin initializes and starts a plugin
func (pm *PluginManager) StartPlugin(ctx context.Context, pType PluginType, name string, config map[string]interface{}) error {
	plugin, err := pm.registry.Create(pType, name)
	if err != nil {
		return fmt.Errorf("failed to create plugin: %w", err)
	}

	if err := plugin.Initialize(ctx, config); err != nil {
		return fmt.Errorf("failed to initialize plugin: %w", err)
	}

	pm.mu.Lock()
	pm.status[name] = &PluginStatus{
		Running:   true,
		StartTime: time.Now(),
	}
	pm.mu.Unlock()

	return nil
}

// StopPlugin stops a running plugin
func (pm *PluginManager) StopPlugin(name string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	status, exists := pm.status[name]
	if !exists || !status.Running {
		return fmt.Errorf("plugin not running: %s", name)
	}

	status.Running = false
	status.Error = nil
	return nil
}

// GetPluginStatus returns the current status of a plugin
func (pm *PluginManager) GetPluginStatus(name string) (*PluginStatus, error) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	status, exists := pm.status[name]
	if !exists {
		return nil, fmt.Errorf("plugin not found: %s", name)
	}

	return status, nil
}
