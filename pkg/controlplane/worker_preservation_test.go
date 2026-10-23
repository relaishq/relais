package controlplane

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func (s *stalledPool) addCall(t *testing.T) sessionstore.Lease {
	t.Helper()
	id, _, err := s.a.fakeWorker.CreateSession(context.Background(), "offer")
	require.NoError(t, err)
	lease, err := s.p.store.Get(context.Background(), id)
	require.NoError(t, err)
	require.NoError(t, s.p.store.PutState(context.Background(), lease, takeoverSnapshot(t, id, 0)))
	s.p.calls[id] = &call{id: id, lease: lease}
	s.a.inventory = append(s.a.inventory, mediaworker.InstalledSession{ID: id, Owner: lease.Worker, Epoch: lease.Epoch})
	return lease
}

func TestWorkerPreservationKeepsAllCalls(t *testing.T) {
	s := newStalledPool(t)
	second := s.addCall(t)
	s.now = s.now.Add(100 * time.Millisecond)
	s.sourceReturns(t)
	s.targetReturns(t)
	source := s.p.workers["a"]
	s.p.recovery.pending[source] = map[string]*moveTransfer{}
	for _, lease := range []sessionstore.Lease{s.lease, second} {
		s.p.recovery.pending[source][lease.SessionID] = &moveTransfer{call: s.p.calls[lease.SessionID], lease: lease,
			routed: s.a.addr, routedLease: lease, excluded: map[netip.AddrPort]bool{s.a.addr: true}, attemptLimit: maxResumeAttempts,
			unrequested: true, deathCycle: source.observation.deathCycle}
	}
	require.Nil(t, s.p.recovery.recover(context.Background(), source, s.lease, s.now))
	require.Empty(t, s.p.recovery.pending[source], "one worker decision must abandon both pending recoveries before the other job runs")
	require.True(t, s.p.recoverWorker(context.Background(), source, s.now))
	require.True(t, s.p.workers["a"].observation.knowledge().eligible, "both source calls must return to service together")
	for _, before := range []sessionstore.Lease{s.lease, second} {
		after, err := s.p.store.Get(context.Background(), before.SessionID)
		require.NoError(t, err)
		require.Equal(t, before.Epoch, after.Epoch, "whole-worker preservation must not advance either epoch")
		require.Equal(t, before.Worker, after.Worker)
		require.True(t, s.a.runs(before.SessionID))
	}
	require.Empty(t, s.p.recovery.pending[s.p.workers["a"]], "all pending recoveries must be abandoned together")
	require.Empty(t, s.p.recentTakeovers())
	require.Zero(t, s.p.lostCount)
	// Removing every preserved lease must not strand the registration dead.
	for _, lease := range []sessionstore.Lease{s.lease, second} {
		require.NoError(t, s.p.End(context.Background(), lease.SessionID))
	}
	require.NoError(t, s.p.Heartbeat(s.a.addr), "a fully preserved source must need no rejoin challenge after its last call leaves")
	require.True(t, s.p.workers["a"].observation.knowledge().eligible)
}

func TestWorkerPreservationForbiddenAfterAnyTransfer(t *testing.T) {
	s := newStalledPool(t)
	second := s.addCall(t)
	s.targetReturns(t)
	require.NotNil(t, s.p.recovery.recover(context.Background(), s.p.workers["a"], s.lease, s.now))
	// Superseded installation is already gone: the death-cycle Transfer marker,
	// rather than stale inventory, must forbid preserving the second call.
	s.a.inventory = s.a.inventory[1:]
	s.a.mu.Lock()
	delete(s.a.running, s.id)
	s.a.mu.Unlock()
	s.now = s.now.Add(100 * time.Millisecond)
	s.sourceReturns(t)
	require.True(t, s.p.recoverWorker(context.Background(), s.p.workers["a"], s.now))
	o := &s.p.workers["a"].observation
	require.False(t, o.deathPreserved, "one issued Transfer must forbid preservation of the other call")
	for _, lease := range []sessionstore.Lease{s.lease, second} {
		current, err := s.p.store.Get(context.Background(), lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, s.b.addr, current.Worker, "all source calls must follow recovery after the first Transfer")
	}
	o.recoveryFinished(true)
	require.True(t, o.canReplace(0), "settled recovery must not leave a preservation quarantine")
	require.ErrorIs(t, s.p.Heartbeat(s.a.addr), mediaworker.ErrRejoinRequired, "fenced source must use the existing drop-all challenge")
	s.a.mu.Lock()
	clear(s.a.running)
	s.a.mu.Unlock()
	require.NoError(t, s.p.Heartbeat(s.a.addr))
	require.True(t, o.knowledge().eligible)
}

func TestWorkerPreservationRejectsInventorySubset(t *testing.T) {
	s := newStalledPool(t)
	s.addCall(t)
	s.a.inventory = s.a.inventory[:1]
	s.now = s.now.Add(100 * time.Millisecond)
	s.sourceReturns(t)
	s.targetReturns(t)
	s.p.recovery.recover(context.Background(), s.p.workers["a"], s.lease, s.now)
	require.False(t, s.p.workers["a"].observation.deathPreserved, "strict-subset inventory must not preserve a worker with two authoritative tenures")
	current, err := s.p.store.Get(context.Background(), s.id)
	require.NoError(t, err)
	require.Equal(t, s.b.addr, current.Worker)
}

func TestWorkerPreservationRechecksOwnership(t *testing.T) {
	s := newStalledPool(t)
	detected := s.now
	s.now = s.now.Add(100 * time.Millisecond)
	s.sourceReturns(t)
	s.a.statusHook = func(context.Context) {
		_, err := s.p.store.Transfer(context.Background(), s.lease, s.b.addr, time.Minute)
		require.NoError(t, err)
	}
	pending := &moveTransfer{call: s.p.calls[s.id], lease: s.lease, unrequested: true, firstDetection: detected, deathCycle: s.p.workers["a"].observation.deathCycle}
	s.p.recovery.pending[s.p.workers["a"]] = map[string]*moveTransfer{s.id: pending}
	require.False(t, s.p.recovery.preserveSource(context.Background(), s.p.workers["a"], pending), "ownership changed during inventory must veto worker preservation")
	require.False(t, s.p.workers["a"].observation.knowledge().eligible)
}

func TestWorkerPreservationRecheckedBeforeTransfer(t *testing.T) {
	s := newStalledPool(t)
	detected := s.now
	s.now = s.now.Add(100 * time.Millisecond)
	s.sourceReturns(t)
	s.targetReturns(t)
	reads := 0
	s.a.statusHook = func(context.Context) { reads++; s.a.truncated = reads == 1 }
	s.p.recovery.recover(context.Background(), s.p.workers["a"], s.lease, detected)
	current, err := s.p.store.Get(context.Background(), s.id)
	require.NoError(t, err)
	require.Equal(t, s.lease.Epoch, current.Epoch, "complete inventory becoming available during selection must preserve before Transfer")
	require.True(t, s.p.workers["a"].observation.knowledge().eligible)
	require.Equal(t, 2, reads, "source preservation must be checked again just before Transfer")
}

func TestWorkerPreservationHeartbeatBeforeTransfer(t *testing.T) {
	s := newStalledPool(t)
	s.targetReturns(t)
	issued := false
	s.p.recovery.beforeTransfer = func() {
		if !issued {
			issued = true
			s.now = s.now.Add(100 * time.Millisecond)
			s.sourceReturns(t)
		}
	}
	s.p.recovery.recover(context.Background(), s.p.workers["a"], s.lease, s.now)
	current, err := s.p.store.Get(context.Background(), s.id)
	require.NoError(t, err)
	require.Equal(t, s.lease.Epoch, current.Epoch, "heartbeat between the last source check and Transfer must trigger preservation")
	require.True(t, s.p.workers["a"].observation.knowledge().eligible)
}

func TestWorkerPreservationSlowInventoryDoesNotStarve(t *testing.T) {
	s := newStalledPool(t)
	detected := s.now
	s.targetReturns(t)
	s.now = s.now.Add(100 * time.Millisecond)
	s.sourceReturns(t)
	reads := 0
	s.a.statusHook = func(context.Context) {
		reads++
		s.now = s.now.Add(250 * time.Millisecond)
		s.sourceReturns(t)
		s.a.truncated = true
	}
	result := s.p.recovery.recover(context.Background(), s.p.workers["a"], s.lease, detected)
	require.NotNil(t, result, "slow inventory and continuing heartbeats must resolve by D")
	require.False(t, result.Lost, "ordinary heartbeats during failed inventory must not starve an eligible target")
	require.LessOrEqual(t, reads, 2, "failed confirmation must not restart selection on each heartbeat")
	require.False(t, s.now.After(detected.Add(takeoverBudget)), "recovery must end within the original deadline")
}

func TestTargetWaitHeartbeatWakesWaiter(t *testing.T) {
	s := newStalledPool(t)
	woken := false
	s.p.recovery.targetWait = func(ctx context.Context, changed <-chan struct{}, _ time.Duration) error {
		require.ErrorContains(t, s.p.Heartbeat(s.a.addr), "takeover in progress")
		select {
		case <-changed:
			woken = true
		default:
			require.FailNow(t, "Heartbeat() must wake the waiting source recovery")
		}
		return waitTargetObservation(ctx, changed, time.Hour, time.Now().Add(time.Hour))
	}
	require.True(t, s.p.recoverWorker(context.Background(), s.p.workers["a"], s.now))
	require.True(t, woken, "waiter must be woken through the real Heartbeat entry point")
	require.True(t, s.p.workers["a"].observation.knowledge().eligible)
}

func TestTargetWaitIgnoresOrdinaryHeartbeats(t *testing.T) {
	s := newStalledPool(t)
	require.Error(t, s.p.Heartbeat(s.a.addr))
	changed := s.p.workerChanged
	require.Error(t, s.p.Heartbeat(s.a.addr))
	require.Equal(t, changed, s.p.workerChanged, "only the dead source's first returning heartbeat may wake waiters")
	s.targetReturns(t)
	changed = s.p.workerChanged
	require.NoError(t, s.p.Heartbeat(s.b.addr))
	require.Equal(t, changed, s.p.workerChanged, "healthy worker heartbeats must not amplify wakeups")
}

func TestTargetWaitLeavesRequestedRecoveryUnchanged(t *testing.T) {
	for _, planned := range []bool{false, true} {
		t.Run(map[bool]string{false: "rebuild", true: "planned"}[planned], func(t *testing.T) {
			s := newStalledPool(t)
			source := s.p.workers["a"]
			start := s.now.Add(-time.Second)
			s.p.recovery.pending[source] = map[string]*moveTransfer{s.id: {call: s.p.calls[s.id], lease: s.lease, planned: planned, held: true,
				routed: s.a.addr, routedLease: s.lease, excluded: map[netip.AddrPort]bool{s.a.addr: true}, attemptLimit: maxResumeAttempts,
				firstDetection: start.Add(-time.Hour), targetDeadline: start.Add(-time.Hour)}}
			s.p.recovery.targetWait = func(context.Context, <-chan struct{}, time.Duration) error {
				require.FailNow(t, "planned moves and rebuild must not enter the takeover wait")
				return nil
			}
			result := s.p.recovery.recover(context.Background(), source, s.lease, start)
			require.NotNil(t, result)
			require.Equal(t, start, result.Start, "requested recovery must keep its original Start")
			require.NotContains(t, result.Error, ErrTargetWaitExpired.Error(), "requested recovery must keep its existing target retry policy")
			require.True(t, result.Lost)
		})
	}
}
