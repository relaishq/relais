package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// The fake protocol boundary accepts takeover options, but still enforces
// the real store's lease check before it can run anything.
type takeoverWorker struct {
	*fakeWorker
	resumeFail bool
	margin     uint16
	rtcpMargin uint32
}

func (w *takeoverWorker) ResumeSession(data []byte, opts mediaworker.ResumeOptions) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.margin, w.rtcpMargin = opts.SequenceMargin, opts.SRTCPIndexMargin
	if w.resumeFail {
		return "", errors.New("injected takeover resume failure")
	}
	if _, err := w.store.Renew(context.Background(), opts.Lease, time.Minute); err != nil {
		return "", err
	}
	var snap retrySnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return "", err
	}
	w.running[snap.State.ID] = true
	return snap.State.ID, nil
}

func TestTakeoverFailuresReleaseLeaseAndRecordLoss(t *testing.T) {
	for _, failure := range []string{"none", "no-snapshot", "resume", "no-target"} {
		t.Run(failure, func(t *testing.T) {
			p, _, baseB, _ := setup(t)
			b := &takeoverWorker{fakeWorker: baseB, resumeFail: failure == "resume"}
			p.workers["b"].worker = b
			ctx := context.Background()
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			if failure != "no-snapshot" {
				require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
			}
			if failure == "no-target" {
				p.workers["b"].draining = true
			}
			p.workers["a"].dead = true
			p.takeover(ctx, p.workers["a"], lease, time.Now())
			status, err := p.Status(ctx)
			require.NoError(t, err)
			require.Len(t, status.Takeovers, 1)
			res := status.Takeovers[0]
			require.Equal(t, "takeover", res.Kind)
			if failure == "none" {
				require.False(t, res.Lost)
				require.True(t, b.runs(id))
				require.EqualValues(t, 8192, b.margin)
				require.EqualValues(t, 128, b.rtcpMargin)
				require.Equal(t, "b", status.Calls[0].Owner)
				require.EqualValues(t, 1, status.Calls[0].TakeoverCount)
				require.Equal(t, "takeover", status.Calls[0].LastMoveKind)
				require.Zero(t, status.LostCount)
				require.NoError(t, p.End(ctx, id))
			} else {
				require.True(t, res.Lost)
				require.NotEmpty(t, res.Error)
				require.False(t, b.runs(id))
				require.Empty(t, status.Calls)
				require.EqualValues(t, 1, status.LostCount)
				_, err := p.store.Get(ctx, id)
				require.ErrorIs(t, err, sessionstore.ErrNotFound)
			}
			_, err = p.store.GetState(ctx, id)
			require.ErrorIs(t, err, sessionstore.ErrNotFound)
		})
	}
}

type flakyListStore struct {
	sessionstore.Store
	fail atomic.Bool
}

func (s *flakyListStore) ListByWorker(ctx context.Context, workerAddr netip.AddrPort) ([]sessionstore.Lease, error) {
	if s.fail.Load() {
		return nil, errors.New("store temporarily unavailable")
	}
	return s.Store.ListByWorker(ctx, workerAddr)
}

// Failed lease enumeration must not let a returning worker rejoin with its
// original leases. Detection retries while the registration remains dead.
func TestHeartbeatRecoveryRetriesFailedEnumeration(t *testing.T) {
	p, a, _, _ := setup(t)
	backing := &flakyListStore{Store: p.store}
	backing.fail.Store(true)
	p.store = backing
	p.config.DeadAfter = 20 * time.Millisecond
	p.config.CheckInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	require.Eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.workers["a"].dead }, time.Second, time.Millisecond)
	require.Error(t, p.Heartbeat(a.addr), "cannot rejoin until leases enumerated")
	backing.fail.Store(false)
	require.Eventually(t, func() bool { return p.Heartbeat(a.addr) == nil }, time.Second, time.Millisecond)
}

func TestTakeoverRecoversMissingCallAndWaitsForIncoming(t *testing.T) {
	p, a, baseB, _ := setup(t)
	p.workers["b"].worker = &takeoverWorker{fakeWorker: baseB}
	ctx := context.Background()
	lease, err := p.store.Claim(ctx, "orphan", a.addr, time.Minute)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, "orphan", 0)))
	source := p.workers["a"]
	source.dead, source.reserved = true, 1
	require.False(t, p.recoverWorker(ctx, source, time.Now()), "in-flight create must settle before enumeration")
	_, err = p.store.Get(ctx, "orphan")
	require.NoError(t, err)
	source.reserved = 0
	require.True(t, p.recoverWorker(ctx, source, time.Now()))
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.Zero(t, status.LostCount)
	require.Len(t, status.Takeovers, 1)
	require.False(t, status.Takeovers[0].Lost)
	require.Equal(t, "b", status.Calls[0].Owner)
	_, err = p.store.GetState(ctx, "orphan")
	require.NoError(t, err)
}
