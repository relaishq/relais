package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type epochCall struct {
	kind     string
	from, to netip.AddrPort
	epoch    uint64
}

// epochRelay records every route call with the epoch it carried (#443).
type epochRelay struct {
	*fakeRelay
	mu     sync.Mutex
	calls  []epochCall
	refuse func(epochCall) error
}

func (r *epochRelay) record(id string, call epochCall) error {
	r.mu.Lock()
	r.calls = append(r.calls, call)
	refuse := r.refuse
	r.mu.Unlock()
	if refuse != nil {
		if err := refuse(call); err != nil {
			return err
		}
	}
	return r.fakeRelay.MoveSession(id, call.from, call.to)
}
func (r *epochRelay) MoveSession(id string, from, to netip.AddrPort) error {
	return r.record(id, epochCall{"move", from, to, 0})
}
func (r *epochRelay) MoveSessionAtEpoch(id string, from, to netip.AddrPort, epoch uint64) error {
	return r.record(id, epochCall{"epoch", from, to, epoch})
}
func (r *epochRelay) MoveSessionAfterExport(id string, from, to netip.AddrPort, epoch uint64) error {
	return r.record(id, epochCall{"after-export", from, to, epoch})
}
func (r *epochRelay) recorded() []epochCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]epochCall(nil), r.calls...)
}

// staleRefusal is how RemoteRelay returns the relay's typed 409.
func staleRefusal() error {
	return fmt.Errorf("%s: %w", relay.ErrStaleMove.Error(), relay.ErrStaleMove)
}

// #443: every control-plane route move carries the store lease epoch it
// acted on, so the relay can order moves. Each case also characterizes the
// outcome that must not change.
func TestControlMovesCarryStoreLeaseEpoch(t *testing.T) {
	ctx := context.Background()
	t.Run("planned", func(t *testing.T) {
		p, a, b, fake := setup(t)
		routes := &epochRelay{fakeRelay: fake}
		p.relay = routes
		id, _, err := p.Create(ctx, "offer", "a")
		require.NoError(t, err)
		_, err = p.Move(ctx, id, "b")
		require.NoError(t, err)
		require.True(t, b.runs(id))
		require.False(t, a.runs(id))
		lease, err := p.store.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, []epochCall{{"after-export", a.addr, b.addr, lease.Epoch}}, routes.recorded())
	})
	t.Run("rollback after a failed resume", func(t *testing.T) {
		p, a, b, fake := setup(t)
		routes := &epochRelay{fakeRelay: fake}
		p.relay = routes
		id, _, err := p.Create(ctx, "offer", "a")
		require.NoError(t, err)
		b.resumeError = true
		res, err := p.Move(ctx, id, "b")
		require.ErrorContains(t, err, "rolled back")
		require.True(t, res.Result.RolledBack)
		require.True(t, a.runs(id), "rollback restores the source")
		lease, err := p.store.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, a.addr, lease.Worker)
		require.Equal(t, []epochCall{{"after-export", a.addr, b.addr, lease.Epoch - 1}, {"epoch", b.addr, a.addr, lease.Epoch}}, routes.recorded())
	})
	t.Run("uncertain rollback is kept for recovery", func(t *testing.T) {
		p, a, b, fake := setup(t)
		forward := true
		routes := &epochRelay{fakeRelay: fake, refuse: func(call epochCall) error {
			if forward && call.to == b.addr {
				forward = false
				return errors.New("route reply lost")
			}
			return nil
		}}
		p.relay = routes
		id, _, err := p.Create(ctx, "offer", "a")
		require.NoError(t, err)
		p.store = &reverseReplyLostStore{retainedMoveStore: &retainedMoveStore{Store: p.store}, source: a.addr}
		_, err = p.Move(ctx, id, "b")
		require.Error(t, err)
		source := p.workers["a"]
		require.NotNil(t, pendingRecovery(p, source, id), "uncertain reverse transfer stays with recovery")
		settleRecovery(t, p, source, id)
		require.True(t, a.runs(id))
		lease, err := p.store.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, a.addr, lease.Worker)
		calls := routes.recorded()
		require.GreaterOrEqual(t, len(calls), 2)
		for _, call := range calls {
			require.NotZero(t, call.epoch, "%+v", call)
		}
		require.Equal(t, epochCall{"epoch", netip.AddrPort{}, a.addr, lease.Epoch}, calls[len(calls)-1], "an uncertain route is repaired at the adopted epoch")
	})
	t.Run("restart repair", func(t *testing.T) {
		p, a, _, fake := setup(t)
		id, _, err := p.Create(ctx, "offer", "a")
		require.NoError(t, err)
		lease, err := p.store.Get(ctx, id)
		require.NoError(t, err)
		authorizeTestControl(t, p)
		routes := &epochRelay{fakeRelay: fake}
		p.SetRelay(routes)
		intent := sessionstore.ControlIntent{ID: id, Token: "interrupted", Kind: "move", Lease: lease, Started: time.Now()}
		require.NoError(t, p.restart.store.PutControlIntent(ctx, intent))
		installed := mediaworker.InstalledSession{ID: id, Owner: lease.Worker, Epoch: lease.Epoch}
		require.NoError(t, p.recovery.rebuild(ctx, intent, map[string]mediaworker.InstalledSession{id + "/" + fmt.Sprint(lease.Epoch) + "/" + lease.Worker.String(): installed}))
		require.True(t, a.runs(id))
		require.Equal(t, []epochCall{{"epoch", netip.AddrPort{}, a.addr, lease.Epoch}}, routes.recorded(), "repair still re-points every route, at the store epoch")
	})
}

// #443: a stale-move refusal is definite. The route did not change, so a
// planned move rolls back without a reverse route call, and nothing is left
// for uncertain recovery.
func TestControlStaleMoveIsDefiniteRefusal(t *testing.T) {
	ctx := context.Background()
	t.Run("planned move", func(t *testing.T) {
		p, a, b, fake := setup(t)
		routes := &epochRelay{fakeRelay: fake, refuse: func(call epochCall) error {
			if call.to == b.addr {
				return staleRefusal()
			}
			return nil
		}}
		p.relay = routes
		id, _, err := p.Create(ctx, "offer", "a")
		require.NoError(t, err)
		res, err := p.Move(ctx, id, "b")
		require.ErrorIs(t, err, relay.ErrStaleMove)
		require.NotErrorIs(t, err, privateapi.ErrUncertain)
		require.True(t, res.Result.RolledBack)
		require.True(t, a.runs(id))
		require.False(t, b.runs(id))
		require.Nil(t, pendingRecovery(p, p.workers["a"], id))
		lease, err := p.store.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, a.addr, lease.Worker)
		require.Len(t, routes.recorded(), 1, "the refused route never moved: no reverse route call")
	})
	t.Run("rollback route", func(t *testing.T) {
		p, a, b, fake := setup(t)
		routes := &epochRelay{fakeRelay: fake, refuse: func(call epochCall) error {
			if call.to == a.addr {
				return staleRefusal()
			}
			return nil
		}}
		p.relay = routes
		id, _, err := p.Create(ctx, "offer", "a")
		require.NoError(t, err)
		b.resumeError = true
		// Through the operator move route: the serialized codes must name the
		// refusal and the rolled-back move must not read as uncertain.
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		var res MoveResult
		err = privateapi.DoResult(ctx, server.Client(), server.URL, http.MethodPost, "/calls/"+id+"/move?to=b", nil, &res, RemoteErrors)
		require.ErrorContains(t, err, "relay restore failed")
		require.ErrorContains(t, err, "injected resume failure", "the move failure cause is kept")
		require.ErrorIs(t, err, relay.ErrStaleMove)
		require.NotErrorIs(t, err, privateapi.ErrUncertain)
		require.True(t, res.Result.RolledBack)
		require.True(t, a.runs(id))
		require.Nil(t, pendingRecovery(p, p.workers["a"], id))
		require.Len(t, routes.recorded(), 2)
	})
}

// #443 review: only the stale-move sentinel crosses the rollback error. A
// route error that also carries uncertainty must not make the rolled-back
// move uncertain, and stale_move is still reported.
func TestControlRollbackStaleMoveIsolatesRouteError(t *testing.T) {
	p, a, b, fake := setup(t)
	routes := &epochRelay{fakeRelay: fake, refuse: func(call epochCall) error {
		if call.to == a.addr {
			return errors.Join(staleRefusal(), privateapi.Uncertain(errors.New("reply lost")))
		}
		return nil
	}}
	p.relay = routes
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	b.resumeError = true
	res, err := p.Move(context.Background(), id, "b")
	require.True(t, res.Result.RolledBack)
	require.ErrorIs(t, err, relay.ErrStaleMove)
	require.False(t, errors.Is(err, privateapi.ErrUncertain))
	codes := privateapi.ErrorCodes(err, RemoteErrors)
	require.Contains(t, codes, "stale_move")
	require.NotContains(t, codes, "uncertain")
	require.True(t, a.runs(id))
}

// #443: RemoteRelay maps the relay's 409 stale-move code to the typed error
// on both move routes; move-after-export does not fall back to /move.
func TestRemoteRelayStaleMoveIsTyped(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		paths = append(paths, req.URL.Path[strings.LastIndex(req.URL.Path, "/"):])
		mu.Unlock()
		privateapi.Error(w, relay.ErrStaleMove, relay.RemoteErrors)
	}))
	defer server.Close()
	remote := &RemoteRelay{URL: server.URL, Client: server.Client()}
	from, to := netip.MustParseAddrPort("127.0.0.1:1"), netip.MustParseAddrPort("127.0.0.1:2")
	for _, err := range []error{
		remote.MoveSessionAtEpoch("call", from, to, 4),
		remote.MoveSessionAtEpoch("call", netip.AddrPort{}, to, 4),
		remote.MoveSessionAfterExport("call", from, to, 4),
	} {
		require.ErrorIs(t, err, relay.ErrStaleMove)
		require.NotErrorIs(t, err, privateapi.ErrUncertain)
	}
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"/move", "/move", "/move-after-export"}, paths)
}
