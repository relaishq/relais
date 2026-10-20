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
				require.NoError(t, s.persistSnapshotMode(context.Background(), true))
				limits := s.snapshotLimits()
				s.mu.Lock()
				s.storeClock = time.Now().Add(offset) // store clock offset from the worker
				s.storeClockAt = time.Now()
				s.mu.Unlock()
				now := s.taskNow()
				s.snapshotBudgetMu.Lock()
				s.snapshotBytesBudget = snapshotBucket{tokens: 1000, at: now}
				s.snapshotBudgetMu.Unlock()
				candidate := cloneAgentState(s.state.Agent)
				candidate.State.Bytes = []byte("changed working")
				if history {
					candidate.History = []byte("changed history")
				}
				_, err := s.admissionSnapshot(candidate)
				t.Logf("admission err=%v", err)
				s.snapshotBudgetMu.Lock()
				after := s.snapshotBytesBudget.available(limits, now)
				at := s.snapshotBytesBudget.at
				s.snapshotBudgetMu.Unlock()
				t.Logf("tokens before=1000 after=%.0f; bucket.at moved %v", after, at.Sub(now))
				require.EqualValues(t, 1000, after, "admission cannot publish refill credit")
				require.Equal(t, now, at, "admission cannot move the bucket clock")
				if history {
					require.ErrorIs(t, err, agent.ErrCapacity)
				} else {
					require.NoError(t, err)
				}
			})
		}
	}
}

func TestSnapshotBucketReadsArePureAndWritesRebase(t *testing.T) {
	l := SnapshotLimits{}.splitDefaults()
	at := time.Now()
	b := snapshotBucket{tokens: 1000, at: at}
	require.EqualValues(t, 1000, b.available(l, at.Add(-2*time.Second)))
	require.Equal(t, at, b.at)
	b.debit(l, 100, at.Add(-time.Second))
	require.Equal(t, at.Add(-time.Second), b.at)
	require.EqualValues(t, 900, b.available(l, at.Add(-time.Second)))
	require.EqualValues(t, 8900, b.available(l, at.Add(-900*time.Millisecond)))
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

func TestHistoryAdmissionReservesFutureHotGrowth(t *testing.T) {
	_, s := historyUnitSession(t, &historyStateAgent{state: agent.State{Version: 1}}, agent.Limits{})
	candidate := cloneAgentState(s.agent.latest)
	candidate.History = []byte("new")
	sizes := componentSizes{Hot: 1, Setup: 100, History: 100}
	required := 2*s.snapshotLimits().MaxBytes + 200
	now := s.taskNow()
	// Pin taskNow's sample far behind the debit time so no incidental refill
	// can cover the one-byte boundary during this arithmetic test.
	s.snapshotBytesBudget = snapshotBucket{tokens: float64(required - 1), at: now.Add(time.Hour)}
	require.ErrorIs(t, s.admitHistoryUpdate(candidate, sizes), agent.ErrCapacity)
	s.snapshotBytesBudget.tokens++
	require.NoError(t, s.admitHistoryUpdate(candidate, sizes))
	require.Equal(t, s.snapshotLimits().MaxBytes+200, s.snapshotHistoryHold.bytes)
	require.Equal(t, now.Add(time.Hour), s.snapshotBytesBudget.at)
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
	require.NoError(t, s.persistSnapshotMode(context.Background(), true))
	tuple, err := store.ReadComponents(context.Background(), s.id)
	require.NoError(t, err)
	packed, err := componentMetadata(tuple.Metadata).marshal()
	require.NoError(t, err)
	charge := len(tuple.Components.Transport) + componentCipherOverhead + len(packed) + componentCommitTimeBytes + componentFieldNameBytes + componentProtocolReserve + 8192 + 6 + componentCipherOverhead
	// Both versions can perform this cold write. Its acknowledgement leaves
	// exactly one maximum hot checkpoint plus one byte, with no timed refill.
	s.snapshotBytesBudget = snapshotBucket{tokens: float64(charge + s.snapshotLimits().MaxBytes + 1), at: store.now}
	s.mu.Lock()
	setComponentTestHistory(s, bytes.Repeat([]byte{'h'}, 8192))
	s.mu.Unlock()
	require.NoError(t, s.persistSnapshotMode(context.Background(), true))
	require.EqualValues(t, s.snapshotLimits().MaxBytes+1, s.snapshotBytesBudget.tokens)
	// A required ROC/replay/transport checkpoint can spend those bytes now.
	require.NoError(t, s.persistSnapshotMode(context.Background(), true))
	require.NoError(t, s.ctx.Err())
}

func TestHeldCheckpointSkipsAreSeparateFromFailures(t *testing.T) {
	s := componentWriterSession(t, sessionstore.NewMemory(), true)
	require.NoError(t, s.persistSnapshotMode(context.Background(), true))
	before := s.state.Checkpoint.Failures
	forced := s.worker.metrics.forcedFailures.Load()
	s.snapshotHistoryHold = &historyWriteHold{version: s.state.Agent.State.Version, history: []byte("newer"), bytes: s.componentBudgets().Total}
	s.snapshotBytesBudget = snapshotBucket{tokens: 1000, at: s.taskNow()}
	require.ErrorIs(t, s.persistSnapshotBudget(context.Background(), true, true), errCheckpointSkipped)
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
	_, s := historyUnitSession(t, a, agent.Limits{})
	s.worker.cfg.Agent.CallbackTimeout = time.Second
	a.contexts = make(chan context.Context, 1)
	a.hold = s.snapshotBudgetMu.Lock
	locked := false
	defer func() {
		if locked {
			s.snapshotBudgetMu.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		done <- s.handleV2Event(agent.Event{Kind: agent.Control, ID: "late-history", Control: &agent.ControlMessage{ID: "late", Sequence: 1, Name: "remember", Data: []byte{'b'}}})
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
	s.snapshotBudgetMu.Unlock()
	locked = false
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(3 * time.Second):
		t.Fatal("canceled Save did not settle")
	}
	require.Equal(t, []byte("aaaa"), a.state.History)
	require.NotNil(t, s.snapshotHistoryHold)
	require.True(t, s.snapshotHistoryHold.matches(s.agent.latest))
	require.Equal(t, s.componentBudgets().Total, s.snapshotHistoryHold.bytes, "rollback protects a maximum transaction, not zero bytes")
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
	require.Eventually(t, source.session(call.id).snapshotStored.Load, 2*time.Second, time.Millisecond)
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
		now := s.taskNow()
		_, _, captureErr := s.captureSnapshotCandidate(now, &candidate)
		_, admissionErr := s.admissionSnapshot(candidate)
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
		_, _, captureErr := s.captureSnapshotCandidate(s.taskNow(), &candidate)
		_, admissionErr := s.admissionSnapshot(candidate)
		if captureErr != nil {
			require.ErrorIs(t, admissionErr, agent.ErrCapacity)
		} else {
			require.NoError(t, admissionErr)
		}
	}
}

func TestAdmissionCaptureUsesStoreClockAtSizeBoundary(t *testing.T) {
	s := componentWriterSession(t, sessionstore.NewMemory(), true)
	s.agent = &agentHost{latest: cloneAgentState(s.state.Agent), v2: &v2Host{registration: agenthost.Registration{Bounds: effectiveAgentBounds(agent.Limits{}, s.snapshotLimits(), true)}}}
	storeNow := time.Now().Add(-time.Hour)
	s.storeClock, s.storeClockAt = storeNow, time.Now()
	candidate := cloneAgentState(s.state.Agent)
	candidate.History = bytes.Repeat([]byte{'h'}, 8192)
	s.agent.latest = cloneAgentState(candidate)
	// The fixed-width wire format makes the size alone insensitive to the clock
	// domain. Inspect the actual production Save capture as well as its boundary.
	data, err := s.captureAdmissionSnapshot(candidate)
	require.NoError(t, err)
	snap, err := decodeSnapshot(data)
	require.NoError(t, err)
	require.WithinDuration(t, storeNow, snap.State.Checkpoint.CapturedAt, time.Second)
	require.Equal(t, snap.State.Checkpoint.CapturedAt, snap.State.Agent.V2.Quota.At)
	low, high := 0, 8193
	for low+1 < high {
		mid := (low + high) / 2
		candidate.State.Bytes = make([]byte, mid)
		_, _, err = s.captureSnapshotCandidate(s.taskNow(), &candidate)
		if err == nil {
			low = mid
		} else {
			require.ErrorIs(t, err, agent.ErrCapacity)
			high = mid
		}
	}
	candidate.State.Bytes = make([]byte, low)
	_, err = s.admissionSnapshot(candidate)
	require.NoError(t, err)
	candidate.State.Bytes = make([]byte, low+1)
	_, err = s.admissionSnapshot(candidate)
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
	require.Eventually(t, source.session(call.id).snapshotStored.Load, 2*time.Second, time.Millisecond)
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
	require.NoError(t, s.persistSnapshotMode(context.Background(), true))
	s.worker.cfg.Agent.SaveInterval = time.Hour
	s.mu.Lock()
	s.state.Agent.State.Bytes = []byte("coalesced state")
	s.mu.Unlock()
	// Read the process-wide counters as deltas: other tests can share them.
	intervalBefore := checkpointWritesFromScrape(t, s.worker, "skipped-interval")
	budgetBefore := checkpointWritesFromScrape(t, s.worker, "skipped-budget")
	failures := s.state.Checkpoint.Failures
	require.ErrorIs(t, s.persistSnapshotBudget(context.Background(), false, false), errCheckpointSkipped)
	require.Equal(t, intervalBefore+1, checkpointWritesFromScrape(t, s.worker, "skipped-interval"))
	require.Equal(t, budgetBefore, checkpointWritesFromScrape(t, s.worker, "skipped-budget"))
	s.snapshotHistoryHold = &historyWriteHold{version: s.state.Agent.State.Version, history: []byte("newer"), bytes: s.componentBudgets().Total}
	s.snapshotBytesBudget = snapshotBucket{tokens: 1000, at: s.taskNow()}
	require.ErrorIs(t, s.persistSnapshotBudget(context.Background(), true, true), errCheckpointSkipped)
	require.Equal(t, failures, s.state.Checkpoint.Failures)
	require.Equal(t, budgetBefore+1, checkpointWritesFromScrape(t, s.worker, "skipped-budget"))
	require.Contains(t, metricsBody(s.worker), "relais_worker_agent_checkpoint_budget_skips_total 1\n")
	require.Contains(t, metricsBody(s.worker), "relais_worker_agent_checkpoint_interval_skips_total 1\n")
	require.Contains(t, metricsBody(s.worker), "relais_worker_agent_checkpoint_skips_total 2\n")
}
