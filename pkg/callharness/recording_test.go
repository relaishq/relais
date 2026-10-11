package callharness_test

import (
	"context"
	"testing"
	"time"

	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Exercise the bounded recorder against both real store choices without adding
// a long soak to PR checks. One takeover is discovered and summarized live.
func TestBoundedRecordingTakeover(t *testing.T) {
	if testing.Short() {
		t.Skip("targeted memory/Redis bounded-recording check")
	}
	forSessionStores(t, func(t *testing.T, store sessionstore.Store) {
		h, err := callharness.Start(callharness.Options{Relay: true, Workers: 2, SessionStore: store})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, h.Close()) })
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		call, err := h.Dial(ctx, callharness.CallOptions{Video: true, Recording: callharness.RecordingOptions{History: 10 * time.Second, DecodeEvery: 30}})
		require.NoError(t, err)
		sent := make(chan error, 1)
		go func() { sent <- call.SendMedia(ctx, 20*time.Second) }()
		time.Sleep(2 * time.Second)
		require.NoError(t, h.Kill(0))
		require.NoError(t, <-sent)
		rep, err := call.Hangup(ctx)
		require.NoError(t, err)
		require.Len(t, rep.Moves, 1, "periodic and final collections must not duplicate the event")
		require.Equal(t, "takeover", rep.Moves[0].Kind)
		require.Equal(t, 1, rep.Recording.EventSummaries)
		require.True(t, rep.Moves[0].Measurement.AudioLoss.Trusted, rep.Moves[0].Measurement.Summary())
		require.True(t, rep.Moves[0].Measurement.VideoLoss.Trusted, rep.Moves[0].Measurement.Summary())
		require.LessOrEqual(t, rep.Recording.ContentUnits, 900)
		require.Zero(t, rep.Recording.OfflineFrames)
		require.True(t, rep.Track("video").Video.OnlineDecode.Conclusive(), "decode: %+v", rep.Track("video").Video.OnlineDecode)
		require.Zero(t, rep.DecryptionFailures.Total())
		t.Log(rep.Moves[0].Measurement.Summary())
	})
}
