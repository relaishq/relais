package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Public status and metrics must retain one terminal outcome even when the
// authoritative ownership inventory no longer contains the placed call.
func TestOwnershipExpiredCountedOnce(t *testing.T) {
	p, _, _, _ := setup(t)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	_, err = p.store.Renew(ctx, lease, time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { _, _ = p.Status(ctx) })
	}
	wg.Wait()
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.Empty(t, status.Calls)
	require.EqualValues(t, 1, status.LostCount)
	require.Len(t, status.Takeovers, 1)
	require.True(t, status.Takeovers[0].Lost)
	b, err := json.Marshal(status.Takeovers[0])
	require.NoError(t, err)
	require.Contains(t, string(b), `"loss_cause":"ownership_expired"`)
	require.Contains(t, planeMetricsBody(p), `relais_control_losses_total{cause="ownership_expired"} 1`)
	_, err = p.store.GetState(ctx, id)
	require.ErrorIs(t, err, sessionstore.ErrNotFound)
}

func TestOwnershipExpiredDetectorWithoutStatusPoll(t *testing.T) {
	p, _, _, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	require.NoError(t, p.store.Release(ctx, lease))
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	require.Eventually(t, func() bool {
		r := httptest.NewRecorder()
		p.ProcessHandler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return r.Code == http.StatusOK && containsExpiredLoss(r.Body.String())
	}, time.Second, 10*time.Millisecond)
}

func containsExpiredLoss(body string) bool {
	return strings.Contains(body, `relais_control_losses_total{cause="ownership_expired"} 1`)
}

type vanishedStatusStore struct {
	sessionstore.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *vanishedStatusStore) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	lease, err := s.Store.Get(ctx, id)
	if errors.Is(err, sessionstore.ErrNotFound) {
		s.once.Do(func() {
			close(s.entered)
			select {
			case <-s.release:
			case <-ctx.Done():
			}
		})
	}
	return lease, err
}

func TestOwnershipExpiredStaleStatusPreservesSuccessor(t *testing.T) {
	p, a, b, _ := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	old, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	_, err = p.store.Renew(ctx, old, time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	base := p.store
	gated := &vanishedStatusStore{Store: base, entered: make(chan struct{}), release: make(chan struct{})}
	p.store = gated
	done := make(chan error, 1)
	go func() { _, err := p.Status(ctx); done <- err }()
	<-gated.entered
	next, err := base.Claim(ctx, id, b.addr, time.Minute)
	require.NoError(t, err)
	next, err = base.Transfer(ctx, next, a.addr, time.Minute)
	require.NoError(t, err)
	require.NoError(t, base.PutState(ctx, next, []byte("new tenure")))
	close(gated.release)
	require.NoError(t, <-done)
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Calls, 1)
	require.Equal(t, next.Epoch, status.Calls[0].Epoch)
	require.Zero(t, status.LostCount)
	require.Empty(t, status.Takeovers)
	got, err := base.GetState(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "new tenure", string(got))
}

func TestOwnershipExpiredEndCountsLoss(t *testing.T) {
	p, _, _, _ := setup(t)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	old, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	_, err = p.store.Renew(ctx, old, time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	require.ErrorIs(t, p.End(ctx, id), sessionstore.ErrNotFound)
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, status.LostCount)
	require.Len(t, status.Takeovers, 1)
}

type fenceDuringEndWorker struct{ *fakeWorker }

func (w *fenceDuringEndWorker) EndSession(id string) error {
	lease, err := w.store.Get(context.Background(), id)
	if err != nil {
		return err
	}
	if err := w.store.Release(context.Background(), lease); err != nil {
		return err
	}
	return mediaworker.ErrUnknownSession
}
func TestOwnershipExpiredFenceDuringEndCounted(t *testing.T) {
	p, a, _, _ := setup(t)
	p.workers["a"].worker = &fenceDuringEndWorker{a}
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	require.ErrorIs(t, p.End(ctx, id), mediaworker.ErrUnknownSession)
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, status.LostCount)
	require.Len(t, status.Takeovers, 1)
}

type ownershipReadFault struct {
	sessionstore.Store
	fail bool
}

func (s *ownershipReadFault) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	if s.fail {
		return sessionstore.Lease{}, sessionstore.ErrTransient
	}
	return s.Store.Get(ctx, id)
}

type placedLeaseWorker struct{ *fakeWorker }

func (w *placedLeaseWorker) CreateSessionWithLease(ctx context.Context, offer string) (string, string, sessionstore.Lease, error) {
	id, answer, err := w.CreateSession(ctx, offer)
	if err != nil {
		return id, answer, sessionstore.Lease{}, err
	}
	lease, err := w.store.Get(ctx, id)
	return id, answer, lease, err
}

type recordingTenureRelay struct {
	*fakeRelay
	cleanup sessionstore.Lease
}

func (r *recordingTenureRelay) ForgetSessionTenure(_ context.Context, lease sessionstore.Lease) error {
	r.cleanup = lease
	return nil
}

// Successful placement carries its fenced token even if control's first
// ownership read fails and the store stays unavailable through lease expiry.
func TestOwnershipExpiredPlacementRetainsLeaseAcrossReadFailure(t *testing.T) {
	p, a, _, r := setup(t)
	p.workers["a"].worker = &placedLeaseWorker{a}
	routed := &recordingTenureRelay{fakeRelay: r}
	p.SetRelay(routed)
	fault := &ownershipReadFault{Store: p.store, fail: true}
	p.store = fault
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	old, err := a.store.Get(ctx, id)
	require.NoError(t, err)
	_, err = a.store.Renew(ctx, old, time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	fault.fail = false
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, status.LostCount)
	require.Equal(t, old.Worker, routed.cleanup.Worker)
	require.Equal(t, old.Epoch, routed.cleanup.Epoch)
}

type retryTenureRelay struct {
	*fakeRelay
	failures int
	attempts int
}

func (r *retryTenureRelay) ForgetSessionTenure(_ context.Context, _ sessionstore.Lease) error {
	r.attempts++
	if r.attempts <= r.failures {
		return sessionstore.ErrTransient
	}
	return nil
}
func TestOwnershipExpiredLossCountedBeforeCleanupRetry(t *testing.T) {
	p, _, _, r := setup(t)
	retry := &retryTenureRelay{fakeRelay: r, failures: 1}
	p.SetRelay(retry)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	_, err = p.store.Renew(ctx, lease, time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, status.LostCount)
	require.Len(t, status.Takeovers, 1)
	require.Empty(t, status.Calls)
	require.Contains(t, planeMetricsBody(p), `relais_control_losses_total{cause="ownership_expired"} 1`)
	status, err = p.Status(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, status.LostCount)
	require.Len(t, status.Takeovers, 1)
	require.Equal(t, 2, retry.attempts)
}
