package mediaworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
)

const (
	v2SessionStateVersion      = 8
	controlSessionStateVersion = 9
	v2Contract                 = "agent/v2"
	v2MoveSessionBytes         = 1 << 20
	v2MoveWorkerBytes          = 64 << 20
	v2MaxWireBytes             = 4 << 20
	v2HostMetadataBytes        = 2 << 10
	// Keep one export's worst-case copy/base64 scratch available at admission.
	v2ExportScratchBytes = 8 * v2MoveSessionBytes
)

// Versions 8 and 9 contain this subtree. The legacy state/progress encoding is unchanged.
type v2Envelope struct {
	Controls        []controlRecord `json:"ct,omitempty"`
	ControlFloor    uint64          `json:"cl,omitempty"`
	ControlHigh     uint64          `json:"ch,omitempty"`
	Quota           *snapshotQuota  `json:"sq,omitempty"`
	Name            string
	Contract        string
	TaskKinds       []string
	Bounds          agenthost.Bounds `json:",omitzero"` // main f6428f2 legacy v8 identity
	Limits          *agent.Limits    `json:"limits,omitempty"`
	HasMoveOnly     bool
	MoveOnlyLength  int
	Started         bool
	Tasks           []taskIntent  `json:",omitempty"`
	Capacity        []taskRefusal `json:",omitempty"`
	CancelFloor     agent.Turn    `json:"cf,omitempty"`
	TimerGeneration uint64        `json:"tg,omitempty"`
	Timers          []hostTimer   `json:"tm,omitempty"`
	Playback        playbackState `json:"pb,omitzero"`
}

type v2Host struct {
	instance       agent.Agent
	registration   agenthost.Registration
	ready          chan struct{}
	tasks          []*localTask   // actor-owned
	receiptRetries []receiptRetry // actor-owned; only receipt-blocked events wait
	pendingInput   *agentInput    // one backpressured, unconsumed input; actor-owned
	emissions      chan taskEmission
	playback       playbackRuntime // guarded by session.mu
	wakePlayback   chan struct{}
	controls       chan controlRequest
	force          chan struct{}
}

// movePayload exists only in exact exports, including an empty exact export.
// A checkpoint never includes this field or calls SaveMoveOnly.
type movePayload struct {
	ControlAck uint64 `json:",omitempty"`
	Bytes      []byte
	Tasks      []taskMove    `json:",omitempty"`
	Audio      []agent.Audio `json:",omitempty"`
}

func (s *session) initV2Agent(r *agenthost.Adapter, restoring bool, opts ResumeOptions, move *movePayload) error {
	reg := r.Registration
	// The legacy Factory field is opaque until it returns an adapter. Public
	// registrations are checked in New; factory-only v2 hosts are checked here.
	if err := s.snapshotLimits().ValidateForInterval(s.worker.cfg.SnapshotInterval); err != nil {
		return err
	}
	if opts.Kind == legacy.Takeover || opts.SequenceMargin > 0 {
		move = nil
	}
	legacyBounds := reg.Bounds
	reg.Bounds = effectiveAgentBounds(reg.Declared, s.snapshotLimits(), s.componentMode)
	if reg.Name == "" || reg.Name == "echo" || reg.Name == "demo" || reg.Factory == nil ||
		reg.Declared.MaxHistoryBytes < 0 || reg.Declared.MaxHistoryBytes > componentMaxHistoryBytes || reg.Bounds.MaxStateBytes <= 0 || reg.Bounds.MaxStateBytes > 64<<10 || reg.Bounds.MaxPendingBytes <= 0 ||
		reg.Bounds.MaxMailboxBytes <= 0 || reg.Bounds.MaxMailboxBytes > s.snapshotLimits().MoveBytesPerSession || reg.Bounds.MaxActiveTasks <= 0 ||
		reg.Bounds.MaxAudioBytes < 0 || reg.Bounds.MaxAudioDuration < 0 {
		return s.agentError(fmt.Errorf("%w: invalid internal registration", agent.ErrIncompatibleAgent))
	}
	if s.componentMode && reg.Bounds.MaxStateBytes+6+componentCipherOverhead > s.snapshotLimits().MaxBytes {
		return s.agentError(fmt.Errorf("%w: declared working component cannot fit operator hot cap", agent.ErrStateTooLarge))
	}
	for _, kind := range reg.TaskKinds {
		if kind == "" {
			return s.agentError(fmt.Errorf("%w: empty task kind", agent.ErrIncompatibleAgent))
		}
	}
	if restoring {
		e := s.state.Agent.V2
		if e == nil || e.Contract != v2Contract || e.Name != reg.Name || !slices.Equal(e.TaskKinds, reg.TaskKinds) || (e.Limits != nil && *e.Limits != reg.Declared) || (e.Limits == nil && e.Bounds != legacyBounds) {
			return s.agentError(fmt.Errorf("%w: %w", legacy.ErrRestore, agent.ErrIncompatibleAgent))
		}
		if err := s.validateRestoredAgentBounds(reg.Bounds, move != nil); err != nil {
			return s.v2RestoreError(err)
		}
	} else {
		s.state.Version = v2SessionStateVersion
		s.state.Agent.V2 = &v2Envelope{Name: reg.Name, Contract: v2Contract, TaskKinds: slices.Clone(reg.TaskKinds), Limits: &reg.Declared}
	}
	metadata, err := json.Marshal(v2Envelope{Name: reg.Name, Contract: v2Contract, TaskKinds: reg.TaskKinds, Limits: &reg.Declared})
	if err != nil {
		return s.agentError(err)
	}
	if len(metadata) > min(reg.Bounds.MaxPendingBytes, v2HostMetadataBytes) {
		return s.agentError(fmt.Errorf("%w: registration metadata exceeds internal host budget", agent.ErrIncompatibleAgent))
	}
	// Crash metadata has no payload to copy or reserve. Exact restores only
	// reserve the one move-payload copy they make, for its actual byte length.
	if restoring && move != nil && (len(move.Bytes) > 0 || len(move.Tasks) > 0 || len(move.Audio) > 0) {
		scratch := int64(len(move.Bytes) + audioRefsBytes(move.Audio))
		for _, t := range move.Tasks {
			scratch += int64(len(t.Request))
			for _, r := range t.Results {
				scratch += int64(taskResultBytes(r))
			}
			for _, r := range t.Audio {
				scratch += int64(taskRetainedAudioBytes(r))
			}
		}
		if scratch > int64(reg.Bounds.MaxMailboxBytes) {
			return s.v2RestoreError(agent.ErrCapacity)
		}
		if !s.worker.reserveMoveBytes(scratch) {
			return ErrMoveBudget
		}
		defer s.worker.moveBytes.Add(-scratch)
	}

	h := &v2Host{controls: make(chan controlRequest), registration: reg, ready: make(chan struct{}), emissions: make(chan taskEmission), force: make(chan struct{}, 1), wakePlayback: make(chan struct{}, 1)}
	s.agent.v2 = h
	if err := s.checkpoint.restore(checkpointRestore{profile: true}); err != nil {
		return err
	}

	if restoring && s.worker.cfg.Relay != nil {
		ctx, cancel := context.WithTimeout(s.ctx, ownershipTimeout)
		now, err := s.worker.cfg.Relay.Owners.Clock(ctx, s.id)
		cancel()
		if err != nil {
			return err
		}
		// Restoration is exclusive here, before callbacks and packet adoption.
		capturedAt := s.checkpoint.progress(checkpointProgressRequest{}).observations.CapturedAt
		if now.Before(capturedAt) {
			return sessionstore.ErrUnsafeCheckpointClock
		}
		s.storeClock, s.storeClockAt = now, time.Now()
	}
	if restoring {
		if err := s.checkpoint.restore(checkpointRestore{quota: s.state.Agent.V2.Quota}); err != nil {
			return s.v2RestoreError(err)
		}
	}
	// Factory follows the legacy factory rule: quick, nonblocking construction.
	h.instance = reg.Factory(agent.FactoryArgs{SessionID: s.id, AgentName: reg.Name, MaxStateBytes: reg.Bounds.MaxStateBytes, MaxHistoryBytes: reg.Bounds.MaxHistoryBytes})
	if h.instance == nil {
		return s.agentError(errors.New("agent: nil v2 factory result"))
	}
	if restoring {
		state := v2State(s.state.Agent)
		if err := s.validateV2StateMetadata(state, move == nil); err != nil {
			return s.v2RestoreError(err)
		}
		if move != nil {
			if err := s.reserveMoveBytes(int64(state.MoveOnlyLength)); err != nil {
				return err
			}
		}
		var ack *uint64
		if move != nil {
			ack = &move.ControlAck
		}
		if err := s.checkpoint.restore(checkpointRestore{pair: &s.state.Agent, controlAck: ack}); err != nil {
			return s.v2RestoreError(err)
		}
		if err := s.agentCall(func(ctx context.Context) error { return h.instance.Restore(ctx, cloneV2State(state)) }); err != nil {
			return s.v2RestoreError(err)
		}
		outcome := agent.OK
		if move == nil {
			for _, t := range s.state.Agent.V2.Tasks {
				if (t.Class == agent.ReadOnly && t.RequestLength > 0 && (!t.Final || t.Cursor != t.Published)) || t.Published > t.Cursor {
					outcome = agent.OutcomeUnknown
					break
				}
			}
		}
		if move == nil && s.state.Agent.V2.Playback.Pending > 0 {
			outcome = agent.OutcomeUnknown
		}
		if state.HasMoveOnly {
			if move != nil {
				if len(move.Bytes) != state.MoveOnlyLength {
					return s.agentError(fmt.Errorf("%w: move-only length mismatch", legacy.ErrRestore))
				}
				if err := s.agentCall(func(ctx context.Context) error {
					return h.instance.(agent.MoveOnlyState).RestoreMoveOnly(ctx, slices.Clone(move.Bytes))
				}); err != nil {
					return s.v2RestoreError(err)
				}
			} else {
				outcome = agent.OutcomeUnknown
			}
		} else if move != nil && len(move.Bytes) != 0 {
			return s.agentError(fmt.Errorf("%w: unexpected move-only payload", legacy.ErrRestore))
		}
		kind := opts.Kind
		if kind == "" {
			kind = legacy.PlannedMove
			if opts.SequenceMargin > 0 {
				kind = legacy.Takeover
			}
		}
		notice := &agent.ResumeNotice{ResumeNotice: legacy.ResumeNotice{Kind: kind, CheckpointAge: opts.CheckpointAge, SnapshotAge: opts.SnapshotAge,
			InputMayBeDuplicated: opts.InputMayBeDuplicated, DuplicateWindows: slices.Clone(opts.DuplicateWindows), Progress: s.state.Agent.Progress}, MoveOnly: outcome}
		s.agent.latest = cloneAgentState(s.state.Agent)
		if err := s.restoreTasks(move); err != nil {
			return s.v2RestoreError(err)
		}
		if err := s.restorePlayback(move); err != nil {
			return s.v2RestoreError(err)
		}
		s.publishTaskIntents()
		// Resumed is per adoption attempt. It is not a durable task cause.
		attempt, err := randomString(16)
		if err != nil {
			return err
		}
		if err := s.handleV2EventMode(agent.Event{ID: agent.EventID("resumed/" + s.id + "/" + attempt), Kind: agent.Resumed, Resume: notice}, true); err != nil {
			return s.v2RestoreError(err)
		}
		close(h.ready)
	} else {
		var saved agent.State
		if err := s.agentCall(func(ctx context.Context) error {
			var err error
			saved, err = s.measuredSave(ctx, h.instance)
			if err != nil {
				return fmt.Errorf("%w: %w", legacy.ErrSave, err)
			}
			if err = s.validateV2State(saved); err != nil {
				return err
			}
			saved = cloneV2State(saved)
			return nil
		}); err != nil {
			return s.agentError(err)
		}
		s.acceptV2State(saved)
		s.agent.latest = cloneAgentState(s.state.Agent)
	}
	return nil
}

func cloneV2State(state agent.State) agent.State {
	state.Bytes = slices.Clone(state.Bytes)
	state.History = slices.Clone(state.History)
	return state
}
func v2State(pair agentState) agent.State {
	return agent.State{Version: pair.State.Version, Bytes: pair.State.Bytes, History: pair.History, HasMoveOnly: pair.V2.HasMoveOnly, MoveOnlyLength: pair.V2.MoveOnlyLength}
}
func (s *session) v2RestoreError(err error) error {
	if errors.Is(err, ErrMoveBudget) {
		return err
	}
	return s.agentError(fmt.Errorf("%w: %w", legacy.ErrRestore, err))
}

func (s *session) validateV2State(state agent.State) error {
	if err := s.validateV2StateMetadata(state); err != nil {
		return err
	}
	if s.agent.latest.V2 != nil {
		return s.taskBudget(s.agent.v2.tasks, state.MoveOnlyLength)
	}
	return s.reserveMoveBytes(int64(state.MoveOnlyLength))
}
func (s *session) validateV2StateMetadata(state agent.State, lostMoveData ...bool) error {
	if state.Version == 0 {
		return agent.ErrVersion
	}
	if len(state.Bytes) > s.agent.v2.registration.Bounds.MaxStateBytes {
		return agent.ErrStateTooLarge
	}
	if len(state.History) > s.agent.v2.registration.Bounds.MaxHistoryBytes || (!s.componentMode && len(state.History) != 0) {
		return fmt.Errorf("%w: %w", agent.ErrCapacity, agent.ErrHistoryTooLarge)
	}
	if state.MoveOnlyLength < 0 || (!state.HasMoveOnly && state.MoveOnlyLength != 0) {
		return errors.New("agent: invalid move-only metadata")
	}
	if (len(lostMoveData) == 0 || !lostMoveData[0]) && (state.MoveOnlyLength > s.agent.v2.registration.Bounds.MaxMailboxBytes || state.MoveOnlyLength > s.snapshotLimits().MoveBytesPerSession) {
		return fmt.Errorf("%w: move-only data exceeds session budget", agent.ErrCapacity)
	}
	if state.HasMoveOnly {
		if _, ok := s.agent.v2.instance.(agent.MoveOnlyState); !ok {
			return errors.New("agent: HasMoveOnly requires MoveOnlyState")
		}
	}
	return nil
}

// Reservation tracks admitted move data without reading it during Save. Close
// and callback acceptance synchronize so a late callback cannot leak a reserve.
func (s *session) reserveMoveBytes(n int64) error {
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	if s.moveClosed {
		return context.Canceled
	}
	delta := n - s.moveReserved
	var scratchDelta int64
	if s.exportScratch != nil {
		scratchDelta = max(0, 8*n-s.exportScratch.bytes.Load())
	}
	if scratchDelta > 0 {
		if !s.worker.reserveMoveBytes(scratchDelta) {
			return ErrMoveBudget
		}
	}
	if delta > 0 && !s.worker.reserveMoveBytesWithin(delta, s.snapshotLimits().MoveBytesTotal-int64(8*s.snapshotLimits().MoveBytesPerSession)) {
		s.worker.moveBytes.Add(-scratchDelta)
		return ErrMoveBudget
	}
	if delta < 0 {
		s.worker.moveBytes.Add(delta)
	}
	if scratchDelta > 0 {
		s.exportScratch.bytes.Add(scratchDelta)
	}
	s.moveReserved = n
	return nil
}
func (w *Worker) reserveMoveBytes(n int64) bool {
	return w.reserveMoveBytesWithin(n, w.cfg.SnapshotLimits.MoveBytesTotal)
}
func (w *Worker) reserveMoveBytesWithin(n, limit int64) bool {
	for {
		old := w.moveBytes.Load()
		if n > limit-old {
			return false
		}
		if w.moveBytes.CompareAndSwap(old, old+n) {
			return true
		}
	}
}
func (s *session) releaseMoveBytes() {
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	s.moveClosed = true
	s.worker.moveBytes.Add(-s.moveReserved)
	s.moveReserved = 0
	if s.exportScratch != nil {
		s.exportScratch.release()
		s.exportScratch = nil
	}
}
func (s *session) acceptV2State(saved agent.State) {
	s.state.Agent.State = legacy.State{Version: saved.Version, Bytes: saved.Bytes}
	s.state.Agent.History = saved.History
	s.state.Agent.V2.HasMoveOnly = saved.HasMoveOnly
	s.state.Agent.V2.MoveOnlyLength = saved.MoveOnlyLength
	s.state.Agent.Revision++
}

func validateV2Effects(kind agent.EventKind, e agent.Effects) error {
	if kind == agent.Resumed && len(e.StartTasks) != 0 {
		return errors.New("agent: StartTasks from Resumed is forbidden")
	}

	return nil
}

func (s *session) handleV2Event(event agent.Event) error {
	return s.handleV2EventMode(event, false)
}

func (s *session) handleV2EventMode(event agent.Event, adoption bool) error {

	// Future kinds have no delivery route. Do not call
	// the agent or Save, so an unknown event cannot alter accepted state.
	switch event.Kind {
	case agent.Started, agent.AudioInput, agent.Resumed, agent.TaskMessage, agent.TimerFired, agent.PlaybackFinished, agent.Control:
	default:
		return nil
	}
	if event.Kind == agent.Control {
		if event.Control == nil {
			return agent.ErrControlInvalid
		}
		owned := *event.Control
		owned.Data = slices.Clone(owned.Data)
		event.Control = &owned
	}
	h := s.agent
	if !adoption && len(h.v2.receiptRetries) > 0 && s.receiptEventThrottled(event) {
		return errControlReceiptBackpressure
	}

	event.At = s.taskNow()
	before := cloneAgentState(h.latest)
	// The actor can stop attempts as soon as session cancellation releases
	// agentCall, even if its callback goroutine is still running. Copy task
	// nodes before dispatch so its staging never copies live attempt bookkeeping.
	// Payloads are immutable here; only their headers and task fields need copies.
	base := slices.Clone(h.v2.tasks)
	for i, t := range base {
		node := *t
		base[i] = &node
	}
	var saved agent.State
	var effects agent.Effects
	var tasks []*localTask
	var saving atomic.Bool
	var handleFailed atomic.Bool
	var refusals []taskRefusal
	var timers []hostTimer
	var timerGeneration uint64
	var admittedControls []controlRecord
	var admittedFloor uint64
	err := s.agentCall(func(ctx context.Context) error {
		var err error
		s.worker.metrics.agentEvents.Add(1)
		callbackEvent := event
		if event.Control != nil {
			message := *event.Control
			message.Data = slices.Clone(message.Data)
			callbackEvent.Control = &message
		}
		effects, err = h.v2.instance.Handle(ctx, callbackEvent)
		if err != nil {
			handleFailed.Store(true)
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validateV2Effects(event.Kind, effects); err != nil {
			return err
		}
		effects = filterAudioEffects(effects, before.V2.CancelFloor)
		timers, timerGeneration, err = prepareTimers(before.V2.Timers, before.V2.TimerGeneration, event, effects, s.taskNow())
		if err != nil {
			return err
		}
		if err = s.validateAudioEffects(effects, event, base); err != nil {
			return err
		}
		s.mu.Lock()
		projection := s.projectedPlaybackLocked(effects, event).snapshot()
		s.mu.Unlock()
		// Refusals are compact terminal intents, delivered as later transactions.
		// Never recursively call Handle while accepting another callback's effects.
		refusals = uniqueTaskRefusals(before.V2.Capacity)
		if event.Kind == agent.TaskMessage && len(refusals) > 0 && refusals[0].ID == event.Result.TaskID {
			refusals = refusals[1:]
		}
		var refused []agent.TaskResult
		tasks, refused, err = s.prepareTaskEffects(event, effects, base, refusals, projection)
		if err != nil {
			return err
		}
		for _, r := range refused {
			refusals = append(refusals, taskRefusal{ID: r.TaskID, Turn: r.Turn})
		}
		// Stage consumption on an owned node before checking the complete metadata.
		// The accepted cursor must not make a previously checked envelope overflow.
		if event.Kind == agent.TaskMessage {
			for i, t := range tasks {
				if t.intent.ID == event.Result.TaskID {
					next := *t
					next.intent.Cursor = event.Result.Sequence
					if t.results[0].Chunk.Audio != nil {
						next.audio = append(slices.Clone(t.audio), t.results[0])
						next.mailboxBytes -= taskAudioRefBytes(t.results[0])
					} else {
						next.mailboxBytes -= taskResultBytes(t.results[0])
					}
					next.results = t.results[1:]
					s.retireTaskRequest(&next)
					tasks[i] = &next
				}
			}
		}
		prior := uniqueTaskRefusals(before.V2.Capacity)
		if event.Kind == agent.TaskMessage && len(prior) > 0 && prior[0].ID == event.Result.TaskID {
			prior = prior[1:]
		}
		tasks, refusals, err = s.fitTaskRefusals(base, tasks, prior, refusals, projection)
		if err != nil {
			return err
		}
		saving.Store(true)
		saved, err = s.measuredSave(ctx, h.v2.instance)
		if err != nil {
			return fmt.Errorf("%w: %w", legacy.ErrSave, err)
		}
		if err := s.validateV2StateMetadata(saved); err != nil {
			if errors.Is(err, agent.ErrStateTooLarge) || errors.Is(err, agent.ErrHistoryTooLarge) {
				return fmt.Errorf("%w: %w", errSnapshotAdmissionCapacity, err)
			}
			return err
		}
		if err := s.hostEffectsBudget(effects, event, timers, tasks, refusals, saved.MoveOnlyLength, timerGeneration); err != nil {
			return err
		}
		candidate := cloneAgentState(before)
		candidate.State = legacy.State{Version: saved.Version, Bytes: saved.Bytes}
		candidate.History = saved.History
		candidate.V2.HasMoveOnly, candidate.V2.MoveOnlyLength = saved.HasMoveOnly, saved.MoveOnlyLength
		candidate.V2.Timers, candidate.V2.TimerGeneration = timers, timerGeneration
		candidate.V2.Playback = projection
		candidate.V2.Capacity = refusals
		candidate.V2.Tasks = make([]taskIntent, len(tasks))
		for i, t := range tasks {
			candidate.V2.Tasks[i] = t.intent
		}
		if effects.CancelBefore != nil {
			candidate.V2.CancelFloor = max(candidate.V2.CancelFloor, *effects.CancelBefore)
		}
		if event.Kind == agent.Started {
			candidate.V2.Started = true
		}
		if event.Kind == agent.AudioInput {
			candidate.Progress = event.Input.Progress
			candidate.Progress.Consumed = before.Progress.Consumed + 1
		}
		stageControl(candidate.V2, event)
		// Reserve future task terminal fields as well as the immediate effects.
		// Compaction remains local until the whole callback transaction commits.
		if len(candidate.V2.Controls) > 0 {
			guarded := taskMetadataGuard(*candidate.V2)
			s.compactControlMetadata(&guarded, min(h.v2.registration.Bounds.MaxPendingBytes, v2HostMetadataBytes))
			candidate.V2.Controls, candidate.V2.ControlFloor = guarded.Controls, guarded.ControlFloor
		}
		admittedControls, admittedFloor = candidate.V2.Controls, candidate.V2.ControlFloor
		candidate.Revision++
		_, err = s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: before, event: true, sideEffect: slices.ContainsFunc(effects.StartTasks, func(t agent.TaskSpec) bool { return t.Class == agent.SideEffecting })})
		if err != nil {
			return err
		}
		saved = cloneV2State(saved)
		return nil
	}, !adoption)
	if err != nil {
		if adoption && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			return err
		}
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		if restoreErr := s.agentCall(func(ctx context.Context) error { return h.v2.instance.Restore(ctx, cloneV2State(v2State(before))) }); restoreErr != nil {
			return fmt.Errorf("%w: %w", legacy.ErrRestore, restoreErr)
		}
		s.checkpoint.rollback(before)
		if reserveErr := s.taskBudget(h.v2.tasks, before.V2.MoveOnlyLength); reserveErr != nil {
			return reserveErr
		}
		if event.Kind == agent.Control && errors.Is(err, errSnapshotAdmissionCapacity) {
			return agent.ErrCapacity
		}
		if errors.Is(err, errSnapshotAdmissionCapacity) && !adoption {
			return s.refuseSnapshotEvent(event, before)
		}
		// Control has no consumed input cursor. Quarantine a failed Handle and
		// restore accepted state before refusing without advancing its sequence.
		// Save/Restore failures and invalid effects remain terminal.
		if event.Kind == agent.Control && !saving.Load() && (handleFailed.Load() || errors.Is(err, context.DeadlineExceeded)) {
			return fmt.Errorf("%w: %v", agent.ErrControlRejected, err)
		}
		// A late AudioInput Handle is quarantined before Restore, just as in the
		// legacy host. It cannot advance accepted state or consumed-input progress.
		// Capacity TaskMessages get their own transaction and fail closed too.
		if !adoption && event.Kind == agent.AudioInput && errors.Is(err, context.DeadlineExceeded) && !saving.Load() {
			s.worker.agents.drops.Add(1)
			return nil
		}
		// Save deadlines, other callback failures, invalid state and rejected effects
		// remain terminal. No later event may publish a rejected callback's changes.
		if errors.Is(err, context.DeadlineExceeded) && saving.Load() {
			return fmt.Errorf("%w: %w", legacy.ErrSave, err)
		}
		if !adoption && errors.Is(err, errControlReceiptBackpressure) {
			s.throttleReceiptEvent(event)
		}
		return err
	}
	// actor owns latest; publish only after Handle, effects and Save all succeed.
	next := cloneAgentState(before)
	next.State = legacy.State{Version: saved.Version, Bytes: saved.Bytes}
	next.History = saved.History
	next.V2.HasMoveOnly, next.V2.MoveOnlyLength = saved.HasMoveOnly, saved.MoveOnlyLength
	next.V2.Capacity = refusals
	stageControl(next.V2, event)
	next.V2.Controls, next.V2.ControlFloor = admittedControls, admittedFloor
	if event.Kind == agent.Started {
		next.V2.Started = true
	}
	if event.Kind == agent.AudioInput {
		next.Progress = event.Input.Progress
		next.Progress.Consumed = before.Progress.Consumed + 1
	}
	s.acceptTaskEffects(tasks, effects)
	if effects.CancelBefore != nil {
		next.V2.CancelFloor = max(next.V2.CancelFloor, *effects.CancelBefore)
	}
	next.V2.Timers = timers
	next.V2.TimerGeneration = timerGeneration
	// App state, timer/task cursors and playback admission share one publication
	// fence. Neither the pacer nor a snapshot can observe half this transaction.
	s.mu.Lock()
	h.v2.playback = s.projectedPlaybackLocked(effects, event)
	next.V2.Playback = s.playbackSnapshotLocked()
	next.V2.Tasks = cloneTaskIntents(h.latest.V2.Tasks)
	next.Revision++
	h.latest = next
	if !s.fenced.Load() && s.ctx.Err() == nil {
		s.state.Agent = cloneAgentState(next)
	}
	s.mu.Unlock()
	s.wakePacer()
	if !agentStateEqual(before, next) {
		s.checkpoint.request()
	}
	return nil
}

func (s *session) processV2Audio(in agentInput) error {
	in.input.Progress.Consumed = s.agent.latest.Progress.Consumed + 1
	id := "audio/" + strconv.FormatUint(uint64(in.input.Progress.SSRC), 10) + "/" + strconv.FormatUint(in.input.Progress.Index, 10)
	return s.handleV2Event(agent.Event{ID: agent.EventID(id), Kind: agent.AudioInput, Input: &in.input})
}

// Scratch stays session-owned until export claims it. Close also handles a
// successful actor reply abandoned by a cancelled flush caller.
type moveScratch struct {
	release func()
	bytes   atomic.Int64
}

// The actor reserves actual accepted payloads before pausing. If queued work
// grows them, reserveMoveBytes grows this hold before accepting that work.
func (s *session) reserveV2ExportScratch() error {
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	if s.moveClosed {
		return context.Canceled
	}
	if s.exportScratch != nil {
		return nil
	}
	n := s.agent.latest.V2.MoveOnlyLength + audioRefsBytes(s.agent.v2.playback.queue)
	for _, t := range s.agent.v2.tasks {
		n += len(t.request)
		n += t.mailboxBytes
		n += len(t.intent.ID) + 64
	}

	scratch := int64(8 * n)
	if !s.worker.reserveMoveBytes(scratch) {
		return ErrExportNotFlushed
	}
	hold := &moveScratch{}
	hold.bytes.Store(scratch)
	hold.release = sync.OnceFunc(func() { s.worker.moveBytes.Add(-hold.bytes.Swap(0)) })
	s.exportScratch = hold
	return nil
}
func (s *session) claimV2ExportScratch() (func(), error) {
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	if s.moveClosed || s.exportScratch == nil {
		return func() {}, context.Canceled
	}
	release := s.exportScratch.release
	s.exportScratch = nil
	return release, nil
}

// Called by the paused actor exactly once per export request, after the last
// accepted Save. The scratch was reserved before queue draining began.
func (s *session) saveV2MoveOnly(deadline context.Context) (agentMoveExport, error) {
	s.mu.Lock()
	audio := slices.Clone(s.agent.v2.playback.queue)
	s.mu.Unlock()
	export := agentMoveExport{tasks: s.exportTasks(), audio: audio}
	state := v2State(s.agent.latest)
	if !state.HasMoveOnly {
		return export, nil
	}
	err := s.agentCall(func(ctx context.Context) error {
		if limit, ok := deadline.Deadline(); ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, limit)
			defer cancel()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := s.agent.v2.instance.(agent.MoveOnlyState).SaveMoveOnly(ctx)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(data) != state.MoveOnlyLength {
			return errors.New("agent: move-only length differs from accepted Save")
		}
		export.payload = slices.Clone(data)
		return nil
	}, true)
	if err != nil {
		return agentMoveExport{}, err
	}
	return export, nil
}

func v2EnvelopeEqual(a, b *v2Envelope) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Name == b.Name && a.Contract == b.Contract && slices.Equal(a.TaskKinds, b.TaskKinds) && a.Bounds == b.Bounds && limitsEqual(a.Limits, b.Limits) &&
		a.HasMoveOnly == b.HasMoveOnly && a.MoveOnlyLength == b.MoveOnlyLength && a.Started == b.Started && taskIntentsEqual(a.Tasks, b.Tasks) && slices.Equal(a.Capacity, b.Capacity) &&
		a.ControlHigh == b.ControlHigh && a.ControlFloor == b.ControlFloor && slices.Equal(a.Controls, b.Controls) &&
		a.CancelFloor == b.CancelFloor && a.TimerGeneration == b.TimerGeneration && slices.Equal(a.Timers, b.Timers) && playbackEqual(a.Playback, b.Playback)
}

func limitsEqual(a, b *agent.Limits) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func validateV2Wire(data []byte) error {
	// Size includes nested base64 wrapping at the real private HTTP boundary.
	for _, value := range []any{ExportReply{State: data}, ResumeRequest{State: data}} {
		wire, err := json.Marshal(value)
		if err != nil {
			return err
		}
		// Leave room for lease, margins, ages and replay metadata on actual resume.
		if len(wire) >= v2MaxWireBytes-(64<<10) {
			return fmt.Errorf("%w: exact export exceeds private API wire budget", agent.ErrCapacity)
		}
	}
	return nil
}
