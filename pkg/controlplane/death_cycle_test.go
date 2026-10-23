package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestAbandonedRecoveryCannotTransferInLaterCycle(t *testing.T) {
	s := newStalledPool(t)
	second := s.addCall(t)
	s.p.config.TakeoverParallelism = 1
	s.targetReturns(t)
	s.now = s.now.Add(100 * time.Millisecond)
	s.sourceReturns(t)
	source := s.p.workers["a"]
	fault := &interruptedTransferStore{Store: s.p.store, uncertain: true}
	s.p.store = fault
	s.a.statusHook = func(context.Context) {
		s.a.statusHook = nil
		// X is confirming inventory. Y preserves the whole cycle, deleting X's
		// pending entry. A second death then clears the preservation marker.
		require.Nil(t, s.p.recovery.recover(context.Background(), source, second, s.now))
		require.True(t, source.observation.deathPreserved)
		require.Empty(t, s.p.recovery.pending[source])
		s.now = s.now.Add(s.p.config.DeadAfter + time.Millisecond)
		require.True(t, source.observation.detect(s.now, s.p.config.DeadAfter).newDeath)
		s.a.truncated = true // X's old confirmation fails after cycle 2 begins.
	}
	require.Nil(t, s.p.recovery.recover(context.Background(), source, s.lease, s.now))
	current, err := s.p.store.Get(context.Background(), s.id)
	require.NoError(t, err)
	require.Equal(t, s.lease.Worker, current.Worker, "abandoned prior-cycle job must not issue Transfer with an untracked pending")
	require.Equal(t, s.lease.Epoch, current.Epoch)
	require.False(t, fault.interrupted, "the abandoned job must not reach the uncertain Transfer seam")
	// Cycle 2 must retain and retry an uncertain adoption rather than orphan X.
	require.False(t, s.p.recoverWorker(context.Background(), source, s.now))
	require.NotEmpty(t, s.p.recovery.pending[source], "cycle 2 must retain an uncertain adoption")
	require.True(t, s.p.recoverWorker(context.Background(), source, s.now))
	for _, lease := range []sessionstore.Lease{s.lease, second} {
		require.True(t, s.b.runs(lease.SessionID), "cycle 2 must adopt both calls without an ownership-expired orphan")
	}
	require.Empty(t, s.p.recovery.pending[source])
	require.Zero(t, s.p.lostCount)
}

func TestPreservedDecisionVetoesTransferAtBoundary(t *testing.T) {
	s := newStalledPool(t)
	s.targetReturns(t)
	source := s.p.workers["a"]
	s.p.recovery.beforeTransfer = func() {
		// Pin the decision marker independently of the membership guard. The
		// pending remains registered at this controlled publication boundary.
		s.p.mu.Lock()
		source.observation.deathPreserved = true
		s.p.mu.Unlock()
	}
	require.Nil(t, s.p.recovery.recover(context.Background(), source, s.lease, s.now), "published deathPreserved decision must abandon recovery before Transfer")
	current, err := s.p.store.Get(context.Background(), s.id)
	require.NoError(t, err)
	require.Equal(t, s.lease.Epoch, current.Epoch, "published deathPreserved decision must veto Transfer even with registered pending")
	require.False(t, s.b.runs(s.id))
}

type ownershipReadHookStore struct {
	sessionstore.Store
	id        string
	afterRead func()
}

func (s *ownershipReadHookStore) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	lease, err := s.Store.Get(ctx, id)
	if id == s.id && s.afterRead != nil {
		hook := s.afterRead
		s.afterRead = nil
		hook()
	}
	return lease, err
}

func TestFinalPreservationRejectsTransferCompletedDuringRead(t *testing.T) {
	s := newStalledPool(t)
	second := s.addCall(t)
	detected := s.now
	s.now = s.now.Add(100 * time.Millisecond)
	s.sourceReturns(t)
	source := s.p.workers["a"]
	pending := &moveTransfer{call: s.p.calls[s.id], lease: s.lease, unrequested: true, firstDetection: detected, deathCycle: source.observation.deathCycle}
	s.p.recovery.pending[source] = map[string]*moveTransfer{s.id: pending, second.SessionID: {call: s.p.calls[second.SessionID], lease: second, unrequested: true}}
	backing := &ownershipReadHookStore{Store: s.p.store, id: second.SessionID}
	s.p.store = backing
	backing.afterRead = func() {
		// The read already sampled Y's old tenure. Y completes Transfer and
		// leaves pending before X acquires the final publication lock.
		_, err := s.p.recovery.transfer(context.Background(), second, s.b.addr)
		require.NoError(t, err)
		s.p.recovery.finishPending(source, second.SessionID)
	}
	require.False(t, s.p.recovery.preserveSource(context.Background(), source, pending), "Transfer completed during confirmation must veto the final preservation decision")
	require.True(t, source.observation.deathTransferIssued)
	require.False(t, source.observation.deathPreserved)
}

func TestRequestedTransferMarksDeadSourceCycle(t *testing.T) {
	s := newStalledPool(t)
	second := s.addCall(t)
	detected := s.now
	s.now = s.now.Add(100 * time.Millisecond)
	s.sourceReturns(t)
	source := s.p.workers["a"]
	pending := &moveTransfer{call: s.p.calls[s.id], lease: s.lease, unrequested: true, firstDetection: detected, deathCycle: source.observation.deathCycle}
	s.p.recovery.pending[source] = map[string]*moveTransfer{s.id: pending, second.SessionID: {call: s.p.calls[second.SessionID], lease: second, planned: true}}
	// Planned and rebuild recovery call transfer directly, without the
	// unrequested takeover loop's Transfer-issued publication.
	_, err := s.p.recovery.transfer(context.Background(), second, s.b.addr)
	require.NoError(t, err)
	s.p.recovery.finishPending(source, second.SessionID)
	s.a.inventory = s.a.inventory[:1]
	require.True(t, source.observation.deathTransferIssued, "planned or rebuild Transfer while dead must mark the worker death cycle")
	require.False(t, s.p.recovery.preserveSource(context.Background(), source, pending), "requested Transfer must forbid preserving the remaining call")
}

func TestQueuedTakeoverAfterTargetDeadlineStillAdopts(t *testing.T) {
	for _, waited := range []bool{false, true} {
		t.Run(map[bool]string{false: "first-selection", true: "previous-wait"}[waited], func(t *testing.T) {
			s := newStalledPool(t)
			detected := s.now
			source := s.p.workers["a"]
			if waited {
				s.p.recovery.targetWait = func(context.Context, <-chan struct{}, time.Duration) error { return context.Canceled }
				require.Nil(t, s.p.recovery.recover(context.Background(), source, s.lease, detected))
			}
			s.now = s.now.Add(2 * time.Second) // queued behind the earlier recovery jobs
			s.sourceReturns(t)
			s.targetReturns(t)
			s.a.statusHook = func(context.Context) { require.FailNow(t, "preservation confirmation must be skipped after D") }
			result := s.p.recovery.recover(context.Background(), source, s.lease, detected)
			require.NotNil(t, result)
			require.False(t, result.Lost, "a queued call with an eligible target after D must be adopted, not lost")
			require.True(t, s.b.runs(s.id))
			require.Zero(t, s.p.lostCount)
		})
	}
}

func TestTargetWaitDeadlineWakeFinishesKnownNoTarget(t *testing.T) {
	s := newStalledPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := s.p.workers["a"]
	s.p.recovery.targetWait = func(context.Context, <-chan struct{}, time.Duration) error {
		s.now = s.p.recovery.pending[source][s.id].targetDeadline
		// Model the timer winning the select at D, with context expiry visible
		// before the next selection. No store outcome is uncertain here.
		cancel()
		return nil
	}
	result := s.p.recovery.recover(ctx, source, s.lease, s.now)
	require.NotNil(t, result, "a wake at D must finish the known no-target attempt before selection sees cancellation")
	require.True(t, result.Lost)
	require.Contains(t, result.Error, ErrTargetWaitExpired.Error())
	require.EqualValues(t, 1, s.p.lostCount)
	require.Empty(t, s.p.recovery.pending[source])
}
