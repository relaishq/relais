package controlplane

import (
	"context"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
	"time"
)

type outageSweepStore struct {
	sessionstore.Store
	once     sync.Once
	observed chan struct{}
}

func (s *outageSweepStore) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	l, e := s.Store.Get(ctx, id)
	s.once.Do(func() { close(s.observed) })
	return l, e
}

// Expire just after the first sweep. The next sweep must keep the old 1.5s
// cadence, rather than waiting half the new 33s retention lifetime.
func TestOutageRetentionDoesNotSlowOwnershipExpiryDetection(t *testing.T) {
	p, _, _, _ := setup(t)
	require.Equal(t, 33*time.Second, p.ttl)
	p.config.DeadAfter = time.Hour // isolate ownership expiry from worker death
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, _, e := p.Create(ctx, "offer", "a")
	require.NoError(t, e)
	lease, e := p.store.Get(ctx, id)
	require.NoError(t, e)
	watched := &outageSweepStore{Store: p.store, observed: make(chan struct{})}
	p.store = watched
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	select {
	case <-watched.observed:
	case <-time.After(time.Second):
		t.Fatal("first sweep missing")
	}
	// Let the first sweep finish reading the live tenure before expiry.
	time.Sleep(20 * time.Millisecond)
	_, e = p.store.Renew(ctx, lease, time.Millisecond)
	require.NoError(t, e)
	expiredAt := time.Now()
	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.lostCount == 1
	}, 1900*time.Millisecond, 5*time.Millisecond)
	t.Logf("ownership expiry accounted without status polling in %s", time.Since(expiredAt))
}
