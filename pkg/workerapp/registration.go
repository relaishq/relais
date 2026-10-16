package workerapp

import (
	"fmt"
	"unicode/utf8"

	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
)

// Registration gives an agent a stable persisted name and versioned task kinds.
// A pool must use the same registration and restore prior state/task schemas.
type Registration struct {
	Name    string
	Factory agent.Factory
	Tasks   map[string]agent.TaskHandler
	Limits  agent.Limits
}

func registeredAgents(options Options, selected string, limits mediaworker.SnapshotLimits) (mediaworker.AgentConfig, error) {
	factories := make(map[string]legacy.Factory, len(options.Agents))
	for _, r := range options.Agents {
		if r.Name == "" || !utf8.ValidString(r.Name) || r.Name == "echo" || r.Name == "demo" || factories[r.Name] != nil || r.Factory == nil {
			return mediaworker.AgentConfig{}, fmt.Errorf("%w: invalid or duplicate registration %q", agent.ErrIncompatibleAgent, r.Name)
		}
		bounds := agenthost.Bounds(r.Limits)
		// State is a declaration: fail rather than shrink it. Other resource limits
		// can only tighten operator/host ceilings.
		if bounds.MaxPendingBytes > 2048 {
			bounds.MaxPendingBytes = 2048
		}
		if bounds.MaxMailboxBytes > limits.MoveBytesPerSession {
			bounds.MaxMailboxBytes = limits.MoveBytesPerSession
		}
		if bounds.MaxMailboxBytes == 0 {
			bounds.MaxMailboxBytes = limits.MoveBytesPerSession
		}
		if bounds.MaxActiveTasks > 4 {
			bounds.MaxActiveTasks = 4
		}
		if bounds.MaxAudioBytes > bounds.MaxMailboxBytes {
			bounds.MaxAudioBytes = bounds.MaxMailboxBytes
		}
		f := agenthost.Factory(agenthost.Registration{Name: r.Name, Factory: r.Factory, Tasks: r.Tasks, Bounds: bounds})
		resolved := f("").(*agenthost.Adapter).Registration
		if err := mediaworker.ValidateAgentRegistration(resolved.Name, resolved.TaskKinds, agent.Limits(resolved.Bounds), limits); err != nil {
			return mediaworker.AgentConfig{}, err
		}
		for kind, handler := range r.Tasks {
			if kind == "" || !utf8.ValidString(kind) || handler == nil {
				return mediaworker.AgentConfig{}, fmt.Errorf("%w: invalid task kind %q", agent.ErrIncompatibleAgent, kind)
			}
		}
		factories[r.Name] = f
	}
	cfg, err := selectAgent(selected)
	if f, ok := factories[selected]; ok {
		cfg.Factory = f
		err = nil
	}
	if err != nil {
		return cfg, err
	}
	cfg.Registrations = factories
	return cfg, nil
}
