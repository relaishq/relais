package mediaworker

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/relaishq/relais/internal/taskrun"
	"github.com/relaishq/relais/internal/workerprobe"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// This host-generated refusal is distinct from errors returned by an agent.
var errTaskAdmissionBackpressure = fmt.Errorf("%w: pending task admission outcomes", agent.ErrBackpressure)

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

// A refusal has no request or result payload. These are terminal host intent
// metadata, not a durable result inbox.
type taskRefusal struct {
	ID   agent.TaskID `json:"i"`
	Turn agent.Turn   `json:"t,omitempty"`
}

func (r taskRefusal) result() agent.TaskResult {
	return agent.TaskResult{TaskID: r.ID, Turn: r.Turn, Sequence: 1, Outcome: agent.Capacity, Final: true, Detail: "task admission capacity"}
}

type taskMove struct {
	ID      agent.TaskID
	Request []byte
	Results []agent.TaskResult
	Audio   []agent.TaskResult // consumed audio payloads retained through acknowledgement/horizon
	// An exact export can prove a prepared handler did not cross admission.
	NeverLaunched bool
}
type localTask struct {
	intent        taskIntent
	request       []byte
	results       []agent.TaskResult
	audio         []agent.TaskResult
	launched      bool
	prepared      bool
	interrupted   bool
	cancel        context.CancelFunc
	attempt       uint64
	mailboxBytes  int
	terminalBytes int
	lastEmission  time.Time // actor-owned monotonic rate clock, never read by callbacks
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

// The version and fixed field list are independent of TaskSpec's JSON schema.
func taskDigest(spec agent.TaskSpec) string {
	h := sha256.New()
	h.Write([]byte("relais/task-arguments/v1"))
	for _, field := range [][]byte{[]byte(spec.Step), []byte(spec.Kind), []byte(strconv.FormatUint(uint64(spec.Turn), 10)), []byte(spec.Class), []byte(spec.Resume), spec.Request, []byte(strconv.FormatInt(int64(spec.Timeout), 10))} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		h.Write(size[:])
		h.Write(field)
	}
	return "v1:" + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
func taskDigestMatches(spec agent.TaskSpec, digest string) bool {
	if strings.HasPrefix(digest, "v1:") {
		return taskDigest(spec) == digest
	}
	// Read checkpoints written by the initial experimental slice. Freeze that
	// schema as well, and accept both historical encodings of an empty request.
	legacySpec := struct {
		Step    string
		Kind    string
		Turn    agent.Turn
		Class   agent.TaskClass
		Resume  agent.ResumePolicy
		Request []byte
		Timeout time.Duration
	}{spec.Step, spec.Kind, spec.Turn, spec.Class, spec.Resume, spec.Request, spec.Timeout}
	for i := 0; i < 2; i++ {
		data, _ := json.Marshal(legacySpec)
		sum := sha256.Sum256(data)
		if base64.RawURLEncoding.EncodeToString(sum[:]) == digest {
			return true
		}
		if len(legacySpec.Request) > 0 {
			break
		}
		if legacySpec.Request == nil {
			legacySpec.Request = []byte{}
		} else {
			legacySpec.Request = nil
		}
	}
	return false
}
func taskDetail(detail string) string {
	detail = strings.ToValidUTF8(detail, "�")
	if len(detail) > taskDetailBytes {
		end := taskDetailBytes
		for end > 0 && !utf8.RuneStart(detail[end]) {
			end--
		}
		detail = detail[:end]
	}
	return detail
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
	return s.taskBudgetWithRefusals(*s.agent.latest.V2, tasks, appBytes, s.agent.latest.V2.Capacity)
}
func (s *session) taskBudgetWithRefusals(basis v2Envelope, tasks []*localTask, appBytes int, refusals []taskRefusal, projection ...playbackState) error {
	s.mu.Lock()
	queueBytes := audioRefsBytes(s.agent.v2.playback.queue)
	if len(projection) > 0 {
		queueBytes = projection[0].referenceBytes
	}
	s.mu.Unlock()
	n := int64(appBytes + queueBytes)
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
	if err := s.taskMetadataBudget(basis, intents, refusals, false, projection...); err != nil {
		return err
	}
	if n > int64(s.agent.v2.registration.Bounds.MaxMailboxBytes) {
		return agent.ErrCapacity
	}
	return s.reserveMoveBytes(n)
}
func (s *session) taskMetadataBudget(basis v2Envelope, intents []taskIntent, refusals []taskRefusal, admission bool, projection ...playbackState) error {
	return s.taskMetadataBudgetMode(basis, intents, refusals, admission, false, projection...)
}
func (s *session) taskMetadataBudgetMode(basis v2Envelope, intents []taskIntent, refusals []taskRefusal, admission, withoutControl bool, projection ...playbackState) error {
	envelope := basis
	if withoutControl {
		envelope.Controls = nil
		envelope.ControlHigh = 0
		envelope.ControlFloor = 0
	}
	s.mu.Lock()
	envelope.Playback = s.playbackSnapshotLocked()
	if len(projection) > 0 {
		envelope.Playback = projection[0]
	}
	s.mu.Unlock()
	envelope.Tasks = intents
	envelope.Capacity = refusals
	_, err := s.checkpoint.admit(checkpointAdmission{host: &checkpointHostPreflight{envelope: envelope, taskHeadroom: admission}})
	return err
}
func taskMetadataGuard(envelope v2Envelope) v2Envelope {
	intents, refusals := envelope.Tasks, envelope.Capacity
	guard := slices.Clone(refusals)
	for i := len(guard); i < len(intents); i++ {
		guard = append(guard, taskRefusal{ID: intents[i].ID, Turn: intents[i].Turn})
	}
	envelope.Capacity = guard
	// A launched task may finish before the next admission. Reserve its
	// terminal fields and consumed cursor from the real intent now, including
	// while compact refusals occupy the rest of the metadata area.
	envelope.Tasks = slices.Clone(intents)
	for i, intent := range envelope.Tasks {
		if !intent.Final {
			intent.Phase = taskStarted
			intent.Final = true
			intent.Terminal = agent.OutcomeUnknown
			intent.Published++
		}
		intent.Cursor = intent.Published
		envelope.Tasks[i] = intent
	}
	return envelope
}
func (s *session) taskAdmissionBudget(basis v2Envelope, tasks []*localTask, appBytes int, refusals []taskRefusal, projection ...playbackState) error {
	intents := make([]taskIntent, len(tasks))
	for i, t := range tasks {
		intents[i] = t.intent
	}
	if err := s.taskMetadataBudget(basis, intents, refusals, true, projection...); err != nil {
		return err
	}
	return s.taskBudgetWithRefusals(basis, tasks, appBytes, refusals, projection...)
}

func (s *session) publishTaskIntents(envelope *v2Envelope) {
	h := s.agent.v2
	envelope.Tasks = nil
	if len(h.tasks) > 0 {
		envelope.Tasks = make([]taskIntent, len(h.tasks))
	}
	for i, t := range h.tasks {
		envelope.Tasks[i] = cloneTaskIntent(t.intent)
	}
	if len(envelope.Controls) > 0 {
		s.compactControlMetadata(envelope, min(h.registration.Bounds.MaxPendingBytes, v2HostMetadataBytes))
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
func (s *session) terminalTask(t *localTask, outcome agent.Outcome, detail string) {
	detail = taskDetail(detail)
	t.intent.Published++
	t.results = append(t.results, agent.TaskResult{TaskID: t.intent.ID, Sequence: t.intent.Published, Turn: t.intent.Turn, Outcome: outcome, Final: true, Detail: detail})
	t.mailboxBytes += taskResultBytes(t.results[len(t.results)-1])
	t.intent.Terminal = outcome
	t.intent.Final = true
	s.retireTaskRequest(t)
	if t.intent.Phase != taskCanceled {
		t.intent.Phase = taskTerminal
	}
	t.prepared = false
	t.launched = false
}
func (s *session) retireTaskRequest(t *localTask) {
	if t.intent.Final && (t.intent.Class == agent.ReadOnly || t.intent.Cursor == t.intent.Published) {
		t.request = nil
		t.intent.Request = nil
	}
}
func (s *session) taskStateChanged() {
	before := s.agent.latest.V2.Tasks
	s.publishTaskIntents(s.agent.latest.V2)
	if taskIntentsEqual(before, s.agent.latest.V2.Tasks) {
		return
	}
	s.agent.latest.Revision++
	s.publishAgent()
	s.checkpoint.request()
}
func (s *session) launchTasks() {
	h := s.agent.v2
	s.mu.Lock()
	paused := s.agent.paused || s.worker.paused.Load() || s.fenced.Load() || s.ctx.Err() != nil
	durable := s.checkpoint.progress(checkpointProgressRequest{tasks: true}).tasks
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
			if t.prepared {
				active--
			}
			s.terminalTask(t, agent.Timeout, "")
			changed = true
			continue
		}
		if !t.prepared && active >= h.registration.Bounds.MaxActiveTasks {
			continue
		}
		// Serialize the entire launch decision with worker-local quiescence,
		// including side-effect preparation. Never checkpoint a new started
		// intent after the launch fence.
		s.worker.taskDrain.mu.Lock()
		if s.worker.taskDrain.quiescing {
			s.worker.taskDrain.mu.Unlock()
			break
		}
		if t.intent.Class == agent.SideEffecting {
			if !t.prepared {
				active++
				t.prepared = true
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
				s.worker.taskDrain.mu.Unlock()
				continue
			}
		}
		if !t.prepared {
			active++
		}
		// The actor owns cancellation and attempt state. The final fence/route check
		// is under the same lock used by export and ownership loss.
		s.mu.Lock()
		if s.agent.paused || s.fenced.Load() || s.ctx.Err() != nil {
			s.mu.Unlock()
			s.worker.taskDrain.mu.Unlock()
			break
		}
		if !t.intent.Deadline.IsZero() && !s.taskClockLocked().Before(t.intent.Deadline) {
			s.mu.Unlock()
			s.worker.taskDrain.mu.Unlock()
			active--
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
		if t.intent.Class == agent.SideEffecting {
			progress := s.checkpoint.progress(checkpointProgressRequest{grant: &t.intent})
			witness := &AgentTaskLaunch{ID: t.intent.ID, Step: t.intent.Step, At: time.Now(), StoreAt: s.taskClockLocked(), Deadline: t.intent.Deadline, CheckpointAt: progress.at, Checkpointed: progress.taskGranted}
			s.sideEffectLaunches++
			s.lastTaskLaunch = witness
		}
		t.prepared = false
		t.intent.Phase = taskStarted
		attempt := t.attempt
		spec := agent.TaskSpec{Step: t.intent.Step, Kind: t.intent.Kind, Turn: t.intent.Turn, Class: t.intent.Class, Resume: t.intent.Resume, Request: t.request, Timeout: t.intent.Timeout}
		run := taskrun.New(spec, t.intent.ID, t.intent.Published, func(emitCtx context.Context, seq uint64, chunk agent.Chunk) error {
			return s.emitTask(ctx, taskEmission{id: t.intent.ID, attempt: attempt, ctx: emitCtx, sequence: seq, chunk: chunk})
		})
		handler := h.registration.Tasks[t.intent.Kind]
		s.mu.Unlock()
		// Drain policy affects only the wait count, never launch admission or
		// urgent export. Class belongs to this task, not its registered kind.
		waitForDrain := spec.Class != agent.ReadOnly || !slices.Contains(h.registration.SkipDrainWait, spec.Kind)
		if waitForDrain {
			if s.worker.taskDrain.active == 0 {
				s.worker.taskDrain.idle = make(chan struct{})
			}
			s.worker.taskDrain.active += 2
		}
		s.worker.taskDrain.mu.Unlock()
		go func(id agent.TaskID) {
			if waitForDrain {
				defer s.worker.taskDrain.finish()
			}
			done := make(chan error, 1)
			go func() {
				if waitForDrain {
					defer s.worker.taskDrain.finish()
				}
				done <- handler(ctx, run)
			}()
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

// A rate wait carries only an owned duration back to the producer. Memory
// pressure retains its short retry interval; neither wait holds actor locks.
type taskEmissionWait struct{ after time.Duration }

func (e *taskEmissionWait) Error() string { return agent.ErrBackpressure.Error() }
func (*taskEmissionWait) Unwrap() error   { return agent.ErrBackpressure }

func (s *session) emitTask(attemptCtx context.Context, e taskEmission) error {
	e.reply = make(chan error, 1)
	for {
		retryAfter := 10 * time.Millisecond
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
			var rateWait *taskEmissionWait
			if errors.As(err, &rateWait) {
				retryAfter = rateWait.after
			}
		case <-attemptCtx.Done():
			return attemptCtx.Err()
		case <-e.ctx.Done():
			return e.ctx.Err()
		}
		timer := time.NewTimer(retryAfter)
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
	if paused || t == nil || t.attempt != e.attempt || t.intent.Final || t.intent.Turn < s.agent.latest.V2.CancelFloor || e.ctx.Err() != nil {
		e.reply <- context.Canceled
		return
	}
	// The first publication is immediate. Later chunks and the terminal share
	// one interval. Backpressure waits in emitTask, outside actor/media locks.
	sinceEmission := time.Since(t.lastEmission)
	if !t.lastEmission.IsZero() && sinceEmission < 100*time.Millisecond {
		e.reply <- &taskEmissionWait{after: 100*time.Millisecond - sinceEmission}
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
		if e.chunk.Audio != nil {
			if err := s.admitTaskAudio(e.chunk.Audio); err != nil {
				e.reply <- err
				return
			}
		}
		r := agent.TaskResult{TaskID: e.id, Sequence: e.sequence, Turn: t.intent.Turn, Outcome: agent.OK, Chunk: e.chunk}
		bytes := taskResultBytes(r)
		if bytes+t.terminalBytes+2*len(t.request)+s.agent.latest.V2.MoveOnlyLength > h.registration.Bounds.MaxMailboxBytes {
			e.reply <- agent.ErrCapacity
			return
		}
		previousIntent := t.intent
		t.intent.Published = e.sequence
		t.mailboxBytes += bytes
		t.results = append(t.results, r)
		err := s.taskBudget(h.tasks, s.agent.latest.V2.MoveOnlyLength)
		if err != nil {
			t.intent = previousIntent
			t.results = t.results[:len(t.results)-1]
			t.mailboxBytes -= bytes
			if errors.Is(err, agent.ErrCapacity) || errors.Is(err, ErrMoveBudget) {
				err = agent.ErrBackpressure
			}
			e.reply <- err
			return
		}
		t.results[len(t.results)-1].Chunk = cloneChunk(e.chunk)
		s.mu.Lock()
		err = s.registerAudioLocked(t.results[len(t.results)-1])
		s.mu.Unlock()
		if err != nil {
			e.reply <- err
			return
		}
		t.intent.Published = e.sequence
	}
	t.lastEmission = time.Now()
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
		if _, err := (acceptedEvents{s}).submit(agent.Event{ID: id, Kind: agent.TaskMessage, Result: &result}, eventLive); err != nil {
			if errors.Is(err, errTaskAdmissionBackpressure) {
				continue
			}
			return false, err
		}
		// submit commits the cursor and removes this exact mailbox entry
		// atomically with application state, before publishing the pair.
		return true, nil
	}
	if pending := s.agent.latest.V2.Capacity; len(pending) > 0 {
		result := pending[0].result()
		_, err := (acceptedEvents{s}).submit(agent.Event{ID: agent.EventID("task/" + string(result.TaskID) + "/1"), Kind: agent.TaskMessage, Result: &result}, eventLive)
		if errors.Is(err, errTaskAdmissionBackpressure) {
			return false, nil
		}
		// Yield to emissions/export/ticker before another refusal transaction.
		return false, err
	}
	return false, nil
}

// Read published intent and acknowledged state under the media lock; the
// dispatcher never reads actor-owned prepared/attempt fields.
func (s *session) taskCheckpointPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fenced.Load() || s.ctx.Err() != nil || s.state.Agent.V2 == nil {
		return false
	}
	for _, t := range s.state.Agent.V2.Tasks {
		if t.Class != agent.SideEffecting || t.Phase != taskStarted || t.Final {
			continue
		}
		if !s.checkpoint.progress(checkpointProgressRequest{grant: &t}).taskGranted {
			return true
		}
	}
	return false
}
func (s *session) taskCheckpointLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.agent.v2.force:
		}
		backoff := 100 * time.Millisecond
		for s.taskCheckpointPending() {
			s.mu.Lock()
			paused := s.agent.paused
			s.mu.Unlock()
			if !paused && !s.worker.paused.Load() {
				_ = s.checkpoint.persist(s.ctx, checkpointIntent)
				if !s.taskCheckpointPending() {
					break
				}
			}
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-s.ctx.Done():
				timer.Stop()
				return
			}
			backoff = min(2*backoff, time.Second)
		}
	}
}

func taskIntentsEqual(a, b []taskIntent) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// validateTaskMoves is shared by source preflight and target restoration.
// It reads owned payloads without copying them or changing actor state.
func (s *session) validateTaskMoves(intents []taskIntent, move *movePayload) error {
	seen := map[agent.TaskID]bool{}
	for _, r := range s.agent.latest.V2.Capacity {
		if r.ID == "" {
			return fmt.Errorf("%w: invalid capacity intent", agent.ErrIncompatibleAgent)
		}
		seen[r.ID] = true
	}
	if move != nil && len(move.Tasks) != len(intents) {
		return fmt.Errorf("%w: unexpected exact task mailbox", agent.ErrIncompatibleAgent)
	}
	for _, intent := range intents {
		if seen[intent.ID] || intent.ID == "" || s.agent.v2.registration.Tasks[intent.Kind] == nil || intent.Cursor > intent.Published ||
			(intent.Class != agent.ReadOnly && intent.Class != agent.SideEffecting) ||
			(intent.Class == agent.SideEffecting && intent.Resume != agent.Retry && intent.Resume != agent.Abandon) ||
			(intent.Phase != taskNever && intent.Phase != taskStarted && intent.Phase != taskTerminal && intent.Phase != taskCanceled) ||
			intent.RequestLength < 0 || (!intent.Final && intent.Class == agent.SideEffecting && len(intent.Request) != intent.RequestLength) ||
			(intent.Final && !taskrun.ValidOutcome(intent.Terminal)) || (intent.Class == agent.ReadOnly && len(intent.Request) > 0) {
			return fmt.Errorf("%w: invalid restored task intent", agent.ErrIncompatibleAgent)
		}
		seen[intent.ID] = true
		request := intent.Request
		if move != nil {
			exact := findTaskMove(move, intent.ID)
			if exact == nil {
				return fmt.Errorf("%w: missing or duplicate task mailbox", agent.ErrIncompatibleAgent)
			}
			request = exact.Request
			audioSeq := uint64(0)
			for _, r := range exact.Audio {
				if r.TaskID != intent.ID || r.Turn != intent.Turn || r.Sequence <= audioSeq || r.Sequence > intent.Cursor || r.Chunk.Audio == nil || r.Final {
					return fmt.Errorf("%w: invalid retained audio", agent.ErrIncompatibleAgent)
				}
				if _, err := validateOpus(r.Chunk.Audio); err != nil {
					return err
				}
				audioSeq = r.Sequence
			}
			seq := intent.Cursor
			for i, r := range exact.Results {
				if r.TaskID != intent.ID || r.Sequence <= seq || r.Sequence > intent.Published || !taskrun.ValidOutcome(r.Outcome) || r.Turn != intent.Turn || (r.Final && (i != len(exact.Results)-1 || !intent.Final || r.Outcome != intent.Terminal)) {
					return fmt.Errorf("%w: invalid exact task result", agent.ErrIncompatibleAgent)
				}
				if r.Chunk.Audio != nil {
					if _, err := validateOpus(r.Chunk.Audio); err != nil {
						return err
					}
				}
				seq = r.Sequence
			}
			if (intent.Final && intent.Cursor < intent.Published && (len(exact.Results) == 0 || !exact.Results[len(exact.Results)-1].Final)) || seq != intent.Published || (!intent.Final && len(request) != intent.RequestLength) {
				return fmt.Errorf("%w: incomplete exact task mailbox", agent.ErrIncompatibleAgent)
			}
		}
		if !intent.Final && (move != nil || intent.Class == agent.SideEffecting || intent.RequestLength == 0) {
			spec := agent.TaskSpec{Step: intent.Step, Kind: intent.Kind, Turn: intent.Turn, Class: intent.Class, Resume: intent.Resume, Request: request, Timeout: intent.Timeout}
			if !taskDigestMatches(spec, intent.Digest) {
				return fmt.Errorf("%w: restored task arguments do not match intent", agent.ErrIncompatibleAgent)
			}
		}
	}
	return nil
}
func findTaskMove(move *movePayload, id agent.TaskID) *taskMove {
	var found *taskMove
	for i := range move.Tasks {
		if move.Tasks[i].ID == id {
			if found != nil {
				return nil
			}
			found = &move.Tasks[i]
		}
	}
	return found
}
func (s *session) restoreTasks(move *movePayload) error {
	h := s.agent.v2
	s.agent.latest.V2.Capacity = uniqueTaskRefusals(s.agent.latest.V2.Capacity)
	if err := s.validateTaskMoves(s.agent.latest.V2.Tasks, move); err != nil {
		return err
	}
	for _, intent := range s.agent.latest.V2.Tasks {
		t := &localTask{intent: cloneTaskIntent(intent), request: slices.Clone(intent.Request), terminalBytes: taskTerminalBytes(intent)}
		// A prior publication cannot grant a fresh immediate chunk at adoption.
		// One conservative full local interval avoids comparing worker clocks.
		if intent.Published > 0 {
			t.lastEmission = time.Now()
		}
		var exact *taskMove
		if move != nil {
			exact = findTaskMove(move, intent.ID)
			t.request = slices.Clone(exact.Request)
			t.results = slices.Clone(exact.Results)
			t.audio = slices.Clone(exact.Audio)
			for i, r := range t.audio {
				t.mailboxBytes += taskRetainedAudioBytes(r)
				t.audio[i].Chunk = cloneChunk(r.Chunk)
			}
			for i, r := range t.results {
				r.Detail = taskDetail(r.Detail)
				t.results[i].Detail = r.Detail
				t.mailboxBytes += taskResultBytes(r)
				t.results[i].Chunk = cloneChunk(r.Chunk)
			}
			if exact.NeverLaunched && !intent.Final {
				t.intent.Phase = taskNever
			}
		}
		if intent.Final {
			s.retireTaskRequest(t)
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
			if intent.Final && intent.Published == intent.Cursor+1 && taskrun.ValidOutcome(intent.Terminal) &&
				(intent.Class != agent.SideEffecting || intent.Resume != agent.Retry || intent.Terminal == agent.Canceled || intent.Terminal == agent.Timeout) {
				s.terminalTask(t, intent.Terminal, "")
				h.tasks = append(h.tasks, t)
				continue
			}
			if (intent.Final && intent.Class == agent.SideEffecting && len(t.request) != intent.RequestLength) || (intent.Class == agent.ReadOnly && (intent.RequestLength > 0 || (intent.Published > 0 && intent.Resume != agent.Retry))) || (intent.Phase != taskNever && intent.Resume == agent.Abandon) {
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
	appBytes := 0
	if move != nil {
		appBytes = s.agent.latest.V2.MoveOnlyLength
	}
	return s.taskBudget(h.tasks, appBytes)
}
func (s *session) taskMoveViews() []taskMove {
	out := make([]taskMove, len(s.agent.v2.tasks))
	for i, t := range s.agent.v2.tasks {
		out[i] = taskMove{ID: t.intent.ID, Request: t.request, Results: t.results, Audio: t.audio, NeverLaunched: !t.launched}
	}
	return out
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
		out[i] = taskMove{ID: t.intent.ID, Request: slices.Clone(t.request), NeverLaunched: !t.launched, Results: slices.Clone(t.results), Audio: slices.Clone(t.audio)}
		for j, r := range out[i].Audio {
			out[i].Audio[j].Chunk = cloneChunk(r.Chunk)
		}
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
	s.worker.running.Add(1)
	go func() { defer s.worker.running.Done(); s.taskCheckpointLoop() }()
	defer s.stopTaskAttempts()
	s.worker.running.Add(1)
	go func() { defer s.worker.running.Done(); s.playbackLoop() }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	ready := make(chan struct{})
	close(ready)
	for {
		var again <-chan struct{}
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
			if !s.agent.latest.V2.Started {
				if _, err := (acceptedEvents{s}).submit(agent.Event{ID: agent.EventID("started/" + s.id), Kind: agent.Started}, eventLive); err != nil && !errors.Is(err, errTaskAdmissionBackpressure) {
					s.agentFailed(err)
					return
				}
			}
			if err := s.deliverClockEvent(); err != nil && !errors.Is(err, errTaskAdmissionBackpressure) {
				s.agentFailed(err)
				return
			}
			if h.pendingInput == nil && s.agent.latest.V2.Started {
				select {
				case in := <-s.agent.queue:
					h.pendingInput = &in
				default:
				}
			}
			if !paused && h.pendingInput != nil {
				if err := s.processAudio(*h.pendingInput); err == nil {
					h.pendingInput = nil
				} else if !errors.Is(err, errTaskAdmissionBackpressure) {
					s.agentFailed(err)
					return
				}
			}
			s.pruneTasks()
			s.launchTasks()
			if delivered, err := s.deliverTaskResult(); err != nil {
				s.agentFailed(err)
				return
			} else if delivered {
				again = ready
			}
		}
		input := s.agent.queue
		if h.pendingInput != nil || !s.agent.latest.V2.Started {
			input = nil
		}
		select {
		case <-s.ctx.Done():
			return
		case request := <-s.agent.flush:
			if !s.finishAgentFlush(request) {
				return
			}
		case request := <-h.controls:
			receipt, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &request.message}, eventLive)
			request.done <- controlReply{receipt, err}
			if err != nil && !controlRefusal(err) {
				s.agentFailed(err)
				return
			}
		case request := <-h.prototypeOutput:
			err := s.acceptPrototypeOutput(request)
			request.done <- err
			// Prototype effects retain fail-closed Save/Restore and callback deadlines.
			if errors.Is(err, errPrototypeOutputCallback) || errors.Is(err, agent.ErrIncompatibleAgent) || errors.Is(err, legacy.ErrSave) || errors.Is(err, legacy.ErrRestore) || errors.Is(err, context.DeadlineExceeded) {
				s.agentFailed(err)
				return
			}
		case e := <-h.emissions:
			s.acceptEmission(e)
		case in := <-input:
			if err := s.processAudio(in); err != nil {
				if errors.Is(err, errTaskAdmissionBackpressure) {
					h.pendingInput = &in
				} else {
					s.agentFailed(err)
					return
				}
			}
		case <-ticker.C:
		case <-again:
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
			n += base64.StdEncoding.EncodedLen(len(p)) + 40 + len(r.TaskID) + 32 // owned packet slice and cached duration, sized per packet
		}
	}
	b, _ := json.Marshal(r)
	return len(b) + n
}

// Internal task events cannot be replayed past their committed cursor; Started
// cannot replay past its committed flag. Input-caused receipts additionally wait
// for the receive replay window to pass the cause and the consumed-input
// floor to include it. Reception advances even while application input waits.
// An older retained export carries its own receipts. No wall-clock TTL is used.
func (s *session) pruneTasks() {
	h := s.agent.v2
	s.mu.Lock()
	durable := s.checkpoint.progress(checkpointProgressRequest{tasks: true}).tasks
	progress := s.state.Agent.Progress
	received := maps.Clone(s.state.SRTP.Inbound)
	liveAudio := make(map[audioKey]struct{}, len(h.playback.queue))
	liveTasks := make(map[agent.TaskID]struct{})
	for _, ref := range h.playback.queue {
		liveAudio[audioKey{ref.Source.TaskID, ref.Source.Sequence}] = struct{}{}
		liveTasks[ref.Source.TaskID] = struct{}{}
	}
	s.mu.Unlock()
	next := h.tasks[:0]
	payloadChanged := false
	for _, t := range h.tasks {
		// A streaming producer must regain mailbox space before its terminal.
		// Keep consumed audio until the checkpoint/horizon and last reference.
		for _, d := range durable {
			if d.ID != t.intent.ID {
				continue
			}
			horizon := true
			if cause := t.intent.InputCause; cause != nil {
				high, known := received[cause.SSRC]
				horizon = known && progress.SSRC == cause.SSRC && progress.Index >= cause.Index && high >= cause.Index && high-cause.Index >= replayWindow
			}
			if !horizon {
				break
			}
			s.mu.Lock()
			kept := t.audio[:0]
			for _, r := range t.audio {
				_, live := liveAudio[audioKey{r.TaskID, r.Sequence}]
				if r.Sequence <= d.Cursor && !live {
					t.mailboxBytes -= taskRetainedAudioBytes(r)
					delete(h.playback.payloads, audioKey{r.TaskID, r.Sequence})
					payloadChanged = true
				} else {
					kept = append(kept, r)
				}
			}
			clear(t.audio[len(kept):])
			t.audio = kept
			s.mu.Unlock()
			break
		}

		ack := false
		if t.intent.Final && t.intent.Cursor == t.intent.Published && len(t.results) == 0 {
			for _, d := range durable {
				if d.ID == t.intent.ID && d.Final && d.Cursor == t.intent.Cursor {
					ack = true
					break
				}
			}
			if cause := t.intent.InputCause; cause != nil {
				high, known := received[cause.SSRC]
				ack = ack && progress.SSRC == cause.SSRC && progress.Index >= cause.Index && known && high >= cause.Index && high-cause.Index >= replayWindow
			}
		}
		s.mu.Lock()
		if _, live := liveTasks[t.intent.ID]; live {
			ack = false
		}
		if ack {
			for key := range h.playback.payloads {
				if key.task == t.intent.ID {
					delete(h.playback.payloads, key)
				}
			}
		}
		s.mu.Unlock()
		if !ack {
			next = append(next, t)
		}
	}
	if len(next) != len(h.tasks) || payloadChanged {
		h.tasks = next
		_ = s.taskBudget(next, s.agent.latest.V2.MoveOnlyLength)
		s.taskStateChanged()
	}
}

// Reference storage is reserved before Emit copies a chunk, then transferred
// from its result reservation to the actual playback queue on consumption.
func taskAudioRefBytes(r agent.TaskResult) int {
	if r.Chunk.Audio == nil {
		return 0
	}
	return len(r.Chunk.Audio.Packets) * (len(r.TaskID) + 32)
}
func taskRetainedAudioBytes(r agent.TaskResult) int { return taskResultBytes(r) - taskAudioRefBytes(r) }
