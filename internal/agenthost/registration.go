// Package agenthost is the internal v2 registration seam. It deliberately does
// not expose a workerapp registration API before tasks, pacing and limits land.
package agenthost

import (
	"context"
	"errors"
	"slices"
	"time"

	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// Bounds are resolved internally until operator admission lands in slice 6.
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
	Bounds    Bounds
}

// Factory carries a registration through the existing legacy configuration
// field. Only the v2 host unwraps it; it cannot accidentally behave like Echo.
func Factory(r Registration) legacy.Factory {
	r.TaskKinds = slices.Clone(r.TaskKinds)
	slices.Sort(r.TaskKinds)
	r.TaskKinds = slices.Compact(r.TaskKinds)
	if r.Bounds.MaxStateBytes == 0 {
		r.Bounds.MaxStateBytes = 1 << 10
	}
	if r.Bounds.MaxPendingBytes == 0 {
		r.Bounds.MaxPendingBytes = 2 << 10
	}
	if r.Bounds.MaxMailboxBytes == 0 {
		r.Bounds.MaxMailboxBytes = 1 << 20
	}
	if r.Bounds.MaxActiveTasks == 0 {
		r.Bounds.MaxActiveTasks = 4
	}
	return func(string) legacy.Agent { return &Adapter{Registration: r} }
}

type Adapter struct{ Registration Registration }

var errAdapter = errors.New("agent: v2 requires the event host")

func (*Adapter) Process(context.Context, legacy.Input) (legacy.Output, error) {
	return legacy.Output{}, errAdapter
}
func (*Adapter) Save(context.Context) (legacy.State, error)        { return legacy.State{}, errAdapter }
func (*Adapter) Restore(context.Context, legacy.State) error       { return errAdapter }
func (*Adapter) Resume(context.Context, legacy.ResumeNotice) error { return errAdapter }
