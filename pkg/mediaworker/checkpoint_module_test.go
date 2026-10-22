package mediaworker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"

	"github.com/stretchr/testify/require"
)

// Fixtures observe only the production projection and owned snapshot encoding.
func checkpointObservation(s *session) checkpointProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpoint.progress(checkpointProgressRequest{})
}
func checkpointKnown(s *session) bool {
	return s.checkpoint.progress(checkpointProgressRequest{readiness: true}).known
}
func checkpointQuota(t *testing.T, s *session) uint64 {
	t.Helper()
	data, err := s.checkpoint.capture(checkpointLive, nil)
	require.NoError(t, err)
	snap, err := decodeSnapshot(data)
	require.NoError(t, err)
	n, err := snap.State.Agent.V2.Quota.balance()
	require.NoError(t, err)
	return n
}
func restoreCheckpointQuota(t *testing.T, s *session, tokens float64, at time.Time) {
	t.Helper()
	// Restore in its observed clock domain, then resume the scenario's clock.
	// This also permits a deliberate backward step without accepting a future
	// timestamp through the production restore validation.
	s.mu.Lock()
	old, oldAt := s.storeClock, s.storeClockAt
	s.storeClock, s.storeClockAt = at, time.Now()
	s.mu.Unlock()
	require.NoError(t, s.checkpoint.restore(checkpointRestore{quota: &snapshotQuota{Version: 1, Balance: quotaBalance(uint64(tokens)), At: at}}))
	s.mu.Lock()
	s.storeClock, s.storeClockAt = old, oldAt
	s.mu.Unlock()
}
func ptrControlAck(n uint64) *uint64 { return &n }

func TestCheckpointHostPreflightDoesNotReserveOrPublish(t *testing.T) {
	store := &fixedCheckpointClockStore{Memory: sessionstore.NewMemory(), now: time.Now()}
	s := componentWriterSession(t, store, true)
	s.agent = &agentHost{latest: cloneAgentState(s.state.Agent), v2: &v2Host{registration: agenthost.Registration{Bounds: effectiveAgentBounds(agent.Limits{}, s.snapshotLimits(), true)}}}
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	debit := s.worker.metrics.snapshotDebitBytes.Load()
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	hot := s.worker.metrics.snapshotDebitBytes.Load() - debit
	// A real required hot write can spend exactly this amount. A history hold
	// would preserve another maximum hot write and refuse it.
	restoreCheckpointQuota(t, s, float64(hot), store.now.Add(time.Hour))
	before := cloneAgentState(s.state.Agent)
	balance := checkpointQuota(t, s)
	envelope := *cloneAgentState(before).V2
	original := *cloneAgentState(before).V2
	for _, headroom := range []bool{false, true} {
		_, err := s.checkpoint.admit(checkpointAdmission{host: &checkpointHostPreflight{envelope: envelope, taskHeadroom: headroom}})
		require.NoError(t, err)
	}
	oversize := envelope
	for range 32 {
		oversize.Timers = append(oversize.Timers, hostTimer{ID: strings.Repeat("timer", 20), Generation: 1})
	}
	_, refused := s.checkpoint.admit(checkpointAdmission{host: &checkpointHostPreflight{envelope: oversize, effects: &agent.Effects{}}})
	require.ErrorIs(t, refused, agent.ErrCapacity)
	require.Equal(t, original, envelope, "preflight cannot publish quota or compaction into the caller's envelope")
	require.Equal(t, before, s.state.Agent)
	require.Equal(t, balance, checkpointQuota(t, s), "preflight cannot debit quota")
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired), "preflight cannot install a history reservation")
	data, _, err := ReadRecoverySnapshot(context.Background(), store, s.id)
	require.NoError(t, err)
	saved, err := decodeSnapshot(data)
	require.NoError(t, err)
	remaining, err := saved.State.Agent.V2.Quota.balance()
	require.NoError(t, err)
	require.Zero(t, remaining)
}

func TestCheckpointHostPreflightCompactsOnlyAcknowledgedReceipts(t *testing.T) {
	_, s := v2UnitSession(t, &v2UnitAgent{state: agent.State{Version: 1}})
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	envelope := *cloneAgentState(s.state.Agent).V2
	_, err := s.checkpoint.admit(checkpointAdmission{host: &checkpointHostPreflight{envelope: envelope}})
	require.NoError(t, err)
	envelope.ControlHigh = 1
	envelope.Controls = []controlRecord{{ID: strings.Repeat("a", 64), Sequence: 1, Digest: strings.Repeat("b", 64)}}
	compact := envelope
	compact.Controls = nil
	compact.ControlFloor = 1
	size, err := s.checkpoint.admit(checkpointAdmission{host: &checkpointHostPreflight{envelope: compact}})
	require.NoError(t, err)
	s.agent.v2.registration.Bounds.MaxPendingBytes = size
	_, err = s.checkpoint.admit(checkpointAdmission{host: &checkpointHostPreflight{envelope: envelope}})
	require.ErrorIs(t, err, agent.ErrCapacity, "unacknowledged receipt cannot be compacted")
	_, err = s.checkpoint.admit(checkpointAdmission{host: &checkpointHostPreflight{envelope: envelope, effects: &agent.Effects{}}})
	require.ErrorIs(t, err, errControlReceiptBackpressure, "receipt pressure stays retryable on the effects path")
	acknowledged := cloneAgentState(s.state.Agent)
	acknowledged.V2.ControlHigh = envelope.ControlHigh
	acknowledged.V2.Controls = envelope.Controls
	require.NoError(t, s.checkpoint.restore(checkpointRestore{pair: &acknowledged}))
	_, err = s.checkpoint.admit(checkpointAdmission{host: &checkpointHostPreflight{envelope: envelope}})
	require.NoError(t, err)
	require.Len(t, envelope.Controls, 1, "compaction is a local preflight, not publication")
	require.Zero(t, s.state.Agent.V2.ControlHigh)
}

func TestCheckpointHostPreflightReservesTaskMetadataHeadroom(t *testing.T) {
	_, s := v2UnitSession(t, &v2UnitAgent{state: agent.State{Version: 1}})
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	envelope := *cloneAgentState(s.state.Agent).V2
	envelope.Tasks = []taskIntent{{ID: "task", Kind: "read/v1", Class: agent.ReadOnly, Phase: taskNever, RequestLength: 128}}
	plain, err := s.checkpoint.admit(checkpointAdmission{host: &checkpointHostPreflight{envelope: envelope}})
	require.NoError(t, err)
	guarded, err := s.checkpoint.admit(checkpointAdmission{host: &checkpointHostPreflight{envelope: envelope, taskHeadroom: true}})
	require.NoError(t, err)
	require.Greater(t, guarded, plain)
	s.agent.v2.registration.Bounds.MaxPendingBytes = plain
	_, err = s.checkpoint.admit(checkpointAdmission{host: &checkpointHostPreflight{envelope: envelope}})
	require.NoError(t, err)
	_, err = s.checkpoint.admit(checkpointAdmission{host: &checkpointHostPreflight{envelope: envelope, taskHeadroom: true}})
	require.ErrorIs(t, err, agent.ErrCapacity)
	require.Equal(t, taskNever, envelope.Tasks[0].Phase)
}

func TestCheckpointHostPreflightClassifiesAudioGrowthAndPermanentOversize(t *testing.T) {
	s, _, result := audioUnitSession(t, 1)
	envelope := *cloneAgentState(s.state.Agent).V2
	baseline, err := s.checkpoint.admit(checkpointAdmission{host: &checkpointHostPreflight{envelope: envelope}})
	require.NoError(t, err)
	effects := agent.Effects{Audio: []agent.Audio{{Turn: result.Turn, Source: agent.AudioRef{TaskID: result.TaskID, Sequence: result.Sequence}}}}
	envelope.Playback = playbackState{Pending: 1, Length: len(result.Chunk.Audio.Packets[0]), Duration: 20 * time.Millisecond,
		Window: []audioSpan{{Task: result.TaskID, Sequence: result.Sequence, Turn: result.Turn, Count: 1}}}
	s.agent.v2.registration.Bounds.MaxPendingBytes = baseline + 1
	_, err = s.checkpoint.admit(checkpointAdmission{host: &checkpointHostPreflight{envelope: envelope, effects: &effects}})
	require.ErrorIs(t, err, errTaskAdmissionBackpressure)
	require.NotErrorIs(t, err, agent.ErrCapacity)
	require.Len(t, effects.Audio, 1)
	for range 32 {
		envelope.Timers = append(envelope.Timers, hostTimer{ID: strings.Repeat("timer", 20), Generation: 1})
	}
	_, err = s.checkpoint.admit(checkpointAdmission{host: &checkpointHostPreflight{envelope: envelope, effects: &effects}})
	require.ErrorIs(t, err, agent.ErrCapacity)
	require.NotErrorIs(t, err, errTaskAdmissionBackpressure)
	require.Empty(t, s.state.Agent.V2.Timers)
}

// Publish the candidate as a caller would, then exercise the real write path.
// Replenishment is restored explicitly so size tests do not become rate tests.
func persistCheckpointCandidate(t *testing.T, s *session, candidate agentState) error {
	t.Helper()
	restoreCheckpointQuota(t, s, float64(s.snapshotLimits().BurstBytes), s.taskNow())
	s.mu.Lock()
	s.state.Agent = cloneAgentState(candidate)
	s.mu.Unlock()
	return s.checkpoint.persist(context.Background(), checkpointRequired)
}

var errCheckpointClockProbe = errors.New("checkpoint test: withhold next capture after settlement")

type checkpointGateStore struct {
	*sessionstore.Memory
	now     time.Time
	gate    atomic.Bool
	entered chan struct{}
	release chan struct{}
	fail    atomic.Bool
}

func (s *checkpointGateStore) Clock(context.Context, string) (time.Time, error) { return s.now, nil }
func (s *checkpointGateStore) CommitComponents(ctx context.Context, l sessionstore.Lease, r sessionstore.ComponentCommit) (sessionstore.ComponentTuple, error) {
	if s.gate.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return sessionstore.ComponentTuple{}, ctx.Err()
		}
	}
	if s.fail.Load() {
		return sessionstore.ComponentTuple{}, &sessionstore.TransientError{Op: "withheld commit", Err: context.DeadlineExceeded}
	}
	return s.Memory.CommitComponents(ctx, l, r)
}
func checkpointModuleSession(t *testing.T, store sessionstore.Store) *session {
	t.Helper()
	s := componentWriterSession(t, store, true)
	s.agent = &agentHost{latest: cloneAgentState(s.state.Agent), v2: &v2Host{registration: agenthost.Registration{Bounds: effectiveAgentBounds(agent.Limits{}, s.snapshotLimits(), true)}}}
	return s
}

func TestCheckpointInflightWriteReservesAdmissionAndExactExport(t *testing.T) {
	for _, export := range []bool{false, true} {
		t.Run(fmt.Sprint(export), func(t *testing.T) {
			store := &checkpointGateStore{Memory: sessionstore.NewMemory(), now: time.Now(), entered: make(chan struct{}), release: make(chan struct{})}
			s := checkpointModuleSession(t, store)
			require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
			restoreCheckpointQuota(t, s, 20000, store.now.Add(time.Hour))
			store.gate.Store(true)
			done := make(chan error, 1)
			go func() { done <- s.checkpoint.persist(context.Background(), checkpointRequired) }()
			select {
			case <-store.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("writer did not reach store")
			}
			candidate := cloneAgentState(s.state.Agent)
			candidate.History = []byte("changed")
			_, err := s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
			require.ErrorIs(t, err, agent.ErrCapacity, "an older write can still debit a maximum transaction")
			if export {
				data, err := s.checkpoint.capture(checkpointExact, &movePayload{})
				require.NoError(t, err)
				snap, err := decodeSnapshot(data)
				require.NoError(t, err)
				q, err := snap.State.Agent.V2.Quota.balance()
				require.NoError(t, err)
				require.Zero(t, q, "exact export reserves the maximum transaction, not just hot")
			}
			close(store.release)
			require.NoError(t, <-done)
			if !export {
				_, err = s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
				require.NoError(t, err, "resolved write no longer requires an in-flight reservation")
			}
		})
	}
}

func TestCheckpointHistoryAdmissionProtectsFutureHotGrowth(t *testing.T) {
	store := &fixedCheckpointClockStore{Memory: sessionstore.NewMemory(), now: time.Now()}
	s := checkpointModuleSession(t, store)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	tuple, err := store.ReadComponents(context.Background(), s.id)
	require.NoError(t, err)
	candidate := cloneAgentState(s.state.Agent)
	candidate.History = bytes.Repeat([]byte{'h'}, 8192)
	transaction := s.snapshotLimits().MaxBytes + len(tuple.Components.Setup) + componentCipherOverhead + len(candidate.History) + 6 + componentCipherOverhead
	required := transaction + s.snapshotLimits().MaxBytes
	restoreCheckpointQuota(t, s, float64(required-1), store.now.Add(time.Hour))
	_, err = s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
	require.ErrorIs(t, err, agent.ErrCapacity, "admission reserves future maximum hot growth, not today's smaller capture")
	restoreCheckpointQuota(t, s, float64(required), store.now.Add(time.Hour))
	_, err = s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
	require.NoError(t, err)
	require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointRequired), errCheckpointSkipped, "an older pair cannot spend the promised history transaction")
	s.mu.Lock()
	s.state.Agent = cloneAgentState(candidate)
	s.agent.latest = cloneAgentState(candidate)
	s.mu.Unlock()
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired), "matching acknowledgement releases the hold and preserves required transport work")
	raw, _, err := ReadRecoverySnapshot(context.Background(), store, s.id)
	require.NoError(t, err)
	saved, err := decodeSnapshot(raw)
	require.NoError(t, err)
	require.Equal(t, candidate.History, saved.State.Agent.History)
}

func TestCheckpointRollbackRestoresMaximumHistoryReservation(t *testing.T) {
	store := &fixedCheckpointClockStore{Memory: sessionstore.NewMemory(), now: time.Now()}
	s := checkpointModuleSession(t, store)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	before := cloneAgentState(s.state.Agent)
	candidate := cloneAgentState(before)
	candidate.History = []byte("tentative history")
	_, err := s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: before})
	require.NoError(t, err)
	s.checkpoint.rollback(before)
	maximum := splitComponentBudgets(s.snapshotLimits().MaxBytes, 8192).Total
	restoreCheckpointQuota(t, s, float64(maximum+s.snapshotLimits().MaxBytes), store.now.Add(time.Hour))
	s.mu.Lock()
	s.state.Agent = cloneAgentState(candidate)
	s.mu.Unlock()
	require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointRequired), errCheckpointSkipped, "tentative pair cannot spend the rollback reservation")
	s.mu.Lock()
	s.state.Agent = cloneAgentState(before)
	s.mu.Unlock()
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	raw, _, err := ReadRecoverySnapshot(context.Background(), store, s.id)
	require.NoError(t, err)
	saved, err := decodeSnapshot(raw)
	require.NoError(t, err)
	require.Equal(t, before.History, saved.State.Agent.History)
}

func TestCheckpointQuotaReadsDoNotRebaseAfterClockRollback(t *testing.T) {
	store := &checkpointGateStore{Memory: sessionstore.NewMemory(), now: time.Now()}
	s := checkpointModuleSession(t, store)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	restoreCheckpointQuota(t, s, 20000, store.now)
	store.now = store.now.Add(-2 * time.Second)
	store.fail.Store(true)
	before := s.worker.metrics.snapshotDebitBytes.Load()
	require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointRequired), sessionstore.ErrTransient)
	require.Equal(t, before, s.worker.metrics.snapshotDebitBytes.Load())
	first := checkpointQuota(t, s)
	candidate := cloneAgentState(s.state.Agent)
	for range 3 {
		_, err := s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
		require.NoError(t, err)
		require.Equal(t, first, checkpointQuota(t, s), "reads cannot refill from a rebased rollback clock")
	}
	store.fail.Store(false)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	raw, _, err := ReadRecoverySnapshot(context.Background(), store, s.id)
	require.NoError(t, err)
	saved, err := decodeSnapshot(raw)
	require.NoError(t, err)
	q, err := saved.State.Agent.V2.Quota.balance()
	require.NoError(t, err)
	require.Less(t, q, first)
	store.now = store.now.Add(100 * time.Millisecond)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	raw, _, err = ReadRecoverySnapshot(context.Background(), store, s.id)
	require.NoError(t, err)
	saved, err = decodeSnapshot(raw)
	require.NoError(t, err)
	refilled, err := saved.State.Agent.V2.Quota.balance()
	require.NoError(t, err)
	require.Greater(t, refilled, q, "successful write rebases quota and permits refill in the new store-clock domain")
}

type checkpointRequestContext struct{}
type checkpointRequestStore struct {
	sessionstore.Store
	sessionstore.ComponentStore
	events chan struct{}
}

func (s *checkpointRequestStore) Clock(ctx context.Context, id string) (time.Time, error) {
	if ctx.Value(checkpointRequestContext{}) != nil {
		s.events <- struct{}{}
		return time.Time{}, errCheckpointClockProbe
	}
	return s.Store.Clock(ctx, id)
}

// Run the production request loop; the store adapter records its attempted
// captures without acknowledging them. Explicit writes use an untagged context.
func observeCheckpointRequests(t *testing.T, s *session) <-chan struct{} {
	t.Helper()
	var base sessionstore.Store = sessionstore.NewMemory()
	relayConfig := RelayConfig{}
	if s.worker.cfg.Relay != nil {
		relayConfig = *s.worker.cfg.Relay
		base = relayConfig.Owners
	}
	store := &checkpointRequestStore{Store: base, events: make(chan struct{}, 64)}
	store.ComponentStore, _ = base.(sessionstore.ComponentStore)
	relayConfig.Owners = store
	s.worker.cfg.Relay = &relayConfig
	s.worker.cfg.SnapshotInterval = time.Hour
	loopContext := context.WithValue(s.ctx, checkpointRequestContext{}, true)
	done := make(chan struct{})
	go func() { defer close(done); s.checkpoint.run(loopContext) }()
	t.Cleanup(func() { s.cancel(); <-done })
	return store.events
}
func requireCheckpointRequest(t *testing.T, events <-chan struct{}, want bool) {
	t.Helper()
	if want {
		select {
		case <-events:
		case <-time.After(2 * time.Second):
			t.Fatal("checkpoint loop did not attempt the requested capture")
		}
	} else {
		require.Never(t, func() bool { return len(events) > 0 }, 30*time.Millisecond, time.Millisecond, "unexpected checkpoint request")
	}
}

func storedCheckpointQuota(t *testing.T, store sessionstore.Store, id string) uint64 {
	t.Helper()
	data, _, err := ReadRecoverySnapshot(context.Background(), store, id)
	require.NoError(t, err)
	snap, err := decodeSnapshot(data)
	require.NoError(t, err)
	n, err := snap.State.Agent.V2.Quota.balance()
	require.NoError(t, err)
	return n
}

func TestV2SnapshotSizeExactBoundary(t *testing.T) {
	s := checkpointModuleSession(t, sessionstore.NewMemory())
	s.componentMode = false
	s.state.Agent.History = nil
	s.agent.latest = cloneAgentState(s.state.Agent)
	candidate := cloneAgentState(s.state.Agent)
	raw, err := s.checkpoint.capture(checkpointLive, nil)
	require.NoError(t, err)
	s.worker.cfg.SnapshotLimits.MaxBytes = len(raw) + snapshotEnvelopeBytes
	_, err = s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
	require.NoError(t, err, "the actual whole-snapshot encoding fits at the exact cap")
	s.worker.cfg.SnapshotLimits.MaxBytes--
	_, err = s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
	require.ErrorIs(t, err, agent.ErrCapacity, "one byte below that encoding is refused")
}

func TestV2SnapshotQuotaSurvivesRestore(t *testing.T) {
	source := checkpointModuleSession(t, sessionstore.NewMemory())
	restoreCheckpointQuota(t, source, 17, time.Now().Add(time.Hour))
	raw, err := source.checkpoint.capture(checkpointLive, nil)
	require.NoError(t, err)
	snap, err := decodeSnapshot(raw)
	require.NoError(t, err)
	target := checkpointModuleSession(t, sessionstore.NewMemory())
	require.NoError(t, target.checkpoint.restore(checkpointRestore{quota: snap.State.Agent.V2.Quota}))
	// Observe in an earlier domain to exclude incidental refill from the exact
	// restored balance, then advance that same domain to exercise the burst cap.
	target.storeClock, target.storeClockAt = snap.State.Agent.V2.Quota.At.Add(-time.Second), time.Now()
	require.EqualValues(t, 17, checkpointQuota(t, target))
	malformed := *snap.State.Agent.V2.Quota
	malformed.Version++
	require.ErrorIs(t, target.checkpoint.restore(checkpointRestore{quota: &malformed}), agent.ErrIncompatibleAgent)
	require.EqualValues(t, 17, checkpointQuota(t, target), "a malformed restore cannot alter the balance")
	target.storeClock, target.storeClockAt = snap.State.Agent.V2.Quota.At.Add(time.Second), time.Now()
	require.EqualValues(t, target.snapshotLimits().BurstBytes, checkpointQuota(t, target))
}

func TestCheckpointScalarProgressDoesNotCopyTasks(t *testing.T) {
	s := checkpointModuleSession(t, sessionstore.NewMemory())
	pair := cloneAgentState(s.state.Agent)
	pair.V2.Tasks = []taskIntent{{ID: "task", Digest: "digest", Class: agent.SideEffecting, Phase: taskStarted, Request: make([]byte, 1024)}}
	require.NoError(t, s.checkpoint.restore(checkpointRestore{pair: &pair}))
	s.mu.Lock()
	s.state.Agent = cloneAgentState(pair)
	allocs := testing.AllocsPerRun(100, func() {
		p := s.checkpoint.progress(checkpointProgressRequest{})
		grant := s.checkpoint.progress(checkpointProgressRequest{grant: &pair.V2.Tasks[0]})
		if !p.hasAgent || !grant.taskGranted {
			panic("missing durable pair or grant")
		}
	})
	s.mu.Unlock()
	require.Zero(t, allocs, "scalar progress and grant observations must not allocate for durable tasks")
	allocs = testing.AllocsPerRun(100, func() {
		if s.taskCheckpointPending() {
			panic("acknowledged task must not be pending")
		}
	})
	require.Zero(t, allocs, "pending-task observation must not allocate")
}

func TestCheckpointReadinessDoesNotWaitForPacketLock(t *testing.T) {
	t.Cleanup(workerprobe.Enable())
	store := sessionstore.NewMemory()
	r, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	w, err := New(Config{Relay: &RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}})
	require.NoError(t, err)
	defer func() { require.NoError(t, w.Close()) }()
	snap := dtlsFixture(t)
	lease, err := store.Claim(context.Background(), snap.State.ID, w.LocalAddr(), time.Hour)
	require.NoError(t, err)
	s := sessionFromState(w, snap.State)
	t.Cleanup(s.close)
	s.lease = lease
	_, err = s.resume(snap.DTLSConnection, ResumeOptions{})
	require.NoError(t, err)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	w.mu.Lock()
	w.sessions[s.id] = s
	w.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	done := make(chan bool, 1)
	go func() { done <- workerprobe.SnapshotReady(w.LocalAddr(), s.id) }()
	select {
	case known := <-done:
		require.True(t, known)
	case <-time.After(time.Second):
		t.Fatal("production readiness waited for the packet lock")
	}
}

func TestCheckpointTaskProjectionOwnsRequestBytes(t *testing.T) {
	s := checkpointModuleSession(t, sessionstore.NewMemory())
	pair := cloneAgentState(s.state.Agent)
	pair.V2.Tasks = []taskIntent{{ID: "task", Request: []byte("request")}}
	require.NoError(t, s.checkpoint.restore(checkpointRestore{pair: &pair}))
	s.mu.Lock()
	projection := s.checkpoint.progress(checkpointProgressRequest{tasks: true})
	s.mu.Unlock()
	projection.tasks[0].Request[0] = 'x'
	projection.tasks[0].ID = "changed"
	s.mu.Lock()
	next := s.checkpoint.progress(checkpointProgressRequest{tasks: true})
	s.mu.Unlock()
	require.Equal(t, agent.TaskID("task"), next.tasks[0].ID)
	require.Equal(t, []byte("request"), next.tasks[0].Request)
}
