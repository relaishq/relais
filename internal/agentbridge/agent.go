// Package agentbridge implements the isolated loopback runner and bot bridge.
package agentbridge

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
)

const stateKind = "bridge.state/v1"

// The ADR handoff deadline ends in an explicit no-capacity/silence outcome.
const attachBudget = 2 * time.Second

// Runner identity, bot outcome and outbox revision fit even at their maxima.
const adapterMetadataBytes = 512

// Bridge serves a worker-owned loopback registry and session endpoint.
// SetWorker is called before serving HTTP or calls.
type Bridge struct {
	config      Config
	worker      *mediaworker.Worker
	agents      sync.Map
	mu          sync.Mutex
	runner      *runner
	changed     chan struct{}
	pending     map[string]*assignment
	revocations map[string]*revocation
	successors  map[string]replacement
	active      map[string]activeBot
}

func New(config Config) (*Bridge, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Bridge{config: config, changed: make(chan struct{}), pending: make(map[string]*assignment), revocations: make(map[string]*revocation), successors: make(map[string]replacement), active: make(map[string]activeBot)}, nil
}
func (b *Bridge) SetWorker(w *mediaworker.Worker) { b.worker = w }
func (b *Bridge) Registration() agenthost.Registration {
	return agenthost.Registration{Name: "bridge", Bounds: agenthost.Bounds{MaxStateBytes: 3072, MaxHistoryBytes: 8192, MaxPendingBytes: 4096},
		Factory: func(args agent.FactoryArgs) agent.Agent {
			a := &adapter{bridge: b, id: args.SessionID, maxWorking: args.MaxStateBytes, maxHistory: args.MaxHistoryBytes, wake: make(chan struct{}, 1), deliveredWake: make(chan struct{}, 1), acks: make(chan agenthost.BridgeCheckpoint, 1), deferred: make(chan map[string]any, 1)}
			a.current = &outboxImage{SessionID: a.id}
			data, _ := json.Marshal(a.current)
			a.current.encoded = data
			a.accepted = a.current
			b.agents.Store(args.SessionID, a)
			return a
		}, Tasks: map[string]agent.TaskHandler{stateKind: b.run}, SkipDrainWait: []string{stateKind}}
}

type checkpoint struct {
	Scope         mediaworker.BridgeOutputScope `json:"scope,omitempty"`
	Resume        string                        `json:"resume,omitempty"`
	Type          string                        `json:"type,omitempty"`
	BotOutcome    string                        `json:"bot_outcome,omitempty"`
	Sequence      uint64                        `json:"seq"`
	Version       uint32                        `json:"version"`
	InputSSRC     uint32                        `json:"input_ssrc"`
	InputSequence uint64                        `json:"input_seq"`
	RunnerID      string                        `json:"runner_id,omitempty"`
	Working       []byte                        `json:"working,omitempty"`
	History       []byte                        `json:"history,omitempty"`
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
	deferred               chan map[string]any
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
			a.current = &outboxImage{SessionID: a.id, Scope: a.state.Scope, Revision: a.current.Revision + 1}
			data, _ := json.Marshal(a.current)
			a.current.encoded = data
		}
		return agent.Effects{SetTimers: []agent.TimerSpec{{ID: "bridge-connect"}}}, nil
	case agent.TaskMessage:
		var update adapterUpdate
		if len(e.Result.Chunk.Data) > 0 {
			if err := json.Unmarshal(e.Result.Chunk.Data, &update); err != nil {
				return agent.Effects{}, err
			}
			scope, ok := a.applicationScope(update.Scope)
			if !ok {
				return agent.Effects{}, nil
			}
			update.Scope = scope
			if update.Checkpoint != nil {
				update.Checkpoint.Scope = scope
			}
		}
		if e.Result.Final {
			// Failed attachment/owner socket loss never claims bot recovery.
			// State is exposed through the existing private agent-session status.
			a.state.BotOutcome = "no_capacity_silence"
		}
		if len(e.Result.Chunk.Data) > 0 {
			if update.Scope.BotInstance != "" && update.Scope != a.state.Scope {
				if err := a.bindOutboxScope(update.Scope); err != nil {
					return agent.Effects{}, err
				}
				a.state.Scope = update.Scope
			}
			if update.RunnerID != "" {
				a.state.RunnerID = update.RunnerID
				a.state.BotOutcome = "attaching"
			}
			if update.Attached {
				a.state.Resume = ""
				a.state.BotOutcome = "attached"
			}
			if update.Checkpoint != nil && update.Checkpoint.Sequence > a.state.Sequence {
				update.Checkpoint.BotOutcome = a.state.BotOutcome
				update.Checkpoint.RunnerID = a.state.RunnerID
				update.Checkpoint.Resume = a.state.Resume
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
		a.current = &outboxImage{SessionID: a.id, Scope: saved.Scope, Revision: saved.Outbox}
	}
	a.outboxMu.Unlock()
	return nil
}
func (a *adapter) Accepted() {
	if previous := a.attachmentState.Load(); previous != nil && previous.Scope != a.state.Scope {
		a.delivered.Store(0)
	}
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
func (a *adapter) Closed() {
	b := a.bridge
	if b.agents.CompareAndDelete(a.id, a) {
		b.mu.Lock()
		delete(b.revocations, a.id)
		delete(b.successors, a.id)
		delete(b.active, a.id)
		for token, p := range b.pending {
			if p.scope.SessionID == a.id {
				delete(b.pending, token)
			}
		}
		b.mu.Unlock()
	}
}
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
	if len(b)+adapterMetadataBytes > a.maxWorking || len(history) > a.maxHistory {
		return agent.ErrCapacity
	}
	return nil
}

var errProtocol = errors.New("bridge: unsupported development protocol")

// A queued callback can survive socket cancellation and exact export. Check its
// captured lineage at application time; an epoch remap keeps the same lineage.
func (a *adapter) applicationScope(scope mediaworker.BridgeOutputScope) (mediaworker.BridgeOutputScope, bool) {
	current := a.state.Scope
	if a.bridge.worker != nil {
		if bound, err := a.bridge.worker.BridgeOutputBinding(a.id); err == nil {
			current = bound
		}
	}
	if current.BotInstance == "" {
		return scope, a.state.Scope.BotInstance == ""
	}
	return current, scope.SessionID == current.SessionID && scope.BotInstance == current.BotInstance && scope.Generation == current.Generation && scope.LineageID == current.LineageID && scope.Epoch <= current.Epoch && scope.Epoch > 0
}

func (a *adapter) Restart(ctx context.Context, durable agent.State, scope agenthost.BridgeScope, runner string) (agent.State, error) {
	var saved adapterState
	if err := json.Unmarshal(durable.Bytes, &saved); err != nil {
		return agent.State{}, err
	}
	saved.History = slices.Clone(durable.History)
	changed := a.state.Scope != mediaworker.BridgeOutputScope(scope)
	saved.Scope = mediaworker.BridgeOutputScope(scope)
	saved.RunnerID, saved.BotOutcome, saved.Resume = runner, "attaching", "bot_restart"
	a.state = saved.checkpoint
	if changed {
		a.current = &outboxImage{SessionID: a.id, Scope: mediaworker.BridgeOutputScope(scope), Revision: a.current.Revision + 1}
		a.current.encoded, _ = json.Marshal(a.current)
	}
	return a.Save(ctx)
}

func (a *adapter) Deferred(result agent.TaskResult) {
	var update adapterUpdate
	if json.Unmarshal(result.Chunk.Data, &update) != nil || update.Checkpoint == nil {
		return
	}
	scope, ok := a.applicationScope(update.Scope)
	if !ok {
		return
	}
	message := map[string]any{"scope": scope, "type": "deferred", "seq": update.Checkpoint.Sequence, "reason": "history_budget", "age_ms": max(0, time.Since(update.Arrived).Milliseconds())}
	select {
	case <-a.deferred:
	default:
	}
	select {
	case a.deferred <- message:
	default:
	}
}
