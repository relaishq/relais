// Package agentbridge implements the isolated loopback runner and bot bridge.
package agentbridge

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/relaishq/relais/internal/agenthost"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
)

const stateKind = "bridge.state/v1"

// Bridge serves a worker-owned loopback registry and session endpoint.
// SetWorker is called before serving HTTP or calls.
type Bridge struct {
	config  Config
	worker  *mediaworker.Worker
	agents  sync.Map
	mu      sync.Mutex
	runner  *runner
	changed chan struct{}
	pending map[string]*assignment
}

func New(config Config) (*Bridge, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Bridge{config: config, changed: make(chan struct{}), pending: make(map[string]*assignment)}, nil
}
func (b *Bridge) SetWorker(w *mediaworker.Worker) { b.worker = w }
func (b *Bridge) Registration() agenthost.Registration {
	return agenthost.Registration{Name: "bridge", Bounds: agenthost.Bounds{MaxStateBytes: 3072, MaxHistoryBytes: 8192, MaxPendingBytes: 4096},
		Factory: func(args agent.FactoryArgs) agent.Agent {
			a := &adapter{bridge: b, id: args.SessionID, maxWorking: args.MaxStateBytes, maxHistory: args.MaxHistoryBytes, wake: make(chan struct{}, 1), deliveredWake: make(chan struct{}, 1), acks: make(chan agenthost.BridgeCheckpoint, 1)}
			a.current = &outboxImage{SessionID: a.id}
			data, _ := json.Marshal(a.current)
			a.current.encoded = data
			a.accepted = a.current
			b.agents.Store(args.SessionID, a)
			return a
		}, Tasks: map[string]agent.TaskHandler{stateKind: b.run}, SkipDrainWait: []string{stateKind}}
}

type checkpoint struct {
	Type          string `json:"type,omitempty"`
	Sequence      uint64 `json:"seq"`
	Version       uint32 `json:"version"`
	InputSSRC     uint32 `json:"input_ssrc"`
	InputSequence uint64 `json:"input_seq"`
	RunnerID      string `json:"runner_id,omitempty"`
	Working       []byte `json:"working,omitempty"`
	History       []byte `json:"history,omitempty"`
}
type adapter struct {
	bridge                 *Bridge
	id                     string
	maxWorking, maxHistory int
	state                  checkpoint // actor-owned, including during rollback
	wake                   chan struct{}
	deliveredWake          chan struct{}
	delivered              atomic.Uint64
	outboxMu               sync.Mutex
	current, accepted      *outboxImage
	restoreLength          int
	acks                   chan agenthost.BridgeCheckpoint
	ackMu                  sync.Mutex
	ackHigh                uint64
	runnerID               atomic.Value
	attachmentState        atomic.Pointer[checkpoint]
}

func (a *adapter) Handle(_ context.Context, e agent.Event) (agent.Effects, error) {
	switch e.Kind {
	case agent.Started:
		return agent.Effects{StartTasks: []agent.TaskSpec{{Step: "connection", Kind: stateKind, Class: agent.ReadOnly, Resume: agent.Abandon, Request: []byte(a.id)}}}, nil
	case agent.AudioInput, agent.Control, agent.PlaybackFinished:
		return agent.Effects{}, a.stageOutbox(e, 0)
	case agent.TimerFired:
		if e.Timer.ID == "bridge-connect" {
			return agent.Effects{StartTasks: []agent.TaskSpec{{Step: "connection", Kind: stateKind, Class: agent.ReadOnly, Resume: agent.Abandon, Request: []byte(a.id)}}}, nil
		}
	case agent.Resumed:
		if e.Resume.MoveOnly == agent.OutcomeUnknown {
			a.current = &outboxImage{SessionID: a.id, Revision: a.current.Revision + 1}
			data, _ := json.Marshal(a.current)
			a.current.encoded = data
		}
		return agent.Effects{SetTimers: []agent.TimerSpec{{ID: "bridge-connect"}}}, nil
	case agent.TaskMessage:
		if len(e.Result.Chunk.Data) > 0 {
			var update adapterUpdate
			if err := json.Unmarshal(e.Result.Chunk.Data, &update); err != nil {
				return agent.Effects{}, err
			}
			if update.RunnerID != "" {
				a.state.RunnerID = update.RunnerID
			}
			if update.Checkpoint != nil && update.Checkpoint.Sequence > a.state.Sequence {
				update.Checkpoint.RunnerID = a.state.RunnerID
				a.state = *update.Checkpoint
			}
			if update.Delivered > 0 {
				return agent.Effects{}, a.stageOutbox(agent.Event{}, update.Delivered)
			}
		}
	}
	return agent.Effects{}, nil
}
func (a *adapter) Save(context.Context) (agent.State, error) {
	c := a.state
	c.History = nil
	b, err := json.Marshal(adapterState{checkpoint: c, Outbox: a.current.Revision})
	return agent.State{Version: 1, Bytes: b, History: slices.Clone(a.state.History), HasMoveOnly: true, MoveOnlyLength: len(a.current.encoded)}, err
}
func (a *adapter) Restore(_ context.Context, s agent.State) error {
	if s.Version != 1 {
		return agent.ErrVersion
	}
	var saved adapterState
	if err := json.Unmarshal(s.Bytes, &saved); err != nil {
		return err
	}
	saved.History = slices.Clone(s.History)
	a.state = saved.checkpoint
	a.restoreLength = s.MoveOnlyLength
	a.outboxMu.Lock()
	if a.accepted.Revision == saved.Outbox {
		a.current = a.accepted
	} else {
		a.current = &outboxImage{SessionID: a.id, Revision: saved.Outbox}
	}
	a.outboxMu.Unlock()
	return nil
}
func (a *adapter) Accepted() {
	a.runnerID.Store(a.state.RunnerID)
	saved := a.state
	saved.Working = slices.Clone(saved.Working)
	saved.History = slices.Clone(saved.History)
	a.attachmentState.Store(&saved)
	a.outboxMu.Lock()
	a.accepted = a.current
	a.outboxMu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
}
func (a *adapter) Checkpointed(c agenthost.BridgeCheckpoint) {
	// Notification follows the store lock release. A newer writer may notify
	// first; never replace its exact capture with an older acknowledgement.
	a.ackMu.Lock()
	defer a.ackMu.Unlock()
	if c.Sequence <= a.ackHigh {
		return
	}
	a.ackHigh = c.Sequence
	select {
	case <-a.acks:
	default:
	}
	select {
	case a.acks <- c:
	default:
	}
}
func (a *adapter) Closed() { a.bridge.agents.CompareAndDelete(a.id, a) }
func (a *adapter) candidate(raw []byte) error {
	var c checkpoint
	if err := json.Unmarshal(raw, &c); err != nil {
		return err
	}
	if c.Version != 1 || c.Sequence == 0 {
		return agent.ErrVersion
	}
	history := c.History
	c.History = nil
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if len(b)+128 > a.maxWorking || len(history) > a.maxHistory {
		return agent.ErrCapacity
	}
	return nil
}

var errProtocol = errors.New("bridge: unsupported development protocol")
