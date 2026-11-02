package controlplane

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type terminalTenureRelay struct {
	*fakeRelay
	forgottenTenures []sessionstore.Lease
}

func (r *terminalTenureRelay) ForgetSessionTenure(_ context.Context, lease sessionstore.Lease) error {
	r.forgottenTenures = append(r.forgottenTenures, lease)
	return nil
}

func finishTerminalPending(t *testing.T, p *Plane, source *registration, c *call, lease sessionstore.Lease, path string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if path == "continue" {
		require.Nil(t, p.recovery.continueLocked(context.Background(), source, c, lease, time.Now()))
	} else {
		res := MoveResult{Kind: "takeover", ID: c.id, Start: time.Now()}
		p.recovery.complete(source, c, lease, &res, true, sessionstore.ErrNotFound)
	}
	require.Empty(t, recoveryWork(p, source))
	require.EqualValues(t, 1, p.lostCount, "pending completion must not count a second loss")
}

func TestTerminalPendingTakeoverForgetsRoutedTenure(t *testing.T) {
	for _, path := range []string{"continue", "complete"} {
		for _, cleanup := range []string{"before", "after"} {
			t.Run(path+"/cleanup-"+cleanup, func(t *testing.T) {
				ctx := context.Background()
				p, _, _, baseRelay := setup(t)
				r := &terminalTenureRelay{fakeRelay: baseRelay}
				p.SetRelay(r)
				id, _, err := p.Create(ctx, "offer", "a")
				require.NoError(t, err)
				routed, err := p.store.Get(ctx, id)
				require.NoError(t, err)
				require.NoError(t, p.store.PutState(ctx, routed, takeoverSnapshot(t, id, 0)))
				source := p.workers["a"]
				expireObservedWorker(t, source)
				baseRelay.fail = true
				require.False(t, p.recoverWorker(ctx, source, time.Now()))
				pending := p.recovery.pending[source][id]
				require.NotNil(t, pending)
				require.Equal(t, routed, pending.routedLease)
				require.False(t, sameTenure(routed, pending.lease))
				c := pending.call
				c.mu.Lock()
				p.endRelayFailure(c, pending.lease, "relay_lost")
				if cleanup == "before" {
					// A reconciliation job can snapshot the call before recovery
					// becomes pending, then acquire its lock after terminal loss.
					p.cleanupExpired(ctx, c)
				}
				c.mu.Unlock()
				finishTerminalPending(t, p, source, c, pending.lease, path)
				p.reconcileOwnership(ctx)
				require.Contains(t, r.forgottenTenures, routed, "cleanup must forget the route's original tenure")
				require.NotContains(t, p.calls, id)
				_, err = p.store.Get(ctx, id)
				require.ErrorIs(t, err, sessionstore.ErrEnded)
			})
		}
	}
}

func TestTerminalPendingMoveReleasesLegacyHold(t *testing.T) {
	for _, path := range []string{"continue", "complete"} {
		for _, cleanup := range []string{"before", "after"} {
			for _, state := range []string{"held", "planned"} {
				t.Run(path+"/cleanup-"+cleanup+"/"+state, func(t *testing.T) {
					ctx := context.Background()
					p, _, _, baseRelay := setup(t)
					r := &trackedHoldRelay{fakeRelay: baseRelay}
					p.SetRelay(r)
					id, _, err := p.Create(ctx, "offer", "a")
					require.NoError(t, err)
					source := p.workers["a"]
					if state == "held" {
						queueUncertainExport(t, p, id)
					} else {
						base := p.store
						p.store = &retainedMoveStore{Store: base}
						_, err = p.Move(ctx, id, "b")
						require.Error(t, err)
						p.store = base
					}
					pending := p.recovery.pending[source][id]
					require.NotNil(t, pending)
					require.Equal(t, state == "held", pending.held)
					require.Equal(t, state == "planned", pending.planned)
					require.Empty(t, r.releases)
					lease, err := p.store.Get(ctx, id)
					require.NoError(t, err)
					c := pending.call
					c.mu.Lock()
					p.endRelayFailure(c, lease, "relay_lost")
					if cleanup == "before" {
						p.cleanupExpired(ctx, c)
					}
					c.mu.Unlock()
					finishTerminalPending(t, p, source, c, lease, path)
					p.reconcileOwnership(ctx)
					require.Equal(t, []netip.AddrPort{{}}, r.releases, "terminal cleanup must release the legacy hold")
					require.Equal(t, []string{id}, baseRelay.forgotten)
					require.NotContains(t, p.calls, id)
				})
			}
		}
	}
}
