package mediaworker

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Admission must neither move a store-clock bucket nor publish refill credit.
func TestAdmissionUsesStoreClockWithoutMintingTokens(t *testing.T) {
	for _, offset := range []time.Duration{-2 * time.Second, 2 * time.Second} {
		for _, history := range []bool{false, true} {
			t.Run(offset.String()+map[bool]string{true: "history-change", false: "working-only"}[history], func(t *testing.T) {
				store := sessionstore.NewMemory()
				s := componentWriterSession(t, store, true)
				s.worker.splitLimits = SnapshotLimits{}.splitDefaults()
				s.agent = &agentHost{latest: cloneAgentState(s.state.Agent), v2: &v2Host{registration: agenthost.Registration{Bounds: effectiveAgentBounds(agent.Limits{}, s.snapshotLimits(), true)}}}
				require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
				s.mu.Lock()
				s.storeClock = time.Now().Add(offset) // store clock offset from the worker
				s.storeClockAt = time.Now()
				s.mu.Unlock()
				now := s.taskNow()
				restoreCheckpointQuota(t, s, 1000, now.Add(time.Hour))
				candidate := cloneAgentState(s.state.Agent)
				candidate.State.Bytes = []byte("changed working")
				if history {
					candidate.History = []byte("changed history")
				}
				_, err := s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
				t.Logf("admission err=%v", err)
				after := float64(checkpointQuota(t, s))
				require.EqualValues(t, 1000, after, "reads and admission cannot rebase a rolled-back clock or publish refill")
				if history {
					require.ErrorIs(t, err, agent.ErrCapacity)
				} else {
					require.NoError(t, err)
				}
			})
		}
	}
}

func TestSplitRegistrationRejectsImpossibleWorkingBound(t *testing.T) {
	for _, split := range []bool{false, true} {
		err := ValidateAgentRegistration("bound/v1", nil, agent.Limits{MaxStateBytes: 16384}, SnapshotLimits{}, split)
		require.ErrorIs(t, err, agent.ErrStateTooLarge)
	}
	require.NoError(t, ValidateAgentRegistration("bound/v1", nil, agent.Limits{MaxStateBytes: 6000}, SnapshotLimits{}, true))
	require.NoError(t, ValidateAgentRegistration("bound/v1", nil, agent.Limits{MaxStateBytes: 16384}, SnapshotLimits{MaxBytes: 20000, BytesPerSecond: 250000, BurstBytes: 80000}, true))
}

func TestSplitProfileDefaultsWithoutWorkerConstructor(t *testing.T) {
	s := componentWriterSession(t, sessionstore.NewMemory(), true)
	require.Zero(t, s.worker.splitLimits.MaxBytes)
	require.Equal(t, 80000, s.snapshotLimits().BytesPerSecond)
	require.Equal(t, 40960, s.snapshotLimits().BurstBytes)
	s.worker.splitLimits = SnapshotLimits{MaxBytes: 20000, BurstBytes: 80000}
	require.Equal(t, 20000, s.snapshotLimits().MaxBytes)
	require.Equal(t, 80000, s.snapshotLimits().BurstBytes)
	require.Equal(t, 80000, s.snapshotLimits().BytesPerSecond)
}

func TestLegacyHistoryRefusedEvenWithStalePositiveBounds(t *testing.T) {
	_, s := v2UnitSession(t, &historyStateAgent{state: agent.State{Version: 1}})
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.agent.v2.registration.Bounds.MaxHistoryBytes = 1024
	require.ErrorIs(t, s.validateV2StateMetadata(agent.State{Version: 1, History: []byte("history")}), agent.ErrHistoryTooLarge)
	s.state.Agent.History = []byte("history")
	require.ErrorIs(t, s.validateRestoredAgentBounds(s.agent.v2.registration.Bounds, false), agent.ErrHistoryTooLarge)
}

func TestRestoreRejectsHistoryBeforeFactory(t *testing.T) {
	_, source := historyUnitSession(t, &historyStateAgent{state: agent.State{Version: 1, History: []byte("1234")}}, agent.Limits{MaxHistoryBytes: 4})
	calls := 0
	_, target := v2UnitSession(t, &historyStateAgent{}, func(c *AgentConfig) {
		c.Factory = agenthost.Factory(agenthost.Registration{Name: "unit/v1", Bounds: agenthost.Bounds{MaxHistoryBytes: 4}, Factory: func(agent.FactoryArgs) agent.Agent { calls++; return &historyStateAgent{} }})
	})
	target.state = source.state
	target.state.Agent = cloneAgentState(source.state.Agent)
	target.state.Agent.History = []byte("12345")
	target.componentMode = true
	require.ErrorIs(t, target.initAgent(true, ResumeOptions{}), agent.ErrHistoryTooLarge)
	require.Zero(t, calls)
}

func TestAgentStateEqualityIncludesHistory(t *testing.T) {
	_, s := historyUnitSession(t, &historyStateAgent{state: agent.State{Version: 1, Bytes: []byte("work"), History: []byte("one")}}, agent.Limits{})
	next := cloneAgentState(s.state.Agent)
	require.True(t, agentStateEqual(s.state.Agent, next))
	next.History = []byte("two")
	require.False(t, agentStateEqual(s.state.Agent, next))
	require.Equal(t, []byte("one"), s.state.Agent.History)
}

type fixedCheckpointClockStore struct {
	*sessionstore.Memory
	now time.Time
}

func (s *fixedCheckpointClockStore) Clock(context.Context, string) (time.Time, error) {
	return s.now, nil
}

func TestRequiredCheckpointAfterColdWriteUsesRemainingQuota(t *testing.T) {
	store := &fixedCheckpointClockStore{Memory: sessionstore.NewMemory(), now: time.Now()}
	s := componentWriterSession(t, store, true)
	s.worker.splitLimits = SnapshotLimits{}.splitDefaults()
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	tuple, err := store.ReadComponents(context.Background(), s.id)
	require.NoError(t, err)
	packed, err := componentMetadata(tuple.Metadata).marshal()
	require.NoError(t, err)
	charge := len(tuple.Components.Transport) + componentCipherOverhead + len(packed) + componentCommitTimeBytes + componentFieldNameBytes + componentProtocolReserve + 8192 + 6 + componentCipherOverhead
	// Both versions can perform this cold write. Its acknowledgement leaves
	// exactly one maximum hot checkpoint plus one byte, with no timed refill.
	restoreCheckpointQuota(t, s, float64(charge+s.snapshotLimits().MaxBytes+1), store.now)
	s.mu.Lock()
	setComponentTestHistory(s, bytes.Repeat([]byte{'h'}, 8192))
	s.mu.Unlock()
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	require.EqualValues(t, s.snapshotLimits().MaxBytes+1, storedCheckpointQuota(t, store, s.id))
	// A required ROC/replay/transport checkpoint can spend those bytes now.
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	require.NoError(t, s.ctx.Err())
}

func TestHeldCheckpointSkipsAreSeparateFromFailures(t *testing.T) {
	s := componentWriterSession(t, sessionstore.NewMemory(), true)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	before := s.state.Checkpoint.Failures
	forced := s.worker.metrics.forcedFailures.Load()
	s.checkpoint.rollback(cloneAgentState(s.state.Agent))
	restoreCheckpointQuota(t, s, 1000, s.taskNow())
	require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointIntent), errCheckpointSkipped)
	require.Equal(t, before, s.state.Checkpoint.Failures)
	require.Equal(t, forced, s.worker.metrics.forcedFailures.Load())
	samples := map[string]float64{}
	for _, sample := range s.worker.metricSamples() {
		samples[sample.Name] = sample.Value
	}
	require.EqualValues(t, 1, samples["relais_worker_agent_checkpoint_skips_total"])
}

type canceledHistorySaveAgent struct {
	historyStateAgent
	contexts chan context.Context
	hold     func()
}

func (a *canceledHistorySaveAgent) Handle(_ context.Context, event agent.Event) (agent.Effects, error) {
	if event.Kind == agent.Control {
		a.state.History = bytes.Repeat(event.Control.Data, 4)
	}
	return agent.Effects{}, nil
}
func (a *canceledHistorySaveAgent) Save(ctx context.Context) (agent.State, error) {
	if a.contexts != nil {
		a.hold()
		a.contexts <- ctx
	}
	return a.historyStateAgent.Save(ctx)
}
func TestCanceledSaveRestoresHistoryReservation(t *testing.T) {
	a := &canceledHistorySaveAgent{historyStateAgent: historyStateAgent{state: agent.State{Version: 1, History: []byte("aaaa")}}}
	store := &fixedCheckpointClockStore{Memory: sessionstore.NewMemory(), now: time.Now()}
	s := componentWriterSession(t, store, true)
	s.worker.cfg.SnapshotInterval = 100 * time.Millisecond
	s.worker.cfg.Agent.Factory = agenthost.Factory(agenthost.Registration{Name: "cancel-history/v1", Factory: func(agent.FactoryArgs) agent.Agent { return a }})
	s.state.Agent = agentState{}
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	_, startedErr := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started, ID: "started/history"}, eventLive)
	require.NoError(t, startedErr)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	prior := cloneAgentState(s.state.Agent)
	s.worker.cfg.Agent.CallbackTimeout = time.Second
	a.contexts = make(chan context.Context, 1)
	a.hold = s.mu.Lock
	locked := false
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, eventErr0 := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, ID: "late-history", Control: &agent.ControlMessage{ID: "late", Sequence: 1, Name: "remember", Data: []byte{'b'}}}, eventLive)
		done <- eventErr0
	}()
	var callback context.Context
	select {
	case callback = <-a.contexts:
		locked = true
	case <-time.After(3 * time.Second):
		t.Fatal("Save did not start")
	}
	// Admission waits behind the held mutex until cancellation. Releasing it
	// lets the late callback install a tentative hold before agentCall rejects
	// its result. The production rollback must restore the old pair and hold.
	<-callback.Done()
	s.mu.Unlock()
	locked = false
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(3 * time.Second):
		t.Fatal("canceled Save did not settle")
	}
	require.Equal(t, []byte("aaaa"), a.state.History)
	require.Equal(t, []byte("aaaa"), s.state.Agent.History)
	require.NoError(t, s.ctx.Err())
	// Bound the real writer after cancellation. A smaller hold would leave room
	// for this tentative write; a hold for the tentative pair could spend it.
	maximum := splitComponentBudgets(s.snapshotLimits().MaxBytes, 8192).Total
	restoreCheckpointQuota(t, s, float64(maximum+s.snapshotLimits().MaxBytes), store.now.Add(time.Hour))
	tentative := cloneAgentState(prior)
	tentative.History = []byte("bbbb")
	s.mu.Lock()
	s.state.Agent = tentative
	s.mu.Unlock()
	require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointRequired), errCheckpointSkipped, "tentative pair cannot spend the prior pair's maximum rollback reservation")
	s.mu.Lock()
	s.state.Agent = cloneAgentState(prior)
	s.mu.Unlock()
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired), "prior pair can spend its restored maximum reservation")
	raw, _, err := ReadRecoverySnapshot(context.Background(), store, s.id)
	require.NoError(t, err)
	saved, err := decodeSnapshot(raw)
	require.NoError(t, err)
	require.Equal(t, prior.History, saved.State.Agent.History)

}

func TestBridgeLegacyBudgetRefusesV10AdoptionBeforeFactory(t *testing.T) {
	store := sessionstore.NewMemory()
	r, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	reg := agenthost.Registration{Name: "bridge-budget/v1", Factory: func(agent.FactoryArgs) agent.Agent {
		return &historyStateAgent{state: agent.State{Version: 1, History: []byte("history")}}
	}}
	cfg := Config{SplitCheckpoints: true, Agent: AgentConfig{Factory: agenthost.Factory(reg)}, SnapshotInterval: time.Hour, Relay: &RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}}
	source, err := New(cfg)
	require.NoError(t, err)
	defer func() { require.NoError(t, source.Close()) }()
	calls := 0
	reg.Factory = func(agent.FactoryArgs) agent.Agent { calls++; return &historyStateAgent{} }
	cfg.Agent.Factory = agenthost.Factory(reg)
	cfg.SplitCheckpoints = false
	cfg.SnapshotLimits.BurstBytes = 28672
	target, err := New(cfg)
	require.NoError(t, err, "an explicit legacy budget remains valid for legacy sessions")
	defer func() { require.NoError(t, target.Close()) }()
	r.AddWorker(source.LocalAddr())
	call, client := dialDTLSCaller(t, source)
	defer client.Close()
	require.Eventually(t, func() bool { return checkpointKnown(source.session(call.id)) }, 2*time.Second, time.Millisecond)
	data, err := source.ExportSession(call.id)
	require.NoError(t, err)
	lease, err := store.Get(context.Background(), call.id)
	require.NoError(t, err)
	lease, err = store.Transfer(context.Background(), lease, target.LocalAddr(), time.Minute)
	require.NoError(t, err)
	before, err := store.ReadComponents(context.Background(), call.id)
	require.NoError(t, err)
	_, err = target.ResumeSession(data, ResumeOptions{Lease: lease})
	require.ErrorIs(t, err, agent.ErrCapacity)
	require.Zero(t, calls, "budget refusal precedes SDK construction")
	require.Zero(t, target.SessionCount())
	after, err := store.ReadComponents(context.Background(), call.id)
	require.NoError(t, err)
	require.Equal(t, before.Metadata, after.Metadata)
	require.True(t, equalComponentBytes(before.Components, after.Components))
}

func TestHistoryLengthAdmissionMatchesActualCapture(t *testing.T) {
	s := componentWriterSession(t, sessionstore.NewMemory(), true)
	s.agent = &agentHost{latest: cloneAgentState(s.state.Agent), v2: &v2Host{registration: agenthost.Registration{Bounds: effectiveAgentBounds(agent.Limits{}, s.snapshotLimits(), true)}}}
	for _, length := range []int{0, 1, 8191, 8192, 8412, 8413} {
		candidate := cloneAgentState(s.state.Agent)
		candidate.History = bytes.Repeat([]byte{0xff}, length)
		// Unchanged history isolates encoded admission from rate admission.
		s.agent.latest = cloneAgentState(candidate)
		captureErr := persistCheckpointCandidate(t, s, candidate)
		_, admissionErr := s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
		if captureErr != nil {
			require.ErrorIs(t, admissionErr, agent.ErrCapacity)
		} else {
			require.NoError(t, admissionErr)
		}
	}
	// The same boundary agrees at the hot cap with an 8 KiB history.
	candidate := cloneAgentState(s.state.Agent)
	candidate.History = bytes.Repeat([]byte{0xff}, 8192)
	s.agent.latest = cloneAgentState(candidate)
	for _, work := range []int{6000, 7000} {
		candidate.State.Bytes = bytes.Repeat([]byte{'w'}, work)
		captureErr := persistCheckpointCandidate(t, s, candidate)
		_, admissionErr := s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
		if captureErr != nil {
			require.ErrorIs(t, admissionErr, agent.ErrCapacity)
		} else {
			require.NoError(t, admissionErr)
		}
	}
}

func TestAdmissionCaptureUsesStoreClockAtSizeBoundary(t *testing.T) {
	storeNow := time.Now().Add(-time.Hour)
	store := &fixedCheckpointClockStore{Memory: sessionstore.NewMemory(), now: storeNow}
	s := componentWriterSession(t, store, true)
	s.agent = &agentHost{latest: cloneAgentState(s.state.Agent), v2: &v2Host{registration: agenthost.Registration{Bounds: effectiveAgentBounds(agent.Limits{}, s.snapshotLimits(), true)}}}
	s.storeClock, s.storeClockAt = storeNow, time.Now()
	candidate := cloneAgentState(s.state.Agent)
	candidate.History = bytes.Repeat([]byte{'h'}, 8192)
	s.agent.latest = cloneAgentState(candidate)
	// The fixed-width wire format makes the size alone insensitive to the clock
	// domain. Inspect the actual production Save capture as well as its boundary.
	_, err := s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
	require.NoError(t, err)
	require.NoError(t, persistCheckpointCandidate(t, s, candidate))
	data, _, err := ReadRecoverySnapshot(context.Background(), store, s.id)
	require.NoError(t, err)
	snap, err := decodeSnapshot(data)
	require.NoError(t, err)
	require.WithinDuration(t, storeNow, snap.State.Checkpoint.CapturedAt, time.Second)
	require.Equal(t, snap.State.Checkpoint.CapturedAt, snap.State.Agent.V2.Quota.At)
	low, high := 0, 8193
	for low+1 < high {
		mid := (low + high) / 2
		candidate.State.Bytes = make([]byte, mid)
		err = persistCheckpointCandidate(t, s, candidate)
		if err == nil {
			low = mid
		} else {
			require.ErrorIs(t, err, agent.ErrCapacity)
			high = mid
		}
	}
	candidate.State.Bytes = make([]byte, low)
	_, err = s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
	require.NoError(t, err)
	candidate.State.Bytes = make([]byte, low+1)
	_, err = s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
	require.ErrorIs(t, err, agent.ErrCapacity)
}

func TestBridgeRejectsOversizedWorkingDeclarationBeforeFactory(t *testing.T) {
	store := sessionstore.NewMemory()
	r, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	reg := agenthost.Registration{Name: "large-declaration/v1", Bounds: agenthost.Bounds{MaxStateBytes: 16384}, Factory: func(agent.FactoryArgs) agent.Agent {
		return &historyStateAgent{state: agent.State{Version: 1, Bytes: []byte("small actual state"), History: []byte("history")}}
	}}
	cfg := Config{SplitCheckpoints: true, Agent: AgentConfig{Factory: agenthost.Factory(reg), CallbackTimeout: time.Second}, SnapshotInterval: time.Hour, SnapshotLimits: SnapshotLimits{MaxBytes: 20000, BytesPerSecond: 250000, BurstBytes: 80000}, Relay: &RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}}
	source, err := New(cfg)
	require.NoError(t, err)
	defer func() { require.NoError(t, source.Close()) }()
	calls := 0
	reg.Factory = func(agent.FactoryArgs) agent.Agent { calls++; return &historyStateAgent{} }
	cfg.SplitCheckpoints = false
	cfg.SnapshotLimits = SnapshotLimits{}
	cfg.Agent.Factory = agenthost.Factory(reg)
	target, err := New(cfg)
	require.NoError(t, err)
	defer func() { require.NoError(t, target.Close()) }()
	r.AddWorker(source.LocalAddr())
	call, client := dialDTLSCaller(t, source)
	defer client.Close()
	require.Eventually(t, func() bool { return checkpointKnown(source.session(call.id)) }, 2*time.Second, time.Millisecond)
	data, err := source.ExportSession(call.id)
	require.NoError(t, err)
	lease, err := store.Get(context.Background(), call.id)
	require.NoError(t, err)
	lease, err = store.Transfer(context.Background(), lease, target.LocalAddr(), time.Minute)
	require.NoError(t, err)
	before, err := store.ReadComponents(context.Background(), call.id)
	require.NoError(t, err)
	_, err = target.ResumeSession(data, ResumeOptions{Lease: lease})
	require.ErrorIs(t, err, agent.ErrStateTooLarge)
	require.Zero(t, calls, "a small actual state does not bypass the adoption declaration check")
	require.Zero(t, target.SessionCount())
	after, err := store.ReadComponents(context.Background(), call.id)
	require.NoError(t, err)
	require.Equal(t, before.Metadata, after.Metadata)
	require.True(t, equalComponentBytes(before.Components, after.Components))
}

func TestCheckpointSkipsDistinguishIntervalFromBudget(t *testing.T) {
	s := componentWriterSession(t, sessionstore.NewMemory(), true)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	s.worker.cfg.Agent.SaveInterval = time.Hour
	s.mu.Lock()
	s.state.Agent.State.Bytes = []byte("coalesced state")
	s.mu.Unlock()
	// Read the process-wide counters as deltas: other tests can share them.
	intervalBefore := checkpointWritesFromScrape(t, s.worker, "skipped-interval")
	budgetBefore := checkpointWritesFromScrape(t, s.worker, "skipped-budget")
	failures := s.state.Checkpoint.Failures
	require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointOptional), errCheckpointSkipped)
	require.Equal(t, intervalBefore+1, checkpointWritesFromScrape(t, s.worker, "skipped-interval"))
	require.Equal(t, budgetBefore, checkpointWritesFromScrape(t, s.worker, "skipped-budget"))
	s.checkpoint.rollback(cloneAgentState(s.state.Agent))
	restoreCheckpointQuota(t, s, 1000, s.taskNow())
	require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointIntent), errCheckpointSkipped)
	require.Equal(t, failures, s.state.Checkpoint.Failures)
	require.Equal(t, budgetBefore+1, checkpointWritesFromScrape(t, s.worker, "skipped-budget"))
	require.Contains(t, metricsBody(s.worker), "relais_worker_agent_checkpoint_budget_skips_total 1\n")
	require.Contains(t, metricsBody(s.worker), "relais_worker_agent_checkpoint_interval_skips_total 1\n")
	require.Contains(t, metricsBody(s.worker), "relais_worker_agent_checkpoint_skips_total 2\n")
}

// Exercise the admission decision itself: a later writer cannot prove which
// clock admission used. Opposite offsets catch both minted and withheld credit.
func TestCheckpointAdmissionUsesStoreClockForHistoryCredit(t *testing.T) {
	for _, offset := range []time.Duration{-time.Hour, time.Hour} {
		t.Run(offset.String(), func(t *testing.T) {
			store := &fixedCheckpointClockStore{Memory: sessionstore.NewMemory(), now: time.Now().Add(offset)}
			s := checkpointModuleSession(t, store)
			require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
			prior := cloneAgentState(s.state.Agent)
			candidate := cloneAgentState(prior)
			candidate.History = []byte("new history")
			anchor := store.now
			if offset > 0 {
				anchor = anchor.Add(-time.Second)
			}
			restoreCheckpointQuota(t, s, 1000, anchor)
			_, err := s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: prior})
			if offset < 0 {
				require.ErrorIs(t, err, agent.ErrCapacity, "worker time must not mint an hour of history-admission credit")
			} else {
				require.NoError(t, err, "worker time must not withhold store-clock refill credit")
			}
			require.Equal(t, prior, s.state.Agent, "admission never publishes the candidate")
		})
	}
}

func TestCheckpointAdmissionSizeIgnoresStoreClockOffset(t *testing.T) {
	workerNow := time.Now()
	store := &fixedCheckpointClockStore{Memory: sessionstore.NewMemory(), now: workerNow}
	s := checkpointModuleSession(t, store)
	prior := cloneAgentState(s.state.Agent)
	candidate := cloneAgentState(prior)
	candidate.State.Bytes = []byte("candidate working state")
	var sizes []int
	for _, offset := range []time.Duration{-time.Hour, time.Hour} {
		store.now = workerNow.Add(offset)
		// Observe each store-clock domain through an acknowledged write. Keep
		// history unchanged so the separate credit-admission rule is irrelevant.
		require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
		size, err := s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: prior})
		require.NoError(t, err)
		require.Positive(t, size)
		sizes = append(sizes, size)
		require.Equal(t, prior, s.state.Agent, "admission cannot publish the candidate")
	}
	require.Equal(t, sizes[0], sizes[1], "fixed-width capture timestamps and quota must not change admission size across store-clock offsets")
}
