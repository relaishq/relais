package callharness

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/relay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func receivedAudioPackets(c *Call) int {
	c.rec.mu.Lock()
	defer c.rec.mu.Unlock()
	for _, track := range c.rec.tracks {
		if track.kind == "audio" {
			return track.packets
		}
	}
	return 0
}

// #443: a stale control process that resumes after the call moved A->B->A
// replays its old A->B route call at the older epoch. The relay must refuse
// it, so the caller's media stays on A instead of going black until a later
// binding lookup repairs the route.
func TestStaleControlMoveLeavesCallerMediaOnOwner(t *testing.T) {
	h, err := Start(Options{Relay: true, Workers: 2})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, h.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	call, err := h.Dial(ctx, CallOptions{})
	require.NoError(t, err)
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 4*time.Second) }()
	time.Sleep(500 * time.Millisecond)

	require.NoError(t, call.Handover(HandoverOptions{To: 1}))
	status, err := h.Status(ctx)
	require.NoError(t, err)
	staleEpoch := status.Calls[0].Epoch
	require.NoError(t, call.Handover(HandoverOptions{To: 0}))
	status, err = h.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, "0", status.Calls[0].Owner)
	require.Greater(t, status.Calls[0].Epoch, staleEpoch)

	a, b := h.workers.list[0].LocalAddr(), h.workers.list[1].LocalAddr()
	time.Sleep(300 * time.Millisecond)
	before := receivedAudioPackets(call)
	// The stale controller's exact planned-move route call.
	staleErr := h.workers.relay.relay.MoveSessionAfterExport(call.SessionID(), a, b, staleEpoch)
	time.Sleep(time.Second)
	// 20 ms audio is 50 packets a second; a misrouted call echoes none.
	require.Greater(t, receivedAudioPackets(call)-before, 25, "caller media must stay on A")
	require.ErrorIs(t, staleErr, relay.ErrStaleMove)

	require.NoError(t, <-sent)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	assert.True(t, report.ConnectedThroughout())
	assert.Zero(t, report.DecryptionFailures.Total())
}
