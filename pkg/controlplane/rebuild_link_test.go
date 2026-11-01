package controlplane

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestRebuildWaitsForLiveTargetCommandLink(t *testing.T) {
	for _, mode := range []string{"returns", "deadline", "canceled-retry"} {
		t.Run(mode, func(t *testing.T) {
			p, a, b, _ := setup(t)
			id, _, err := p.Create(context.Background(), "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(context.Background(), id)
			require.NoError(t, err)
			require.NoError(t, p.store.PutState(context.Background(), lease, takeoverSnapshot(t, id, 0)))
			_, err = a.ExportSession(id)
			require.NoError(t, err)
			authorizeLongControl(t, p)
			links := p.ConfigureDialOut([]string{"b"}, nil)
			t.Cleanup(links.Close)
			p.workers["b"].worker = &takeoverWorker{fakeWorker: b}
			p.workers["b"].observation.linkDown = true
			now := time.Now()
			p.workers["b"].observation.recordHeartbeat(now)
			p.recovery.targetNow = func() time.Time { return now }
			waits := 0
			p.recovery.targetWait = func(context.Context, <-chan struct{}, time.Duration) error {
				waits++
				require.LessOrEqual(t, waits, 31, "link wait renewed its work budget")
				now = now.Add(50 * time.Millisecond)
				p.mu.Lock()
				p.workers["b"].observation.recordHeartbeat(now)
				if mode == "returns" && waits == 4 {
					p.workers["b"].observation.linkDown = false
					p.observationChangedLocked()
				}
				p.mu.Unlock()
				if mode == "canceled-retry" {
					return context.Canceled
				}
				return nil
			}
			intent := sessionstore.ControlIntent{ID: id, Token: "interrupted-export", Kind: "move", Started: now, Lease: lease}
			require.NoError(t, p.restart.store.PutControlIntent(context.Background(), intent))
			if mode == "canceled-retry" {
				require.ErrorIs(t, p.recovery.rebuild(context.Background(), intent, nil), ErrNotReady)
				require.Zero(t, p.lostCount)
				now = now.Add(2 * time.Second)
				p.workers["b"].observation.recordHeartbeat(now)
				p.recovery.targetWait = func(context.Context, <-chan struct{}, time.Duration) error {
					t.Fatal("bootstrap retry renewed the link wait budget")
					return context.Canceled
				}
			}
			require.NoError(t, p.recovery.rebuild(context.Background(), intent, nil))
			require.Positive(t, waits, "bootstrap treated a live reconnecting worker as a failed target")
			state, err := p.Status(context.Background())
			require.NoError(t, err)
			if mode == "returns" {
				require.True(t, b.runs(id))
				require.Zero(t, state.LostCount)
				require.Len(t, state.Calls, 1)
			} else {
				require.False(t, b.runs(id))
				require.EqualValues(t, 1, state.LostCount)
				require.Empty(t, state.Calls)
			}
		})
	}
}

func TestConfirmedRebuildReleasesOldSourceRegistration(t *testing.T) {
	p, _, _, _ := setup(t)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "b")
	require.NoError(t, err)
	old, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	_, err = p.Move(ctx, id, "a")
	require.NoError(t, err)
	current, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	authorizeLongControl(t, p)
	intent := sessionstore.ControlIntent{ID: id, Token: "retained", Kind: "move", Lease: old, Started: time.Now()}
	require.NoError(t, p.restart.store.PutControlIntent(ctx, intent))
	source := p.workers["b"]
	p.recovery.pending[source] = map[string]*moveTransfer{id: {call: p.calls[id], lease: current, rebuilding: true}}
	installed := mediaworker.InstalledSession{ID: id, Owner: current.Worker, Epoch: current.Epoch}
	require.NoError(t, p.recovery.rebuild(ctx, intent, map[string]mediaworker.InstalledSession{id + "/" + fmt.Sprint(current.Epoch) + "/" + current.Worker.String(): installed}))
	require.NoError(t, p.MarkDraining("b"))
	require.NoError(t, p.readdress(ctx, "b", source, netip.MustParseAddrPort("127.0.0.1:17003")), "confirmed recovery must not leave old pointer-keyed work blocking readdress")
}

func TestRebuildWaitsForTargetHeartbeatAfterCommandLink(t *testing.T) {
	state, waits, b, id := rebuildAwaitingHeartbeat(t, 12)
	require.Zero(t, state.LostCount, "a connected target awaiting its first heartbeat can still recover the exported call")
	require.Equal(t, 12, waits)
	require.True(t, b.runs(id))
	require.Len(t, state.Calls, 1)
}

func TestRebuildTargetHeartbeatWaitEndsAtDeadline(t *testing.T) {
	state, waits, b, id := rebuildAwaitingHeartbeat(t, 0)
	require.EqualValues(t, 1, state.LostCount, "a target that never heartbeats must not hold rebuild past its deadline")
	require.Equal(t, int(takeoverBudget/rebuildTick), waits)
	require.False(t, b.runs(id))
	require.Empty(t, state.Calls)
}

const rebuildTick = 50 * time.Millisecond

// rebuildAwaitingHeartbeat rebuilds an interrupted export onto a dial-out
// target whose link connects at once; it heartbeats on wait heartbeatAt
// (never when 0). Each wait advances the fake clock by rebuildTick.
func rebuildAwaitingHeartbeat(t *testing.T, heartbeatAt int) (Status, int, *fakeWorker, string) {
	t.Helper()
	p, a, b, _ := setup(t)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
	_, err = a.ExportSession(id)
	require.NoError(t, err)
	authorizeLongControl(t, p)
	links := p.ConfigureDialOut([]string{"b"}, nil)
	t.Cleanup(links.Close)
	target := p.workers["b"]
	target.worker = &takeoverWorker{fakeWorker: b}
	target.observation.linkDown = true
	target.observation.observeStatus(mediaworker.WorkerStatus{}, true, true)
	now := time.Now()
	target.observation.recordHeartbeat(now)
	p.recovery.targetNow = func() time.Time { return now }
	waits := 0
	p.recovery.targetWait = func(context.Context, <-chan struct{}, time.Duration) error {
		waits++
		require.LessOrEqual(t, waits, 100)
		now = now.Add(rebuildTick)
		// Admission and the separate heartbeat can complete in different ticks.
		p.mu.Lock()
		target.observation.linkDown = false
		p.observationChangedLocked()
		p.mu.Unlock()
		if waits == heartbeatAt {
			_, err := p.remoteHeartbeat("b", HeartbeatRequest{Address: b.addr})
			require.NoError(t, err)
		}
		return nil
	}
	intent := sessionstore.ControlIntent{ID: id, Token: "interrupted-export", Kind: "move", Started: now, Lease: lease}
	require.NoError(t, p.restart.store.PutControlIntent(ctx, intent))
	require.NoError(t, p.recovery.rebuild(ctx, intent, nil))
	state, err := p.Status(ctx)
	require.NoError(t, err)
	return state, waits, b, id
}
