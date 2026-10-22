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
	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/workerprobe"
	"github.com/relaishq/relais/pkg/agent"
	agentv2 "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/relay"
)

// AgentConfig bounds per-call application work. Zero values use the defaults.
type AgentConfig struct {
	// Factory preserves the legacy single-factory seam. V2 hosts also supply
	// Registrations for startup cadence validation; an opaque factory-only v2
	// adapter is validated when resolved, without probing legacy factories.
	Factory agent.Factory
	// Registrations resolves v2 restores by their persisted name, independently
	// of the factory selected for new calls. Nil preserves the single-factory seam.
	Registrations   map[string]agent.Factory
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
	History  []byte      `json:",omitempty"`
	V2       *v2Envelope `json:",omitempty"`
	State    agent.State
	Progress agent.Progress
	Revision uint64
	legacy   bool // only a decoded version-six snapshot with no agent extension
}

type agentInput struct {
	input  agent.Input
	header rtp.Header
}
type agentMoveExport struct {
	payload []byte
	tasks   []taskMove
	audio   []agentv2.Audio
}

type agentFlush struct {
	done     chan error
	deadline time.Time
	moveOnly chan agentMoveExport
}
type agentHost struct {
	instance agent.Agent
	v2       *v2Host
	queue    chan agentInput
	flush    chan agentFlush
	late     chan struct{}
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
	state.History = slices.Clone(state.History)
	if state.V2 != nil {
		e := *state.V2
		e.TaskKinds = slices.Clone(e.TaskKinds)
		e.Tasks = cloneTaskIntents(e.Tasks)
		e.Capacity = slices.Clone(e.Capacity)
		e.Timers = slices.Clone(e.Timers)
		e.Controls = slices.Clone(e.Controls)
		e.Playback.Window = slices.Clone(e.Playback.Window)
		e.Playback.Notify = slices.Clone(e.Playback.Notify)
		state.V2 = &e
	}
	return state
}

func (s *session) initAgent(restoring bool, opts ResumeOptions, move ...*movePayload) error {
	cfg := s.worker.cfg.Agent
	factory := cfg.Factory
	if restoring && s.state.Agent.V2 != nil && cfg.Registrations != nil {
		var ok bool
		factory, ok = cfg.Registrations[s.state.Agent.V2.Name]
		if !ok {
			return s.agentError(fmt.Errorf("%w: %w", agent.ErrRestore, agentv2.ErrIncompatibleAgent))
		}
	}
	h := &agentHost{instance: factory(s.id), queue: make(chan agentInput, cfg.QueueCapacity), flush: make(chan agentFlush, 1), late: make(chan struct{}, 1)}
	s.agent = h
	if adapter, ok := h.instance.(*agenthost.Adapter); ok {
		var payload *movePayload
		if len(move) > 0 {
			payload = move[0]
		}
		return s.initV2Agent(adapter, restoring, opts, payload)
	}
	if restoring && s.state.Agent.V2 != nil {
		return s.agentError(fmt.Errorf("%w: %w", agent.ErrRestore, agentv2.ErrIncompatibleAgent))
	}
	if h.instance == nil {
		return s.agentError(errors.New("agent: nil factory result"))
	}
	if restoring {
		if s.state.Agent.legacy {
			if _, ok := h.instance.(*agent.Echo); !ok {
				return s.agentError(agent.ErrVersion)
			}
			s.state.Agent.State, _ = h.instance.Save(s.ctx)
			s.state.Agent.legacy = false
		}
		if err := s.validateAgentState(s.state.Agent.State); err != nil {
			return s.agentError(err)
		}
		_ = s.checkpoint.restore(checkpointRestore{pair: &s.state.Agent})
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
			return s.agentError(fmt.Errorf("%w: %w", agent.ErrRestore, err))
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
	_, err := s.checkpoint.admit(checkpointAdmission{legacy: &state})
	return err
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
		case s.agent.late <- struct{}{}:
		default:
		}
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
	// Worker memory pressure is a resource refusal, not an agent callback fault.
	if errors.Is(err, ErrMoveBudget) {
		return err
	}
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
		if s.agent.v2 != nil {
			s.state.Agent.V2.Playback = s.playbackSnapshotLocked()
		}
	}
	s.mu.Unlock()
}

func (s *session) processAudio(in agentInput) error {
	h := s.agent
	if h.v2 != nil {
		return s.processV2Audio(in)
	}
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
	if s.agent.v2 != nil {
		s.v2AgentLoop()
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
	for {
		// A move request takes priority over starting another queued callback.
		select {
		case request := <-s.agent.flush:
			if !s.finishAgentFlush(request) {
				return
			}
			continue
		default:
		}
		select {
		case <-s.ctx.Done():
			return
		case request := <-s.agent.flush:
			if !s.finishAgentFlush(request) {
				return
			}
		case in := <-s.agent.queue:
			if err := s.processAudio(in); err != nil {
				s.agentFailed(err)
				return
			}
		}
	}
}

// Reserve a full callback deadline before starting more work. When the move
// budget cannot fit it, discard the remaining queue and publish only the pair
// already consumed. Enqueue is paused; the caller may also discard this queue.
func (s *session) finishAgentFlush(request agentFlush) bool {
	if request.moveOnly != nil {
		if err := s.validateTaskMoves(s.agent.latest.V2.Tasks, &movePayload{Tasks: s.taskMoveViews()}); err != nil {
			s.log.Warnf("session %s: export preflight incompatible: %v", s.id, err)
			request.done <- errors.Join(ErrExportIncompatible, err)
			return true
		}
		// A definitive refusal must precede both queue drain and paused input
		// drops. The actor serializes this admission with Handle and Save.
		s.mu.Lock()
		if err := s.reserveV2ExportScratch(); err != nil {
			s.mu.Unlock()
			request.done <- err
			return true
		}
		s.agent.paused = true
		s.mu.Unlock()
		s.stopTaskAttempts()
	}
	var err error
	reserve := s.worker.cfg.Agent.CallbackTimeout
	if request.moveOnly != nil {
		reserve *= 2
	}
drain:
	for time.Until(request.deadline) >= reserve {
		select {
		case in := <-s.agent.queue:
			err = s.processAudio(in)
			if errors.Is(err, errTaskAdmissionBackpressure) {
				// Export the last accepted pair; the relay replays unconsumed input on
				// the target, just as when the flush deadline leaves queued input.
				err = nil
				break drain
			}
			if err != nil {
				break drain
			}
		default:
			break drain
		}
	}
	s.dropAgentQueue()
	if err == nil {
		s.publishAgent()
	}
	if err == nil && request.moveOnly != nil {
		ctx, cancel := context.WithDeadline(s.ctx, request.deadline)
		var payload agentMoveExport
		payload, err = s.saveV2MoveOnly(ctx)
		cancel()
		if err == nil {
			request.moveOnly <- payload
		}
	}
	// Only the pre-pause admission may promise no dropped input. A callback
	// cannot make a later failure definitive by returning that sentinel.
	if errors.Is(err, ErrExportNotFlushed) {
		err = fmt.Errorf("mediaworker: export failed after pause: %v", err)
	}
	if errors.Is(err, agentv2.ErrCapacity) || errors.Is(err, errTaskAdmissionBackpressure) {
		err = privateapi.Uncertain(err)
	}
	request.done <- err
	if err != nil {
		s.agentFailed(err)
		return false
	}
	return true
}

func (s *session) dropAgentQueue() {
	if s.agent.v2 != nil && s.agent.v2.pendingInput != nil {
		s.agent.v2.pendingInput = nil
		s.worker.agents.drops.Add(1)
	}
	for {
		select {
		case <-s.agent.queue:
			s.worker.agents.drops.Add(1)
		default:
			return
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

func (s *session) flushAgent(moveOnly ...chan agentMoveExport) error {
	if s.agent == nil {
		return nil
	}
	if _, echo := s.agent.instance.(*agent.Echo); echo {
		return nil // Echo has no pending callbacks; fence atomically under mu.
	}
	if len(moveOnly) == 0 {
		s.mu.Lock()
		s.agent.paused = true
		s.mu.Unlock()
	}
	budget := min(time.Duration(cap(s.agent.queue)+2)*s.worker.cfg.Agent.CallbackTimeout, relay.DefaultHoldTimeout/2)
	request := agentFlush{done: make(chan error, 1), deadline: time.Now().Add(budget)}
	if len(moveOnly) > 0 {
		request.moveOnly = moveOnly[0]
	}
	// A callback already quarantined before the move cannot produce a safe
	// export. Queue exhaustion alone never makes a healthy agent terminal.
	if s.agent.overdue.Load() {
		s.agentFailed(context.DeadlineExceeded)
		return context.DeadlineExceeded
	}
	select {
	case s.agent.flush <- request:
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
	timer := time.NewTimer(time.Until(request.deadline))
	defer timer.Stop()
	expired := timer.C
	for {
		select {
		case err := <-request.done:
			return err
		case <-expired:
			// V2 admission belongs to the actor: the waiting caller must not
			// discard input before its scratch reservation succeeds.
			if request.moveOnly == nil {
				s.dropAgentQueue()
			}
			expired = nil
			// The one in-flight callback may still meet its own deadline.
			// Wait for its paired publication, or actual quarantine below.
		case <-s.agent.late:
			if s.agent.overdue.Load() {
				s.agentFailed(context.DeadlineExceeded)
				return context.DeadlineExceeded
			}
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
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
	_ = s.sendAgentAudioLocked(header, payload, state)
}

func (s *session) sendAgentAudioLocked(header rtp.Header, payload []byte, state agentState) error {
	changed := !agentStateEqual(s.state.Agent, state)
	s.state.Agent = cloneAgentState(state)
	if changed {
		s.checkpoint.request()
	}
	if payload == nil {
		return nil
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
		return err
	}
	encrypted, err := s.srtpOut.EncryptRTP(s.encryptBuf, s.plainBuf[:n], nil)
	if err != nil {
		return err
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
		s.checkpoint.request()
	}
	if _, err := s.worker.send(encrypted, s.state.ICE.RemoteAddr); err != nil {
		s.log.Debugf("session %s: send agent audio: %v", s.id, err)
	} else {
		s.tracePacket(encrypted, "agent")
	}
	workerprobe.AfterEcho(s.worker.localAddr, s.ctx, s.id, s.plainBuf[:n])
	return nil
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
	return a.State.Version == b.State.Version && bytes.Equal(a.State.Bytes, b.State.Bytes) && bytes.Equal(a.History, b.History) && v2EnvelopeEqual(a.V2, b.V2)
}
