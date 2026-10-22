package controlplane

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/relay"

	"github.com/relaishq/relais/pkg/framecache"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestTerminalCallDeletesFrames(t *testing.T) {
	for _, outcome := range []string{"hangup", "lost-takeover", "successful-takeover"} {
		t.Run(outcome, func(t *testing.T) {
			p, _, b, _ := setup(t)
			p.workers["b"].worker = &takeoverWorker{fakeWorker: b}
			cache := framecache.NewMemory(framecache.Limits{})
			p.config.FrameCache = cache
			ctx := context.Background()
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			frame := framecache.Frame{Track: framecache.Track{Kind: "video", SSRC: 1}, Keyframe: true, Packets: []framecache.Packet{{Marker: true, Payload: []byte{0}}}}
			require.NoError(t, cache.Append(ctx, id, frame))
			if outcome == "hangup" {
				require.NoError(t, p.End(ctx, id))
			} else {
				lease, err := p.store.Get(ctx, id)
				require.NoError(t, err)
				if outcome == "successful-takeover" {
					require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
				}
				expireObservedWorker(t, p.workers["a"])
				p.recovery.recover(ctx, p.workers["a"], lease, time.Now())
				require.Len(t, p.recentTakeovers(), 1)
				require.Equal(t, outcome == "lost-takeover", p.recentTakeovers()[0].Lost)
			}
			frames, err := cache.Current(ctx, id, frame.Track)
			require.NoError(t, err)
			if outcome == "successful-takeover" {
				require.Len(t, frames, 1)
			} else {
				require.Empty(t, frames)
			}
		})
	}
}

func TestUncertainRollbackCheckpointLossUsesTerminalCleanup(t *testing.T) {
	p, a, b, r := setup(t)
	p.workers["a"].worker = &exportingWorker{fakeWorker: a}
	cache := framecache.NewMemory(framecache.Limits{})
	p.config.FrameCache = cache
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	frame := framecache.Frame{Track: framecache.Track{Kind: "video", SSRC: 1}, Keyframe: true, Packets: []framecache.Packet{{Marker: true, Payload: []byte{0}}}}
	require.NoError(t, cache.Append(ctx, id, frame))
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	state := takeoverSnapshot(t, id, 0)
	require.NoError(t, p.store.PutState(ctx, lease, state))
	p.workers["b"].worker = &exportingWorker{fakeWorker: b, uncertainResume: true}
	now := time.Now()
	p.store = &checkpointClockStore{Store: p.store, checkpoint: sessionstore.Checkpoint{Now: now, StoredAt: now.Add(-4 * time.Second), Age: 4 * time.Second}}
	_, err = p.Move(ctx, id, "b")
	require.ErrorIs(t, err, mediaworker.ErrSequenceBudgetExhausted)
	require.Empty(t, p.calls)
	require.Equal(t, []string{id}, r.forgotten)
	frames, err := cache.Current(ctx, id, frame.Track)
	require.NoError(t, err)
	require.Empty(t, frames)
	_, err = p.store.Get(ctx, id)
	require.ErrorIs(t, err, sessionstore.ErrEnded)
	require.EqualValues(t, 1, p.lostCount)
	require.Len(t, p.recentTakeovers(), 1)
	require.Equal(t, "definitive-loss", p.recentTakeovers()[0].CheckpointPolicy)
	require.True(t, p.recentTakeovers()[0].Lost)
}

type lossHoldRelay struct {
	*fakeRelay
	releases int
}

func (r *lossHoldRelay) ReleaseSession(_ string, _ netip.AddrPort) (int, error) {
	r.releases++
	if r.releases > 1 {
		return 0, relay.ErrHoldExpired
	}
	return 0, nil
}

func TestRollbackLossCleansRouteFramesAndRecordsEvent(t *testing.T) {
	p, a, b, r := setup(t)
	a.resumeError, b.resumeError = true, true
	holds := &lossHoldRelay{fakeRelay: r}
	p.relay = holds
	cache := framecache.NewMemory(framecache.Limits{})
	p.config.FrameCache = cache
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	frame := framecache.Frame{Track: framecache.Track{Kind: "video", SSRC: 1}, Keyframe: true, Packets: []framecache.Packet{{Marker: true, Payload: []byte{0}}}}
	require.NoError(t, cache.Append(ctx, id, frame))
	res, err := p.Move(ctx, id, "b")
	require.True(t, res.Lost)
	require.False(t, res.Result.HoldExpired)
	require.NotContains(t, res.Error, "release caller hold")
	require.Equal(t, 1, holds.releases, "loss cleanup owns the hold release")
	require.ErrorContains(t, err, "call lost")
	require.Empty(t, p.calls)
	require.Equal(t, []string{id}, r.forgotten)
	frames, err := cache.Current(ctx, id, frame.Track)
	require.NoError(t, err)
	require.Empty(t, frames)
	_, err = p.store.Get(ctx, id)
	require.ErrorIs(t, err, sessionstore.ErrEnded)
	require.EqualValues(t, 1, p.lostCount)
	require.Len(t, p.recentTakeovers(), 1)
	require.True(t, p.recentTakeovers()[0].Lost)
	require.Equal(t, "move", p.recentTakeovers()[0].Kind)
}
