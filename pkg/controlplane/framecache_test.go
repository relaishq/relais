package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/relais/pkg/framecache"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
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
				p.workers["a"].dead = true
				p.takeover(ctx, p.workers["a"], lease, time.Now())
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
	p, _, _, r := setup(t)
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
	lease, err = p.store.Transfer(ctx, lease, p.workers["b"].addr, time.Minute)
	require.NoError(t, err)
	now := time.Now()
	p.store = &checkpointClockStore{Store: p.store, checkpoint: sessionstore.Checkpoint{Now: now, StoredAt: now.Add(-4 * time.Second), Age: 4 * time.Second}}
	var result mediaworker.HandoverResult
	err = p.rollback(p.calls[id], p.workers["a"], p.workers["b"], r, state, lease, true, true, true, &result, mediaworker.ErrClosed, now)
	require.ErrorIs(t, err, mediaworker.ErrSequenceBudgetExhausted)
	require.Empty(t, p.calls)
	require.Equal(t, []string{id}, r.forgotten)
	frames, err := cache.Current(ctx, id, frame.Track)
	require.NoError(t, err)
	require.Empty(t, frames)
	_, err = p.store.Get(ctx, id)
	require.ErrorIs(t, err, sessionstore.ErrNotFound)
	require.EqualValues(t, 1, p.lostCount)
	require.Len(t, p.recentTakeovers(), 1)
	require.Equal(t, "definitive-loss", p.recentTakeovers()[0].CheckpointPolicy)
	require.True(t, p.recentTakeovers()[0].Lost)
}

func TestRollbackLossCleansRouteFramesAndRecordsEvent(t *testing.T) {
	p, a, b, r := setup(t)
	a.resumeError, b.resumeError = true, true
	cache := framecache.NewMemory(framecache.Limits{})
	p.config.FrameCache = cache
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	frame := framecache.Frame{Track: framecache.Track{Kind: "video", SSRC: 1}, Keyframe: true, Packets: []framecache.Packet{{Marker: true, Payload: []byte{0}}}}
	require.NoError(t, cache.Append(ctx, id, frame))
	_, err = p.Move(ctx, id, "b")
	require.ErrorContains(t, err, "call lost")
	require.Empty(t, p.calls)
	require.Equal(t, []string{id}, r.forgotten)
	frames, err := cache.Current(ctx, id, frame.Track)
	require.NoError(t, err)
	require.Empty(t, frames)
	_, err = p.store.Get(ctx, id)
	require.ErrorIs(t, err, sessionstore.ErrNotFound)
	require.EqualValues(t, 1, p.lostCount)
	require.Len(t, p.recentTakeovers(), 1)
	require.True(t, p.recentTakeovers()[0].Lost)
	require.Equal(t, "move", p.recentTakeovers()[0].Kind)
}
