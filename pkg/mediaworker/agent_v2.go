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

	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
)

const (
	v2SessionStateVersion = 8
	v2Contract            = "agent/v2"
	v2MoveSessionBytes    = 1 << 20
	v2MoveWorkerBytes     = 64 << 20
	v2MaxWireBytes        = 4 << 20
	v2HostMetadataBytes   = 2 << 10
	// Keep one export's worst-case copy/base64 scratch available at admission.
	v2ExportScratchBytes = 8 * v2MoveSessionBytes
)

// Only v8 contains this subtree. The legacy state/progress encoding is unchanged.
type v2Envelope struct {
	Name           string
	Contract       string
	TaskKinds      []string
	Bounds         agenthost.Bounds
	HasMoveOnly    bool
	MoveOnlyLength int
	Started        bool
	Tasks          []taskIntent  `json:",omitempty"`
	Capacity       []taskRefusal `json:",omitempty"`
}

type v2Host struct {
	instance     agent.Agent
	registration agenthost.Registration
	ready        chan struct{}
	tasks        []*localTask // actor-owned
	pendingInput *agentInput  // one backpressured, unconsumed input; actor-owned
	emissions    chan taskEmission
	force        chan struct{}
}

// movePayload exists only in exact exports, including an empty exact export.
// A checkpoint never includes this field or calls SaveMoveOnly.
type movePayload struct {
	Bytes []byte
	Tasks []taskMove `json:",omitempty"`
}

func (s *session) initV2Agent(r *agenthost.Adapter, restoring bool, opts ResumeOptions, move *movePayload) error {
	reg := r.Registration
	if reg.Name == "" || reg.Name == "echo" || reg.Name == "demo" || reg.Factory == nil ||
		reg.Bounds.MaxStateBytes <= 0 || reg.Bounds.MaxStateBytes > 64<<10 || reg.Bounds.MaxPendingBytes <= 0 ||
		reg.Bounds.MaxMailboxBytes <= 0 || reg.Bounds.MaxMailboxBytes > v2MoveSessionBytes || reg.Bounds.MaxActiveTasks <= 0 ||
		reg.Bounds.MaxAudioBytes < 0 || reg.Bounds.MaxAudioDuration < 0 {
		return s.agentError(fmt.Errorf("%w: invalid internal registration", agent.ErrIncompatibleAgent))
	}
	for _, kind := range reg.TaskKinds {
		if kind == "" {
			return s.agentError(fmt.Errorf("%w: empty task kind", agent.ErrIncompatibleAgent))
		}
	}
	if restoring {
		e := s.state.Agent.V2
		if e == nil || e.Contract != v2Contract || e.Name != reg.Name || !slices.Equal(e.TaskKinds, reg.TaskKinds) || e.Bounds != reg.Bounds {
			return s.agentError(fmt.Errorf("%w: %w", legacy.ErrRestore, agent.ErrIncompatibleAgent))
		}
	} else {
		s.state.Version = v2SessionStateVersion
		s.state.Agent.V2 = &v2Envelope{Name: reg.Name, Contract: v2Contract, TaskKinds: slices.Clone(reg.TaskKinds), Bounds: reg.Bounds}
	}
	metadata, err := json.Marshal(v2Envelope{Name: reg.Name, Contract: v2Contract, TaskKinds: reg.TaskKinds, Bounds: reg.Bounds})
	if err != nil {
		return s.agentError(err)
	}
	if len(metadata) > min(reg.Bounds.MaxPendingBytes, v2HostMetadataBytes) {
		return s.agentError(fmt.Errorf("%w: registration metadata exceeds internal host budget", agent.ErrIncompatibleAgent))
	}
	// Crash metadata has no payload to copy or reserve. Exact restores only
	// reserve the one move-payload copy they make, for its actual byte length.
	if opts.Kind == legacy.Takeover || opts.SequenceMargin > 0 {
		move = nil
	}
	if restoring && move != nil && (len(move.Bytes) > 0 || len(move.Tasks) > 0) {
		scratch := int64(len(move.Bytes))
		for _, t := range move.Tasks {
			scratch += int64(len(t.Request))
			for _, r := range t.Results {
				scratch += int64(taskResultBytes(r))
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

	h := &v2Host{registration: reg, ready: make(chan struct{}), emissions: make(chan taskEmission), force: make(chan struct{}, 1)}
	s.agent.v2 = h
	// Factory follows the legacy factory rule: quick, nonblocking construction.
	h.instance = reg.Factory(agent.FactoryArgs{SessionID: s.id, AgentName: reg.Name, MaxStateBytes: reg.Bounds.MaxStateBytes})
	if h.instance == nil {
		return s.agentError(errors.New("agent: nil v2 factory result"))
	}
	if restoring {
		state := v2State(s.state.Agent)
		if err := s.validateV2StateMetadata(state); err != nil {
			return s.v2RestoreError(err)
		}
		if move != nil {
			if err := s.reserveMoveBytes(int64(state.MoveOnlyLength)); err != nil {
				return err
			}
		}
		s.durableAgent = cloneAgentState(s.state.Agent)
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
			saved, err = h.instance.Save(ctx)
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
	return state
}
func v2State(pair agentState) agent.State {
	return agent.State{Version: pair.State.Version, Bytes: pair.State.Bytes, HasMoveOnly: pair.V2.HasMoveOnly, MoveOnlyLength: pair.V2.MoveOnlyLength}
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
func (s *session) validateV2StateMetadata(state agent.State) error {
	if state.Version == 0 {
		return agent.ErrVersion
	}
	if len(state.Bytes) > s.agent.v2.registration.Bounds.MaxStateBytes {
		return agent.ErrStateTooLarge
	}
	if state.MoveOnlyLength < 0 || (!state.HasMoveOnly && state.MoveOnlyLength != 0) {
		return errors.New("agent: invalid move-only metadata")
	}
	if state.MoveOnlyLength > s.agent.v2.registration.Bounds.MaxMailboxBytes || state.MoveOnlyLength > v2MoveSessionBytes {
		return errors.New("agent: move-only data exceeds session budget")
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
	if delta > 0 && !s.worker.reserveMoveBytesWithin(delta, v2MoveWorkerBytes-v2ExportScratchBytes) {
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
	return w.reserveMoveBytesWithin(n, v2MoveWorkerBytes)
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
	s.state.Agent.V2.HasMoveOnly = saved.HasMoveOnly
	s.state.Agent.V2.MoveOnlyLength = saved.MoveOnlyLength
	s.state.Agent.Revision++
}

func validateV2Effects(kind agent.EventKind, e agent.Effects) error {
	if kind == agent.Resumed && len(e.StartTasks) != 0 {
		return errors.New("agent: StartTasks from Resumed is forbidden")
	}
	switch {
	case e.CancelBefore != nil:
		return errors.New("agent: task cancellation not supported in this slice")
	case len(e.SetTimers) > 0 || len(e.CancelTimers) > 0:
		return errors.New("agent: timers not supported in this slice")
	case len(e.Audio) > 0 || e.ClearAudioBefore != nil || e.NotifyPlaybackFinished != nil:
		return errors.New("agent: pacer effects not supported in this slice")
	}
	return nil
}

func (s *session) handleV2Event(event agent.Event) error {
	return s.handleV2EventMode(event, false)
}

func (s *session) handleV2EventMode(event agent.Event, adoption bool) error {
	// Reserved and future kinds have no delivery route in this slice. Do not call
	// the agent or Save, so an unknown event cannot alter accepted state.
	switch event.Kind {
	case agent.Started, agent.AudioInput, agent.Resumed, agent.TaskMessage:
	default:
		return nil
	}
	h := s.agent
	before := cloneAgentState(h.latest)
	var saved agent.State
	var effects agent.Effects
	var tasks []*localTask
	var saving atomic.Bool
	var refusals []taskRefusal
	err := s.agentCall(func(ctx context.Context) error {
		var err error
		effects, err = h.v2.instance.Handle(ctx, event)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validateV2Effects(event.Kind, effects); err != nil {
			return err
		}
		// Refusals are compact terminal intents, delivered as later transactions.
		// Never recursively call Handle while accepting another callback's effects.
		refusals = slices.Clone(before.V2.Capacity)
		if event.Kind == agent.TaskMessage && len(refusals) > 0 && refusals[0].ID == event.Result.TaskID {
			refusals = refusals[1:]
		}
		var refused []agent.TaskResult
		tasks, refused, err = s.prepareTaskEffects(event, effects, h.v2.tasks, refusals)
		if err != nil {
			return err
		}
		for _, r := range refused {
			refusals = append(refusals, taskRefusal{ID: r.TaskID, Turn: r.Turn})
		}
		intents := make([]taskIntent, len(tasks))
		for i, t := range tasks {
			intents[i] = t.intent
		}
		if err := s.taskMetadataBudget(intents, refusals, false); err != nil {
			if errors.Is(err, agent.ErrCapacity) || errors.Is(err, ErrMoveBudget) {
				return errTaskAdmissionBackpressure
			}
			return err
		}
		saving.Store(true)
		saved, err = h.v2.instance.Save(ctx)
		if err != nil {
			return fmt.Errorf("%w: %w", legacy.ErrSave, err)
		}
		if err := s.validateV2StateMetadata(saved); err != nil {
			return err
		}
		if err := s.taskBudgetWithRefusals(tasks, saved.MoveOnlyLength, refusals); err != nil {
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
		if reserveErr := s.taskBudget(h.v2.tasks, before.V2.MoveOnlyLength); reserveErr != nil {
			return reserveErr
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
		return err
	}
	// actor owns latest; publish only after Handle, effects and Save all succeed.
	next := cloneAgentState(before)
	next.State = legacy.State{Version: saved.Version, Bytes: saved.Bytes}
	next.V2.HasMoveOnly, next.V2.MoveOnlyLength = saved.HasMoveOnly, saved.MoveOnlyLength
	next.V2.Capacity = refusals
	if event.Kind == agent.Started {
		next.V2.Started = true
	}
	if event.Kind == agent.AudioInput {
		next.Progress = event.Input.Progress
		next.Progress.Consumed = before.Progress.Consumed + 1
	}
	if event.Kind == agent.TaskMessage {
		for _, t := range tasks {
			if t.intent.ID == event.Result.TaskID {
				t.intent.Cursor = event.Result.Sequence
				t.mailboxBytes -= taskResultBytes(t.results[0])
				t.results = t.results[1:]
				s.retireTaskRequest(t)
			}
		}
	}
	s.acceptTaskEffects(tasks, effects)
	next.V2.Tasks = cloneTaskIntents(h.latest.V2.Tasks)
	next.Revision++
	h.latest = next
	s.publishAgent()
	if !agentStateEqual(before, next) {
		s.wantSnapshot()
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
	n := s.agent.latest.V2.MoveOnlyLength
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
	export := agentMoveExport{tasks: s.exportTasks()}
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
	return a.Name == b.Name && a.Contract == b.Contract && slices.Equal(a.TaskKinds, b.TaskKinds) && a.Bounds == b.Bounds &&
		a.HasMoveOnly == b.HasMoveOnly && a.MoveOnlyLength == b.MoveOnlyLength && a.Started == b.Started && taskIntentsEqual(a.Tasks, b.Tasks)
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
