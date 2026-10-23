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
	// SkipDrainWait lists task kinds whose ReadOnly handlers do not delay the
	// SIGTERM quiescence wait. SideEffecting tasks always count toward the wait.
	// Listed kinds must be nonempty, valid UTF-8 keys in Tasks. This worker-local
	// setting is not persisted; skipped tasks use the existing urgent export.
	SkipDrainWait []string
	Limits        agent.Limits
}

func registeredAgents(options Options, selected string, limits mediaworker.SnapshotLimits, split ...bool) (mediaworker.AgentConfig, error) {
	factories := make(map[string]legacy.Factory, len(options.Agents))
	for _, r := range options.Agents {
		if r.Name == "" || !utf8.ValidString(r.Name) || r.Name == "echo" || r.Name == "demo" || factories[r.Name] != nil || r.Factory == nil {
			return mediaworker.AgentConfig{}, fmt.Errorf("%w: invalid or duplicate registration %q", agent.ErrIncompatibleAgent, r.Name)
		}
		bounds := agenthost.Bounds(r.Limits)
		f := agenthost.Factory(agenthost.Registration{Name: r.Name, Factory: r.Factory, Tasks: r.Tasks, SkipDrainWait: r.SkipDrainWait, Bounds: bounds})
		for kind, handler := range r.Tasks {
			if kind == "" || !utf8.ValidString(kind) || handler == nil {
				return mediaworker.AgentConfig{}, fmt.Errorf("%w: invalid task kind %q", agent.ErrIncompatibleAgent, kind)
			}
		}
		for _, kind := range r.SkipDrainWait {
			if kind == "" || !utf8.ValidString(kind) || r.Tasks[kind] == nil {
				return mediaworker.AgentConfig{}, fmt.Errorf("%w: invalid skip-drain-wait task kind %q", agent.ErrIncompatibleAgent, kind)
			}
		}
		factories[r.Name] = f
	}
	if err := mediaworker.ValidateSnapshotProfiles(mediaworker.Config{Agent: mediaworker.AgentConfig{Registrations: factories}, SnapshotLimits: limits, SplitCheckpoints: len(split) > 0 && split[0]}); err != nil {
		return mediaworker.AgentConfig{}, err
	}
	cfg, err := selectAgent(selected)
	if f, ok := factories[selected]; ok {
		cfg.Factory = f
		err = nil
	}
	if err != nil {
		return cfg, invalidAgentName(selected, factories)
	}
	cfg.Registrations = factories
	return cfg, nil
}
