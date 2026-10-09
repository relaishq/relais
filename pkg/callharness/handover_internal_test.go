package callharness

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFailedHandoverRollsBack makes a handover fail after the old worker has
// exported the session: the new worker is closed, so it cannot resume it.
// The old worker then resumes the session from the same bytes, and the
// caller sees no more than it would of a successful move.
func TestFailedHandoverRollsBack(t *testing.T) {
	h, err := Start(Options{Workers: 2})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, h.Close()) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	call, err := h.Dial(ctx, CallOptions{Video: true})
	require.NoError(t, err)

	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 3*time.Second) }()
	time.Sleep(time.Second)
	require.NoError(t, h.workers.list[1].Close())
	require.Error(t, call.Handover(HandoverOptions{To: 1}), "handover to a closed worker")
	require.NoError(t, <-sent)

	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	t.Logf("call with a failed handover\n%s", report.Summary())

	assert.True(t, report.ConnectedThroughout(), "connection states: %v", report.ConnectionStates)
	assert.Zero(t, report.DecryptionFailures.Total(), "SRTP decryption failures")
	assert.Zero(t, report.Renegotiations, "renegotiations")
	assert.Zero(t, report.ICERestarts, "ICE restarts")
	require.Len(t, report.Moves, 1, "moves")
	move := report.Moves[0]
	assert.NotEmpty(t, move.Error, "the move failed")
	assert.True(t, move.Result.RolledBack, "the old worker resumed the session")
	assert.Positive(t, move.Result.StateBytes, "the old worker exported the session")
	for _, track := range move.Tracks {
		assert.Positive(t, track.PacketsAfter, "%s after the failed move", track.Kind)
		assert.Less(t, track.Gap, 100*time.Millisecond, "%s gap around the failed move", track.Kind)
		assert.Zero(t, track.SkippedSequenceNumbers, "%s sequence numbers skipped", track.Kind)
	}
	for _, echo := range report.Tracks {
		assert.Zero(t, echo.SequenceDiscontinuities, "%s echo is one continuous stream", echo.Kind)
	}
}
