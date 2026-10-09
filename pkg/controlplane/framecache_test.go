package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/relais/pkg/framecache"
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
