package plugins

import (
	"fmt"
	"sync"
)

// PluginType represents the type of plugin
type PluginType string

const (
	PluginTypeIngress   PluginType = "ingress"
	PluginTypeEgress    PluginType = "egress"
	PluginTypeTransform PluginType = "transform"
)

// PluginFactory creates a new plugin instance
type PluginFactory func() Plugin

// Registry manages plugin registration and creation
type Registry struct {
	mu      sync.RWMutex
	plugins map[PluginType]map[string]PluginFactory
}

// NewRegistry creates a new plugin registry
func NewRegistry() *Registry {
	return &Registry{
		plugins: make(map[PluginType]map[string]PluginFactory),
	}
}

// Register adds a plugin factory to the registry
func (r *Registry) Register(pType PluginType, name string, factory PluginFactory) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.plugins[pType] == nil {
		r.plugins[pType] = make(map[string]PluginFactory)
	}

	if _, exists := r.plugins[pType][name]; exists {
		return fmt.Errorf("plugin already registered: %s", name)
	}

	r.plugins[pType][name] = factory
	return nil
}

// Create instantiates a new plugin by type and name
func (r *Registry) Create(pType PluginType, name string) (Plugin, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if factories, ok := r.plugins[pType]; ok {
		if factory, ok := factories[name]; ok {
			return factory(), nil
		}
	}

	return nil, fmt.Errorf("plugin not found: %s/%s", pType, name)
}
