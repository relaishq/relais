// Package agenthost carries public workerapp registrations to the v2 host.
package agenthost

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// Bounds are the resolved effective registration limits.
type Bounds struct {
	MaxStateBytes    int
	MaxPendingBytes  int
	MaxMailboxBytes  int
	MaxActiveTasks   int
	MaxAudioBytes    int
	MaxAudioDuration time.Duration
}

type Registration struct {
	Name      string
	Factory   agent.Factory
	TaskKinds []string
	Tasks     map[string]agent.TaskHandler
	Bounds    Bounds
	// Declared retains the app declaration before resolving host defaults.
	Declared agent.Limits
}

// Factory carries a registration through the existing legacy configuration
// field. Only the v2 host unwraps it; it cannot accidentally behave like Echo.
func Factory(r Registration) legacy.Factory {
	r.Declared = agent.Limits(r.Bounds)
	r.Tasks = maps.Clone(r.Tasks)
	r.TaskKinds = slices.Clone(r.TaskKinds)
	for kind := range r.Tasks {
		r.TaskKinds = append(r.TaskKinds, kind)
	}
	slices.Sort(r.TaskKinds)
	r.TaskKinds = slices.Compact(r.TaskKinds)
	r.Bounds = r.Bounds.Defaults()
	return func(string) legacy.Agent { return &Adapter{Registration: r} }
}

// Defaults resolves the host defaults while retaining the original declaration.
func (b Bounds) Defaults() Bounds {
	if b.MaxStateBytes == 0 {
		b.MaxStateBytes = 1 << 10
	}
	if b.MaxPendingBytes == 0 {
		b.MaxPendingBytes = 2 << 10
	}
	if b.MaxMailboxBytes == 0 {
		b.MaxMailboxBytes = 1 << 20
	}
	if b.MaxActiveTasks == 0 {
		b.MaxActiveTasks = 4
	}
	return b
}

type Adapter struct{ Registration Registration }

var errAdapter = errors.New("agent: v2 requires the event host")

func (*Adapter) Process(context.Context, legacy.Input) (legacy.Output, error) {
	return legacy.Output{}, errAdapter
}
func (*Adapter) Save(context.Context) (legacy.State, error)        { return legacy.State{}, errAdapter }
func (*Adapter) Restore(context.Context, legacy.State) error       { return errAdapter }
func (*Adapter) Resume(context.Context, legacy.ResumeNotice) error { return errAdapter }
