package mediaworker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/agent"
)

// AgentConfig bounds per-call application work. Zero values use the defaults.
type AgentConfig struct {
	Factory         agent.Factory
	QueueCapacity   int           // default 32 packets
	CallbackTimeout time.Duration // default 100 ms, including state encoding
	SaveInterval    time.Duration // default 100 ms between durable changed-state writes
	MaxStateBytes   int           // default 8 KiB
}

func (c AgentConfig) defaults() AgentConfig {
	if c.Factory == nil {
		c.Factory = func(string) agent.Agent { return &agent.Echo{} }
	}
	if c.QueueCapacity <= 0 {
		c.QueueCapacity = 32
	}
	if c.CallbackTimeout <= 0 {
		c.CallbackTimeout = 100 * time.Millisecond
	}
	if c.SaveInterval <= 0 {
		c.SaveInterval = 100 * time.Millisecond
	}
	if c.MaxStateBytes <= 0 {
		c.MaxStateBytes = agent.DefaultMaxStateBytes
	}
	return c
}

// agentState is copied with media counters inside the encrypted session blob.
// Revision is local ordering evidence, not a schema version.
type agentState struct {
	State    agent.State
	Progress agent.Progress
	Revision uint64
	legacy   bool // only a decoded version-six snapshot with no agent extension
}

type agentInput struct {
	input  agent.Input
	header rtp.Header
}
type agentFlush struct{ done chan error }
type agentHost struct {
	instance agent.Agent
	queue    chan agentInput
	flush    chan agentFlush
	overdue  atomic.Bool
	failed   atomic.Bool
	paused   bool       // guarded by the session's mu
	latest   agentState // actor-owned; publication takes session.mu
}

type agentCounters struct {
	drops     atomic.Uint64
	deadlines atomic.Uint64
	oversize  atomic.Uint64
	saves     atomic.Uint64
	versions  atomic.Uint64
	restores  atomic.Uint64
	callbacks atomic.Uint64
}

// AgentStats are lifetime worker counters, with no session labels.
type AgentStats struct {
	InputDrops        uint64
	CallbackDeadlines uint64
	OversizedStates   uint64
	SaveFailures      uint64
	UnknownVersions   uint64
	RestoreFailures   uint64
	CallbackFailures  uint64
}

func (w *Worker) AgentStats() AgentStats {
	c := &w.agents
	return AgentStats{c.drops.Load(), c.deadlines.Load(), c.oversize.Load(), c.saves.Load(), c.versions.Load(), c.restores.Load(), c.callbacks.Load()}
}

func cloneAgentState(state agentState) agentState {
	state.State.Bytes = slices.Clone(state.State.Bytes)
	return state
}

func (s *session) initAgent(restoring bool, opts ResumeOptions) error {
	cfg := s.worker.cfg.Agent
	h := &agentHost{instance: cfg.Factory(s.id), queue: make(chan agentInput, cfg.QueueCapacity), flush: make(chan agentFlush, 1)}
	s.agent = h
	if h.instance == nil {
		return s.agentError(errors.New("agent: nil factory result"))
	}
	if restoring {
		if s.state.Agent.legacy {
			if _, ok := h.instance.(*agent.Echo); !ok {
				return s.agentError(agent.ErrVersion)
			}
			s.state.Agent.State.Version = 1
			s.state.Agent.legacy = false
		}
		if err := s.validateAgentState(s.state.Agent.State); err != nil {
			return s.agentError(err)
		}
		if err := s.agentCall(func(ctx context.Context) error { return restoreAgent(ctx, h.instance, s.state.Agent.State) }); err != nil {
			return s.agentError(fmt.Errorf("%w: %w", agent.ErrRestore, err))
		}
		kind := opts.Kind
		if kind == "" {
			kind = agent.PlannedMove
			if opts.SequenceMargin > 0 {
				kind = agent.Takeover
			}
		}
		notice := agent.ResumeNotice{Kind: kind, CheckpointAge: opts.CheckpointAge, SnapshotAge: opts.SnapshotAge,
			InputMayBeDuplicated: opts.InputMayBeDuplicated, DuplicateWindows: slices.Clone(opts.DuplicateWindows), Progress: s.state.Agent.Progress}
		if err := s.agentCall(func(ctx context.Context) error { return h.instance.Resume(ctx, notice) }); err != nil {
			return s.agentError(err)
		}
	}
	// Save also captures changes made by Resume before the first durable write.
	var state agent.State
	if err := s.agentCall(func(ctx context.Context) error {
		var err error
		state, err = h.instance.Save(ctx)
		if err != nil {
			return fmt.Errorf("%w: %w", agent.ErrSave, err)
		}
		if err := s.validateAgentState(state); err != nil {
			return err
		}
		state.Bytes = slices.Clone(state.Bytes)
		return nil
	}); err != nil {
		return s.agentError(fmt.Errorf("%w: %w", agent.ErrSave, err))
	}
	s.state.Agent.State = state
	s.state.Agent.Revision++
	h.latest = cloneAgentState(s.state.Agent)
	return nil
}

func restoreAgent(ctx context.Context, a agent.Agent, state agent.State) error {
	if err := a.Restore(ctx, agent.State{Version: state.Version, Bytes: slices.Clone(state.Bytes)}); err != nil {
		return fmt.Errorf("%w: %w", agent.ErrRestore, err)
	}
	return nil
}

func (s *session) validateAgentState(state agent.State) error {
	if state.Version == 0 {
		return agent.ErrVersion
	}
	if len(state.Bytes) > s.worker.cfg.Agent.MaxStateBytes {
		return agent.ErrStateTooLarge
	}
	return nil
}

// agentCall never holds the media lock. One invocation is in flight at a time.
// If a callback ignores cancellation, quarantine that instance until it returns;
// the caller can close meanwhile. Do not create more callback goroutines.
func (s *session) agentCall(fn func(context.Context) error, waitLate ...bool) error {
	ctx, cancel := context.WithTimeout(s.ctx, s.worker.cfg.Agent.CallbackTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	select {
	case err := <-done:
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				s.worker.agents.deadlines.Add(1)
			}
			return ctx.Err()
		}
		return err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			s.worker.agents.deadlines.Add(1)
		}
		if len(waitLate) == 0 || !waitLate[0] {
			return ctx.Err()
		}
		s.agent.overdue.Store(true)
		select {
		case <-done:
			s.agent.overdue.Store(false)
			return ctx.Err()
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
}

func (s *session) agentError(err error) error {
	switch {
	case errors.Is(err, agent.ErrStateTooLarge):
		s.worker.agents.oversize.Add(1)
	case errors.Is(err, agent.ErrVersion):
		s.worker.agents.versions.Add(1)
	case errors.Is(err, agent.ErrSave):
		s.worker.agents.saves.Add(1)
	case errors.Is(err, agent.ErrRestore):
		s.worker.agents.restores.Add(1)
	default:
		s.worker.agents.callbacks.Add(1)
	}
	return fmt.Errorf("mediaworker: agent: %w", err)
}

func (s *session) enqueueAudio(in *rtp.Packet, header rtp.Header) {
	h := s.agent
	if h == nil || h.paused || h.overdue.Load() || s.ctx.Err() != nil {
		s.worker.agents.drops.Add(1)
		return
	}
	input := agent.Input{Payload: slices.Clone(in.Payload), Marker: in.Marker,
		Progress: agent.Progress{SSRC: in.SSRC, Index: extendIndex(s.state.SRTP.Inbound[in.SSRC], in.SequenceNumber), Timestamp: in.Timestamp}}
	select {
	case h.queue <- agentInput{input, header}:
	default:
		s.worker.agents.drops.Add(1)
	}
}

func (s *session) publishAgent() {
	s.mu.Lock()
	if !s.fenced.Load() && s.ctx.Err() == nil {
		s.state.Agent = cloneAgentState(s.agent.latest)
		s.wantSnapshot()
	}
	s.mu.Unlock()
}

func (s *session) processAudio(in agentInput) error {
	h := s.agent
	before := cloneAgentState(h.latest)
	in.input.Progress.Consumed = before.Progress.Consumed + 1
	var output agent.Output
	var saved agent.State
	var saving atomic.Bool
	err := s.agentCall(func(ctx context.Context) error {
		var err error
		output, err = h.instance.Process(ctx, in.input)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(output.Audio) > receiveMTU-12 {
			return errors.New("agent: oversized audio output")
		}
		output.Audio = slices.Clone(output.Audio)
		if output.Changed {
			saving.Store(true)
			saved, err = h.instance.Save(ctx)
			if err != nil {
				return fmt.Errorf("%w: %w", agent.ErrSave, err)
			}
			if err := s.validateAgentState(saved); err != nil {
				return err
			}
			saved.Bytes = slices.Clone(saved.Bytes)
			return nil
		}
		return nil
	}, true)
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	if errors.Is(err, agent.ErrSave) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) && saving.Load() {
		return fmt.Errorf("%w: %w", agent.ErrSave, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		s.worker.agents.drops.Add(1)
		// A late callback can have changed state. Restore the last accepted local
		// pair before processing another input; never commit a late result.
		if restoreErr := s.agentCall(func(ctx context.Context) error { return restoreAgent(ctx, h.instance, before.State) }); restoreErr != nil {
			return fmt.Errorf("%w: %w", agent.ErrRestore, restoreErr)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if output.Changed {
		h.latest.State = saved
		h.latest.Revision++
	}
	h.latest.Progress = in.input.Progress
	h.latest.Progress.Consumed = before.Progress.Consumed + 1
	// A stateless agent still moves its consumed-input progress with the call.
	if !output.Changed {
		h.latest.Revision++
	}
	s.sendAgentAudio(in.header, output.Audio, h.latest)
	return nil
}

func (s *session) agentLoop() {
	if s.agent == nil {
		return
	}
	if _, echo := s.agent.instance.(*agent.Echo); echo {
		for {
			select {
			case <-s.ctx.Done():
				return
			case request := <-s.agent.flush:
				request.done <- nil
			}
		}
	}
	ticker := time.NewTicker(s.worker.cfg.Agent.SaveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case request := <-s.agent.flush:
			// Enqueue was stopped under mu before this request, so the queue is finite.
			var err error
			for len(s.agent.queue) > 0 && err == nil {
				err = s.processAudio(<-s.agent.queue)
			}
			if err == nil {
				s.publishAgent()
			}
			request.done <- err
			if err != nil {
				s.agentFailed(err)
				return
			}
		case <-ticker.C:
			s.wantSnapshot()
		case in := <-s.agent.queue:
			if err := s.processAudio(in); err != nil {
				s.agentFailed(err)
				return
			}
		}
	}
}
func (s *session) agentFailed(err error) {
	if s.ctx.Err() != nil || s.agent == nil || !s.agent.failed.CompareAndSwap(false, true) {
		return
	}
	s.log.Warnf("session %s: %v", s.id, s.agentError(err))
	s.worker.mu.Lock()
	registered := s.worker.sessions[s.id] == s
	s.worker.mu.Unlock()
	s.close()
	if !registered {
		s.worker.release(s)
	}
}

func (s *session) flushAgent() error {
	if s.agent == nil {
		return nil
	}
	s.mu.Lock()
	s.agent.paused = true
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, time.Duration(cap(s.agent.queue)+2)*s.worker.cfg.Agent.CallbackTimeout)
	defer cancel()
	request := agentFlush{done: make(chan error, 1)}
	select {
	case s.agent.flush <- request:
	case <-ctx.Done():
		s.agentFailed(ctx.Err())
		return ctx.Err()
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		s.agentFailed(ctx.Err())
		return ctx.Err()
	}
}

// sendAgentAudio allocates a continuous application output sequence, using
// the same persisted encryption counters and resume margins as echo. Silent
// inputs cannot spend an unsent stream's wrap runway. Input processing and
// state encoding never hold this lock.
func (s *session) sendAgentAudio(header rtp.Header, payload []byte, state agentState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fenced.Load() || s.ctx.Err() != nil || s.srtpOut == nil {
		return
	}
	changed := !agentStateEqual(s.state.Agent, state)
	s.state.Agent = cloneAgentState(state)
	if changed {
		s.wantSnapshot()
	}
	if payload == nil {
		return
	}
	track := &s.state.Audio
	header.SequenceNumber = track.InitialSeq
	if track.Packets > 0 {
		header.SequenceNumber = uint16(track.HighestSentIndex + 1)
	}
	header.Padding = false
	header.PaddingSize = 0
	out := rtp.Packet{Header: header, Payload: payload}
	n, err := out.MarshalTo(s.plainBuf)
	if err != nil {
		return
	}
	encrypted, err := s.srtpOut.EncryptRTP(s.encryptBuf, s.plainBuf[:n], nil)
	if err != nil {
		return
	}
	s.encryptBuf = encrypted[:cap(encrypted)]
	oldOutbound := track.HighestSentIndex
	track.noteSent(&header)
	if track.Packets == 1 {
		if roc, ok := s.srtpOut.ROC(track.SSRC); ok {
			track.HighestSentIndex = uint64(roc)<<16 | uint64(header.SequenceNumber)
		}
	}
	if track.Packets == 1 || oldOutbound>>16 != track.HighestSentIndex>>16 {
		s.wantSnapshot()
	}
	if _, err := s.worker.send(encrypted, s.state.ICE.RemoteAddr); err != nil {
		s.log.Debugf("session %s: send agent audio: %v", s.id, err)
	}
	workerprobe.AfterEcho(s.worker.localAddr, s.ctx, s.id, s.plainBuf[:n])
}

// SessionAgent returns the saved state/progress pair, never the mutable agent.
func (w *Worker) SessionAgent(id string) (agent.State, agent.Progress, error) {
	s := w.session(id)
	if s == nil {
		return agent.State{}, agent.Progress{}, ErrUnknownSession
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state := cloneAgentState(s.state.Agent)
	return state.State, state.Progress, nil
}

func agentStateEqual(a, b agentState) bool {
	return a.State.Version == b.State.Version && bytes.Equal(a.State.Bytes, b.State.Bytes)
}
