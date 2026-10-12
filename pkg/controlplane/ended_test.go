package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type releasedDuringEndWorker struct{ *fakeWorker }

func (w *releasedDuringEndWorker) EndSession(id string) error {
	lease, err := w.store.Get(context.Background(), id)
	if err != nil {
		return err
	}
	if err := w.store.Release(context.Background(), lease); err != nil {
		return err
	}
	return mediaworker.ErrUnknownSession
}

func TestWorkerReleaseAndDeleteRaceEndsWithoutLoss(t *testing.T) {
	for _, duringEnd := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-delete", true: "during-delete"}[duringEnd], func(t *testing.T) {
			p, a, _, _ := setup(t)
			ctx := context.Background()
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			if duringEnd {
				p.workers["a"].worker = &releasedDuringEndWorker{a}
			} else {
				lease, err := p.store.Get(ctx, id)
				require.NoError(t, err)
				require.NoError(t, p.store.Release(ctx, lease))
			}
			require.NoError(t, p.End(ctx, id))
			status, err := p.Status(ctx)
			require.NoError(t, err)
			require.Empty(t, status.Calls)
			require.Zero(t, status.LostCount)
			require.Empty(t, status.Takeovers)
			p.reconcileOwnership(ctx)
			require.Empty(t, p.calls)
			requireControlLosses(t, planeMetricsBody(p), 0)
		})
	}
}

func TestRemoteTenureCleanupOlderRelayIsFinalNoop(t *testing.T) {
	for _, code := range []int{404, 405, 503} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/sessions/s/forget-tenure", r.URL.Path)
				w.WriteHeader(code)
			}))
			defer server.Close()
			remote := &RemoteRelay{URL: server.URL, Client: server.Client()}
			err := remote.ForgetSessionTenure(context.Background(), sessionstore.Lease{SessionID: "s"})
			if code == 503 {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestTakeoverLossRetainsCleanupForRetry(t *testing.T) {
	p, _, _, r := setup(t)
	retry := &retryTenureRelay{fakeRelay: r, failures: 1}
	p.SetRelay(retry)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	source := p.workers["a"]
	c := p.calls[id]
	c.mu.Lock()
	result := MoveResult{Kind: "takeover", ID: id, Start: time.Now()}
	p.completeTakeover(source, c, lease, &result, true, sessionstore.ErrNotFound)
	c.mu.Unlock()
	require.Equal(t, 1, retry.attempts)
	require.Contains(t, p.calls, id)
	require.EqualValues(t, 1, p.lostCount)
	require.True(t, c.cleanupPending)
	p.reconcileOwnership(ctx)
	require.Equal(t, 1, retry.attempts)
	time.Sleep(300 * time.Millisecond)
	p.reconcileOwnership(ctx)
	require.Equal(t, 2, retry.attempts)
	require.Empty(t, p.calls)
	require.EqualValues(t, 1, p.lostCount)
}

type countedOwnershipReads struct {
	sessionstore.Store
	reads atomic.Int64
}

func (s *countedOwnershipReads) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	s.reads.Add(1)
	return s.Store.Get(ctx, id)
}

func TestOwnershipDetectorThrottlesStoreReads(t *testing.T) {
	p, _, _, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	reads := &countedOwnershipReads{Store: p.store}
	p.store = reads
	p.ttl = 200 * time.Millisecond
	p.config.CheckInterval = 10 * time.Millisecond
	p.config.DeadAfter = time.Minute
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	require.Eventually(t, func() bool { return reads.reads.Load() > 0 }, time.Second, time.Millisecond)
	start := reads.reads.Load()
	time.Sleep(250 * time.Millisecond)
	require.InDelta(t, 2, reads.reads.Load()-start, 1, "ownership sweeps must run at TTL/2, not every detector tick")
}

type unconfirmedEndWorker struct {
	*fakeWorker
	unknown bool
}

func (w *unconfirmedEndWorker) EndSession(id string) error {
	w.mu.Lock()
	delete(w.running, id)
	w.mu.Unlock()
	// Worker closes normally but its store release failed while unavailable.
	if w.unknown {
		return mediaworker.ErrUnknownSession
	}
	return nil
}

func TestUncommittedEndRetainsInventoryUntilOwnershipExpires(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "worker-closed", true: "delete-after-close"}[unknown], func(t *testing.T) {
			p, a, _, _ := setup(t)
			p.workers["a"].worker = &unconfirmedEndWorker{fakeWorker: a, unknown: unknown}
			base := p.store
			if unknown {
				p.store = &failedEndStore{Store: base, releaseErr: sessionstore.ErrTransient}
			}
			ctx := context.Background()
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			c := p.calls[id]
			if !unknown {
				require.NoError(t, p.End(ctx, id), "healthy control must commit the explicit hangup")
				require.True(t, c.hungUp.Load())
				require.NotContains(t, p.calls, id)
				_, err = base.Get(ctx, id)
				require.ErrorIs(t, err, sessionstore.ErrEnded)
				p.reconcileOwnership(ctx)
				require.Zero(t, p.lostCount)
				requireControlLosses(t, planeMetricsBody(p), 0)
				return
			}
			require.Error(t, p.End(ctx, id))
			require.NotNil(t, c, "unconfirmed release must not discard placed inventory")
			require.False(t, c.hungUp.Load())
			require.Zero(t, p.lostCount)
			lease, err := base.Get(ctx, id)
			require.NoError(t, err)
			_, err = base.Renew(ctx, lease, time.Millisecond)
			require.NoError(t, err)
			time.Sleep(5 * time.Millisecond)
			p.store = base
			p.reconcileOwnership(ctx)
			status, err := p.Status(ctx)
			require.NoError(t, err)
			require.Empty(t, status.Calls)
			require.EqualValues(t, 1, status.LostCount)
			require.Equal(t, "ownership_expired", status.Takeovers[0].LossCause)
		})
	}
}

func TestOlderRelayCleanupDoesNotRetainLostCall(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Contains(t, r.URL.Path, "/forget-tenure")
				w.WriteHeader(code)
			}))
			defer server.Close()
			p, _, _, _ := setup(t)
			p.SetRelay(&RemoteRelay{URL: server.URL, Client: server.Client()})
			ctx := context.Background()
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			_, err = p.store.Renew(ctx, lease, time.Millisecond)
			require.NoError(t, err)
			time.Sleep(5 * time.Millisecond)
			p.reconcileOwnership(ctx)
			require.EqualValues(t, 1, p.lostCount)
			require.Empty(t, p.calls, "older relay must not leave an endless cleanup job")
		})
	}
}

type expiresBeforeReleaseStore struct{ sessionstore.Store }

func (s *expiresBeforeReleaseStore) Release(ctx context.Context, lease sessionstore.Lease) error {
	if _, err := s.Renew(ctx, lease, time.Millisecond); err == nil {
		time.Sleep(5 * time.Millisecond)
	}
	return s.Store.Release(ctx, lease)
}

func TestEndNoopReleaseDoesNotHideRacingExpiry(t *testing.T) {
	p, a, _, _ := setup(t)
	p.workers["a"].worker = &unconfirmedEndWorker{fakeWorker: a, unknown: true}
	p.store = &expiresBeforeReleaseStore{Store: p.store}
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	require.ErrorIs(t, p.End(ctx, id), mediaworker.ErrUnknownSession)
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, status.LostCount)
	require.Equal(t, "ownership_expired", status.Takeovers[0].LossCause)
	require.False(t, p.calls[id].hungUp.Load(), "a no-op release is not ended evidence")
}

type endWithUnavailableControlStore struct {
	*unconfirmedEndWorker
	unavailable *failedEndStore
}

func (w *endWithUnavailableControlStore) EndSession(id string) error {
	err := w.unconfirmedEndWorker.EndSession(id)
	w.unavailable.getErr = sessionstore.ErrTransient
	return err
}

func TestExplicitEndWithUnavailableControlStoreRemainsAmbiguous(t *testing.T) {
	p, a, _, _ := setup(t)
	base := p.store
	unavailable := &failedEndStore{Store: base, releaseErr: sessionstore.ErrTransient}
	p.store = unavailable
	p.workers["a"].worker = &endWithUnavailableControlStore{
		unconfirmedEndWorker: &unconfirmedEndWorker{fakeWorker: a}, unavailable: unavailable,
	}
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	require.ErrorIs(t, p.End(ctx, id), sessionstore.ErrTransient)
	c := p.calls[id]
	require.NotNil(t, c)
	require.False(t, c.hungUp.Load())
	lease, err := base.Get(ctx, id)
	require.NoError(t, err)
	_, err = base.Renew(ctx, lease, time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	p.store = base
	p.reconcileOwnership(ctx)
	require.EqualValues(t, 1, p.lostCount)
	require.Equal(t, "ownership_expired", p.recentTakeovers()[0].LossCause)
}
