package mediaworker

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/relaishq/relais/internal/taskrun"
	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
)

const (
	taskNever       = "never_launched"
	taskStarted     = "may_have_started"
	taskTerminal    = "terminal"
	taskCanceled    = "canceled"
	taskDetailBytes = 256
)

// Request is durable only for SideEffecting. ReadOnly payloads and all results
// live solely in taskMove. Digest is computed once, at admission.
type taskCause struct {
	SSRC  uint32 `json:"s"`
	Index uint64 `json:"i"`
}
type taskIntent struct {
	InputCause    *taskCause         `json:"a,omitempty"`
	ID            agent.TaskID       `json:"i"`
	Step          string             `json:"s"`
	Kind          string             `json:"k"`
	Turn          agent.Turn         `json:"t,omitempty"`
	Class         agent.TaskClass    `json:"c"`
	Resume        agent.ResumePolicy `json:"r,omitempty"`
	Request       []byte             `json:"q,omitempty"`
	RequestLength int                `json:"l,omitempty"`
	Digest        string             `json:"h"`
	Deadline      time.Time          `json:"d,omitzero"`
	Timeout       time.Duration      `json:"o,omitempty"`
	Phase         string             `json:"p"`
	Cursor        uint64             `json:"u,omitempty"`
	Published     uint64             `json:"n,omitempty"`
	Terminal      agent.Outcome      `json:"z,omitempty"`
	Final         bool               `json:"f,omitempty"`
}
type taskMove struct {
	ID      agent.TaskID
	Request []byte
	Results []agent.TaskResult
	// An exact export can prove a prepared handler did not cross admission.
	NeverLaunched bool
}
type localTask struct {
	intent        taskIntent
	request       []byte
	results       []agent.TaskResult
	launched      bool
	prepared      bool
	interrupted   bool
	cancel        context.CancelFunc
	attempt       uint64
	mailboxBytes  int
	terminalBytes int
}
type taskEmission struct {
	id       agent.TaskID
	attempt  uint64
	ctx      context.Context
	sequence uint64
	chunk    agent.Chunk
	final    bool
	outcome  agent.Outcome
	detail   string
	reply    chan error
}

func deriveTaskID(session, name string, cause agent.EventID, step string) agent.TaskID {
	h := sha256.New()
	h.Write([]byte("relais/task/v1"))
	for _, part := range []string{session, name, string(cause), step} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		h.Write(size[:])
		h.Write([]byte(part))
	}
	return agent.TaskID(base64.RawURLEncoding.EncodeToString(h.Sum(nil)))
}
func taskDigest(spec agent.TaskSpec) string {
	data, _ := json.Marshal(spec)
	sum := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func (s *session) taskNow() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.storeClockAt.IsZero() {
		return s.storeClock.Add(time.Since(s.storeClockAt))
	}
	// Direct, storeless sessions have a single process clock domain.
	return time.Now()
}
func (s *session) taskBudget(tasks []*localTask, appBytes int) error {
	n := int64(appBytes)
	intents := make([]taskIntent, 0, len(tasks))
	for _, t := range tasks {
		intents = append(intents, t.intent)
		// Reserve both the host request and the owned handler copy from real bytes.
		n += int64(2 * len(t.request))
		n += int64(t.mailboxBytes)
		if !t.intent.Final {
			n += int64(t.terminalBytes)
		}
	}
	envelope := *s.agent.latest.V2
	envelope.Tasks = intents
	metadata, _ := json.Marshal(envelope)
	if len(metadata) > min(s.agent.v2.registration.Bounds.MaxPendingBytes, v2HostMetadataBytes) {
		return agent.ErrCapacity
	}
	if n > int64(s.agent.v2.registration.Bounds.MaxMailboxBytes) {
		return agent.ErrCapacity
	}
	return s.reserveMoveBytes(n)
}
func (s *session) prepareTaskEffects(cause agent.Event, effects agent.Effects) ([]*localTask, error) {
	h := s.agent.v2
	next := slices.Clone(h.tasks)
	steps := map[string]bool{}
	now := s.taskNow()
	for _, spec := range effects.StartTasks {
		if spec.Step == "" || spec.Kind == "" || spec.Timeout < 0 || steps[spec.Step] ||
			(spec.Class != agent.ReadOnly && spec.Class != agent.SideEffecting) ||
			(spec.Class == agent.SideEffecting && spec.Resume != agent.Retry && spec.Resume != agent.Abandon) ||
			(spec.Resume != "" && spec.Resume != agent.Retry && spec.Resume != agent.Abandon) {
			return nil, fmt.Errorf("%w: invalid task specification or duplicate Step", agent.ErrIncompatibleAgent)
		}
		if h.registration.Tasks[spec.Kind] == nil {
			return nil, fmt.Errorf("%w: missing task handler %q", agent.ErrIncompatibleAgent, spec.Kind)
		}
		if len(spec.Request) > h.registration.Bounds.MaxMailboxBytes || len(spec.Step)+len(spec.Kind) > h.registration.Bounds.MaxPendingBytes || (spec.Class == agent.SideEffecting && len(spec.Request) > h.registration.Bounds.MaxPendingBytes) {
			return nil, agent.ErrCapacity
		}
		steps[spec.Step] = true
		id := deriveTaskID(s.id, h.registration.Name, cause.ID, spec.Step)
		digest := taskDigest(spec)
		duplicate := false
		for _, t := range next {
			if t.intent.ID == id {
				if t.intent.Digest != digest {
					return nil, fmt.Errorf("%w: conflicting task arguments", agent.ErrIncompatibleAgent)
				}
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		intent := taskIntent{ID: id, Step: spec.Step, Kind: spec.Kind, Turn: spec.Turn, Class: spec.Class, Resume: spec.Resume, RequestLength: len(spec.Request), Digest: digest, Timeout: spec.Timeout, Phase: taskNever}
		if cause.Kind == agent.AudioInput {
			intent.InputCause = &taskCause{SSRC: cause.Input.Progress.SSRC, Index: cause.Input.Progress.Index}
		}
		if spec.Timeout > 0 {
			intent.Deadline = now.Add(spec.Timeout)
		}
		if spec.Class == agent.SideEffecting {
			intent.Request = spec.Request
		}
		t := &localTask{intent: intent, request: spec.Request}
		t.terminalBytes = taskTerminalBytes(intent)
		next = append(next, t)
		if err := s.taskBudget(next, s.agent.latest.V2.MoveOnlyLength); err != nil {
			return nil, err
		}
	}
	return next, nil
}
func (s *session) publishTaskIntents() {
	h := s.agent.v2
	s.agent.latest.V2.Tasks = nil
	if len(h.tasks) > 0 {
		s.agent.latest.V2.Tasks = make([]taskIntent, len(h.tasks))
	}
	for i, t := range h.tasks {
		s.agent.latest.V2.Tasks[i] = cloneTaskIntent(t.intent)
	}
}
func cloneTaskIntent(t taskIntent) taskIntent { t.Request = slices.Clone(t.Request); return t }
func cloneTaskIntents(ts []taskIntent) []taskIntent {
	if ts == nil {
		return nil
	}
	out := make([]taskIntent, len(ts))
	for i, t := range ts {
		out[i] = cloneTaskIntent(t)
	}
	return out
}
func (s *session) acceptTaskEffects(tasks []*localTask, effects agent.Effects) {
	h := s.agent.v2
	for _, t := range tasks {
		if !slices.Contains(h.tasks, t) {
			t.request = slices.Clone(t.request)
			t.intent.Request = slices.Clone(t.intent.Request)
		}
	}
	h.tasks = tasks
	for _, id := range effects.CancelTasks {
		for _, t := range h.tasks {
			if t.intent.ID == id && !t.intent.Final {
				s.cancelTask(t)
			}
		}
	}
	s.publishTaskIntents()
}
func (s *session) cancelTask(t *localTask) {
	if t.cancel != nil {
		t.cancel()
	}
	t.attempt++ // fence all parked and late results from this attempt
	t.launched = false
	t.prepared = false
	t.results = nil
	t.mailboxBytes = 0
	t.intent.Published = t.intent.Cursor
	t.intent.Phase = taskCanceled
	s.terminalTask(t, agent.Canceled, "")
}
func (s *session) terminalTask(t *localTask, outcome agent.Outcome, detail string) {
	if len(detail) > taskDetailBytes {
		detail = detail[:taskDetailBytes]
	}
	t.intent.Published++
	t.results = append(t.results, agent.TaskResult{TaskID: t.intent.ID, Sequence: t.intent.Published, Turn: t.intent.Turn, Outcome: outcome, Final: true, Detail: detail})
	t.mailboxBytes += taskResultBytes(t.results[len(t.results)-1])
	t.intent.Terminal = outcome
	t.intent.Final = true
	if t.intent.Phase != taskCanceled {
		t.intent.Phase = taskTerminal
	}
	t.prepared = false
	t.launched = false
}
func (s *session) taskStateChanged() {
	before := s.agent.latest.V2.Tasks
	s.publishTaskIntents()
	if taskIntentsEqual(before, s.agent.latest.V2.Tasks) {
		return
	}
	s.agent.latest.Revision++
	s.publishAgent()
	s.wantSnapshot()
}
func (s *session) launchTasks() {
	h := s.agent.v2
	s.mu.Lock()
	paused := s.agent.paused || s.fenced.Load() || s.ctx.Err() != nil
	var durable []taskIntent
	if s.durableAgent.V2 != nil {
		durable = cloneTaskIntents(s.durableAgent.V2.Tasks)
	}
	s.mu.Unlock()
	if paused {
		return
	}
	active := 0
	changed := false
	for _, t := range h.tasks {
		if t.interrupted && !t.intent.Final {
			t.interrupted = false
			t.launched = false
			t.prepared = false
			if t.intent.Resume == agent.Abandon || (t.intent.Class == agent.ReadOnly && t.intent.Published > 0 && t.intent.Resume != agent.Retry) {
				s.terminalTask(t, agent.OutcomeUnknown, "task interrupted by unsuccessful export")
			} else {
				t.intent.Phase = taskNever
			}
			changed = true
		}
	}
	for _, t := range h.tasks {
		if t.launched || t.prepared {
			active++
		}
	}
	force := false
	for _, t := range h.tasks {
		if t.intent.Final || t.launched {
			continue
		}
		if !t.intent.Deadline.IsZero() && !s.taskNow().Before(t.intent.Deadline) {
			s.terminalTask(t, agent.Timeout, "")
			changed = true
			continue
		}
		if !t.prepared && active >= h.registration.Bounds.MaxActiveTasks {
			continue
		}
		if t.intent.Class == agent.SideEffecting {
			if !t.prepared {
				t.prepared = true
				active++
				t.intent.Phase = taskStarted
				changed = true
				force = true
			}
			granted := false
			for _, d := range durable {
				if d.ID == t.intent.ID && d.Digest == t.intent.Digest && d.Phase == taskStarted && !d.Final {
					granted = true
					break
				}
			}
			if !granted {
				continue
			}
		} else {
			active++
		}
		// The actor owns cancellation and attempt state. The final fence/route check
		// is under the same lock used by export and ownership loss.
		s.mu.Lock()
		if s.agent.paused || s.fenced.Load() || s.ctx.Err() != nil {
			s.mu.Unlock()
			break
		}
		if !t.intent.Deadline.IsZero() && !s.taskClockLocked().Before(t.intent.Deadline) {
			s.mu.Unlock()
			s.terminalTask(t, agent.Timeout, "")
			changed = true
			continue
		}
		ctx, cancel := context.WithCancel(s.ctx)
		if !t.intent.Deadline.IsZero() {
			left := t.intent.Deadline.Sub(s.taskClockLocked())
			cancel()
			ctx, cancel = context.WithTimeout(s.ctx, left)
		}
		t.cancel = cancel
		t.attempt++
		t.launched = true
		t.prepared = false
		t.intent.Phase = taskStarted
		attempt := t.attempt
		spec := agent.TaskSpec{Step: t.intent.Step, Kind: t.intent.Kind, Turn: t.intent.Turn, Class: t.intent.Class, Resume: t.intent.Resume, Request: t.request, Timeout: t.intent.Timeout}
		run := taskrun.New(spec, t.intent.ID, t.intent.Published, func(emitCtx context.Context, seq uint64, chunk agent.Chunk) error {
			return s.emitTask(ctx, taskEmission{id: t.intent.ID, attempt: attempt, ctx: emitCtx, sequence: seq, chunk: chunk})
		})
		handler := h.registration.Tasks[t.intent.Kind]
		s.mu.Unlock()
		go func(id agent.TaskID) {
			done := make(chan error, 1)
			go func() { done <- handler(ctx, run) }()
			var err error
			select {
			case err = <-done:
			case <-ctx.Done():
				err = ctx.Err()
			}
			outcome, detail := agent.OK, ""
			var failure *taskrun.Failure
			switch {
			case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
				outcome = agent.Timeout
			case ctx.Err() != nil || errors.Is(err, context.Canceled):
				outcome = agent.Canceled
			case errors.As(err, &failure):
				outcome, detail = failure.Outcome, failure.Detail
			case err != nil:
				outcome, detail = agent.Failed, err.Error()
			}
			_ = s.emitTask(s.ctx, taskEmission{id: id, attempt: attempt, ctx: s.ctx, final: true, outcome: outcome, detail: detail})
			cancel()
		}(t.intent.ID)
		changed = true
	}
	if changed {
		s.taskStateChanged()
	}
	if force {
		select {
		case h.force <- struct{}{}:
		default:
		}
	}
}
func (s *session) taskClockLocked() time.Time {
	if !s.storeClockAt.IsZero() {
		return s.storeClock.Add(time.Since(s.storeClockAt))
	}
	return time.Now()
}
func (s *session) emitTask(attemptCtx context.Context, e taskEmission) error {
	e.reply = make(chan error, 1)
	for {
		select {
		case <-attemptCtx.Done():
			return attemptCtx.Err()
		case <-e.ctx.Done():
			return e.ctx.Err()
		case s.agent.v2.emissions <- e:
		}
		select {
		case err := <-e.reply:
			if !errors.Is(err, agent.ErrBackpressure) {
				return err
			}
		case <-attemptCtx.Done():
			return attemptCtx.Err()
		case <-e.ctx.Done():
			return e.ctx.Err()
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-timer.C:
		case <-attemptCtx.Done():
			timer.Stop()
			return attemptCtx.Err()
		case <-e.ctx.Done():
			timer.Stop()
			return e.ctx.Err()
		}
	}
}
func chunkSize(c agent.Chunk, limit int) (int, bool) {
	n := len(c.Data)
	if n > limit {
		return n, false
	}
	if c.Audio != nil {
		n += len(c.Audio.Format)
		if n > limit {
			return n, false
		}
		for _, p := range c.Audio.Packets {
			if len(p)+3 > limit-n {
				return limit + 1, false
			}
			n += len(p) + 3
		}
	}
	return n, n <= limit
}
func cloneChunk(c agent.Chunk) agent.Chunk {
	c.Data = slices.Clone(c.Data)
	if c.Audio != nil {
		a := *c.Audio
		a.Packets = slices.Clone(a.Packets)
		for i, p := range a.Packets {
			a.Packets[i] = slices.Clone(p)
		}
		c.Audio = &a
	}
	return c
}
func (s *session) acceptEmission(e taskEmission) {
	h := s.agent.v2
	var t *localTask
	for _, candidate := range h.tasks {
		if candidate.intent.ID == e.id {
			t = candidate
			break
		}
	}
	s.mu.Lock()
	paused := s.agent.paused || s.fenced.Load()
	s.mu.Unlock()
	if paused || t == nil || t.attempt != e.attempt || t.intent.Final || e.ctx.Err() != nil {
		e.reply <- context.Canceled
		return
	}
	if e.final {
		s.terminalTask(t, e.outcome, e.detail)
	} else {
		if e.sequence <= t.intent.Published || e.sequence == ^uint64(0) {
			e.reply <- fmt.Errorf("%w: task sequence must increase past After", agent.ErrIncompatibleAgent)
			return
		}
		if _, ok := chunkSize(e.chunk, h.registration.Bounds.MaxMailboxBytes); !ok {
			e.reply <- agent.ErrCapacity
			return
		}
		r := agent.TaskResult{TaskID: e.id, Sequence: e.sequence, Turn: t.intent.Turn, Outcome: agent.OK, Chunk: e.chunk}
		bytes := taskResultBytes(r)
		if bytes+t.terminalBytes+2*len(t.request)+s.agent.latest.V2.MoveOnlyLength > h.registration.Bounds.MaxMailboxBytes {
			e.reply <- agent.ErrCapacity
			return
		}
		t.mailboxBytes += bytes
		t.results = append(t.results, r)
		err := s.taskBudget(h.tasks, s.agent.latest.V2.MoveOnlyLength)
		if err != nil {
			t.results = t.results[:len(t.results)-1]
			t.mailboxBytes -= bytes
			if errors.Is(err, agent.ErrCapacity) {
				err = agent.ErrBackpressure
			}
			e.reply <- err
			return
		}
		t.results[len(t.results)-1].Chunk = cloneChunk(e.chunk)
		t.intent.Published = e.sequence
	}
	s.taskStateChanged()
	e.reply <- nil
}
func (s *session) deliverTaskResult() (bool, error) {
	if workerprobe.HoldTaskResults(s.worker.localAddr) {
		return false, nil
	}
	for _, t := range s.agent.v2.tasks {
		if len(t.results) == 0 {
			continue
		}
		result := t.results[0]
		// The callback receives owned payloads; retained exact-export bytes cannot
		// be mutated by an agent. Admission already bounded the payload.
		result.Chunk = cloneChunk(result.Chunk)
		id := agent.EventID("task/" + string(result.TaskID) + "/" + strconv.FormatUint(result.Sequence, 10))
		if err := s.handleV2Event(agent.Event{ID: id, Kind: agent.TaskMessage, Result: &result}); err != nil {
			return false, err
		}
		// handleV2Event commits the cursor and removes this exact mailbox entry
		// atomically with application state, before publishing the pair.
		return true, nil
	}
	return false, nil
}
func (s *session) taskCheckpointLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.agent.v2.force:
			if err := s.persistSnapshotMode(s.ctx, true); err != nil {
				timer := time.NewTimer(100 * time.Millisecond)
				select {
				case <-timer.C:
					select {
					case s.agent.v2.force <- struct{}{}:
					default:
					}
				case <-s.ctx.Done():
					timer.Stop()
					return
				}
			}
			// Failed writes grant nothing. The normal snapshot cadence retries current
			// state; no immediate retry spin, no store read, and no Capacity outcome.
		}
	}
}

func taskIntentsEqual(a, b []taskIntent) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func (s *session) restoreTasks(move *movePayload) error {
	h := s.agent.v2
	seen := map[agent.TaskID]bool{}
	for _, intent := range s.agent.latest.V2.Tasks {
		if seen[intent.ID] || intent.ID == "" || h.registration.Tasks[intent.Kind] == nil || intent.Cursor > intent.Published ||
			(intent.Class != agent.ReadOnly && intent.Class != agent.SideEffecting) ||
			(intent.Class == agent.SideEffecting && intent.Resume != agent.Retry && intent.Resume != agent.Abandon) ||
			(intent.Phase != taskNever && intent.Phase != taskStarted && intent.Phase != taskTerminal && intent.Phase != taskCanceled) ||
			intent.RequestLength < 0 || (intent.Class == agent.SideEffecting && len(intent.Request) != intent.RequestLength) || (intent.Final && !taskrun.ValidOutcome(intent.Terminal)) || (intent.Class == agent.ReadOnly && len(intent.Request) > 0) {
			return fmt.Errorf("%w: invalid restored task intent", agent.ErrIncompatibleAgent)
		}
		seen[intent.ID] = true
		t := &localTask{intent: cloneTaskIntent(intent), request: slices.Clone(intent.Request), terminalBytes: taskTerminalBytes(intent)}
		var exact *taskMove
		if move != nil {
			for i := range move.Tasks {
				if move.Tasks[i].ID == intent.ID {
					if exact != nil {
						return fmt.Errorf("%w: duplicate task mailbox", agent.ErrIncompatibleAgent)
					}
					exact = &move.Tasks[i]
				}
			}
		}
		if move != nil && exact == nil {
			return fmt.Errorf("%w: missing exact task mailbox", agent.ErrIncompatibleAgent)
		}
		if exact != nil {
			t.request = slices.Clone(exact.Request)
			t.results = slices.Clone(exact.Results)
			seq := intent.Cursor
			for i, r := range t.results {
				if r.TaskID != intent.ID || r.Sequence <= seq || r.Sequence > intent.Published || !taskrun.ValidOutcome(r.Outcome) || r.Turn != intent.Turn || len(r.Detail) > taskDetailBytes || (r.Final && (i != len(t.results)-1 || !intent.Final || r.Outcome != intent.Terminal)) {
					return fmt.Errorf("%w: invalid exact task result", agent.ErrIncompatibleAgent)
				}
				t.mailboxBytes += taskResultBytes(r)
				t.results[i].Chunk = cloneChunk(r.Chunk)
				seq = r.Sequence
			}
			if (intent.Final && intent.Cursor < intent.Published && (len(t.results) == 0 || !t.results[len(t.results)-1].Final)) || seq != intent.Published || len(t.request) != intent.RequestLength {
				return fmt.Errorf("%w: incomplete exact task mailbox", agent.ErrIncompatibleAgent)
			}
			if exact.NeverLaunched && !intent.Final {
				t.intent.Phase = taskNever
			}
		}
		if exact != nil || intent.Class == agent.SideEffecting || intent.RequestLength == 0 {
			spec := agent.TaskSpec{Step: intent.Step, Kind: intent.Kind, Turn: intent.Turn, Class: intent.Class, Resume: intent.Resume, Request: t.request, Timeout: intent.Timeout}
			if taskDigest(spec) != intent.Digest {
				return fmt.Errorf("%w: restored task arguments do not match intent", agent.ErrIncompatibleAgent)
			}
		}
		if intent.Final && intent.Cursor == intent.Published {
			h.tasks = append(h.tasks, t)
			continue
		}
		if exact == nil {
			t.intent.Published = t.intent.Cursor
			t.intent.Final = false
			// A persisted terminal with no missing chunks can be regenerated exactly
			// from metadata. In particular a canceled/expired intent must never rerun.
			if intent.Final && intent.Published == intent.Cursor+1 && taskrun.ValidOutcome(intent.Terminal) {
				s.terminalTask(t, intent.Terminal, "")
				h.tasks = append(h.tasks, t)
				continue
			}
			if (intent.Class == agent.ReadOnly && (intent.RequestLength > 0 || (intent.Published > 0 && intent.Resume != agent.Retry))) || (intent.Phase != taskNever && intent.Resume == agent.Abandon) {
				s.terminalTask(t, agent.OutcomeUnknown, "task payload or uncertain effect lost on takeover")
			} else {
				t.intent.Phase = taskNever
			}
		} else if !intent.Final && intent.Phase != taskNever && !exact.NeverLaunched && intent.Resume == agent.Abandon {
			s.terminalTask(t, agent.OutcomeUnknown, "unfinished effect abandoned at move")
		} else if !intent.Final && intent.Class == agent.ReadOnly && intent.Published > 0 && intent.Resume != agent.Retry {
			s.terminalTask(t, agent.OutcomeUnknown, "task cannot reproduce its published prefix")
		}
		h.tasks = append(h.tasks, t)
	}
	if move != nil && len(move.Tasks) != len(h.tasks) {
		return fmt.Errorf("%w: unexpected exact task mailbox", agent.ErrIncompatibleAgent)
	}
	return s.taskBudget(h.tasks, s.agent.latest.V2.MoveOnlyLength)
}
func (s *session) stopTaskAttempts() {
	if s.agent.v2 == nil {
		return
	}
	for _, t := range s.agent.v2.tasks {
		t.attempt++
		if t.launched && !t.intent.Final {
			t.interrupted = true
		}
		if t.cancel != nil {
			t.cancel()
		}
	}
}
func (s *session) exportTasks() []taskMove {
	h := s.agent.v2
	out := make([]taskMove, len(h.tasks))
	for i, t := range h.tasks {
		out[i] = taskMove{ID: t.intent.ID, Request: slices.Clone(t.request), NeverLaunched: !t.launched, Results: slices.Clone(t.results)}
		for j, r := range out[i].Results {
			out[i].Results[j].Chunk = cloneChunk(r.Chunk)
		}
	}
	return out
}
func (s *session) v2AgentLoop() {
	h := s.agent.v2
	select {
	case <-h.ready:
	case <-s.ctx.Done():
		return
	}
	go s.taskCheckpointLoop()
	defer s.stopTaskAttempts()
	if !s.agent.latest.V2.Started {
		if err := s.handleV2Event(agent.Event{ID: agent.EventID("started/" + s.id), Kind: agent.Started}); err != nil {
			s.agentFailed(err)
			return
		}
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		// Export has priority over new work and over mailbox consumption.
		select {
		case request := <-s.agent.flush:
			if !s.finishAgentFlush(request) {
				return
			}
			continue
		default:
		}
		s.mu.Lock()
		paused := s.agent.paused
		s.mu.Unlock()
		if !paused {
			s.pruneTasks()
			s.launchTasks()
			if delivered, err := s.deliverTaskResult(); err != nil {
				s.agentFailed(err)
				return
			} else if delivered {
				continue
			}
		}
		select {
		case <-s.ctx.Done():
			return
		case request := <-s.agent.flush:
			if !s.finishAgentFlush(request) {
				return
			}
		case e := <-h.emissions:
			s.acceptEmission(e)
		case in := <-s.agent.queue:
			if err := s.processAudio(in); err != nil {
				s.agentFailed(err)
				return
			}
		case <-ticker.C:
		}
	}
}

func taskTerminalBytes(t taskIntent) int {
	b, _ := json.Marshal(agent.TaskResult{TaskID: t.ID, Sequence: ^uint64(0), Turn: t.Turn, Final: true, Outcome: agent.OutcomeUnknown, Detail: string(make([]byte, taskDetailBytes))})
	return len(b)
}

// Payload lengths are admitted before copying. Save/budget checks use cached
// accounting and never serialize or hash move-only result bytes.
func taskResultBytes(r agent.TaskResult) int {
	c := r.Chunk
	r.Chunk = agent.Chunk{}
	n := 0
	if len(c.Data) > 0 {
		n += base64.StdEncoding.EncodedLen(len(c.Data)) + 2
	}
	if c.Audio != nil {
		r.Chunk.Audio = &agent.EncodedAudio{}
		n += 6 * len(c.Audio.Format)
		for _, p := range c.Audio.Packets {
			n += base64.StdEncoding.EncodedLen(len(p)) + 3
		}
	}
	b, _ := json.Marshal(r)
	return len(b) + n
}

// Internal task events cannot be replayed past their committed cursor; Started
// cannot replay past its committed flag. Input-caused receipts additionally wait
// for the real SRTP replay window and consumed-input floor to pass the cause.
// An older retained export carries its own receipts. No wall-clock TTL is used.
func (s *session) pruneTasks() {
	h := s.agent.v2
	s.mu.Lock()
	var durable []taskIntent
	if s.durableAgent.V2 != nil {
		durable = cloneTaskIntents(s.durableAgent.V2.Tasks)
	}
	progress := s.state.Agent.Progress
	s.mu.Unlock()
	next := h.tasks[:0]
	for _, t := range h.tasks {
		ack := false
		if t.intent.Final && t.intent.Cursor == t.intent.Published && len(t.results) == 0 {
			for _, d := range durable {
				if d.ID == t.intent.ID && d.Final && d.Cursor == t.intent.Cursor {
					ack = true
					break
				}
			}
			if cause := t.intent.InputCause; cause != nil {
				ack = ack && progress.SSRC == cause.SSRC && progress.Index >= cause.Index && progress.Index-cause.Index >= replayWindow
			}
		}
		if !ack {
			next = append(next, t)
		}
	}
	if len(next) != len(h.tasks) {
		h.tasks = next
		_ = s.taskBudget(next, s.agent.latest.V2.MoveOnlyLength)
		s.taskStateChanged()
	}
}
