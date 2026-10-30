package callharness

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestRelayRequestCancellationStillRecoversCaller(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		_, w, h, relays, drops := relayChangeTopology(t, store, false, true)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		call, err := h.Dial(ctx, CallOptions{Video: true, RelayChange: true})
		require.NoError(t, err)
		sent := make(chan error, 1)
		go func() { sent <- call.SendMedia(ctx, 5*time.Second) }()
		require.Eventually(t, func() bool { return len(call.rec.report().Tracks) == 2 }, time.Second, 10*time.Millisecond)

		requestCtx, disconnect := context.WithCancel(ctx)
		defer disconnect()
		req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, call.resourceURL+"/relay?to=r2", nil)
		require.NoError(t, err)
		requested := make(chan error, 1)
		go func() {
			resp, e := h.httpClient.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			requested <- e
		}()
		// Checks reaching the sink prove Pion has restarted and dropped its old
		// selected pair. Cancellation must still arrange a recovery answer.
		require.Eventually(t, func() bool { return drops.Load() > 0 }, 2*time.Second, 10*time.Millisecond)
		disconnect()
		require.ErrorIs(t, <-requested, context.Canceled)
		require.Eventually(t, func() bool {
			_, a, e := store.(sessionstore.RelayAssignments).RelayOwner(ctx, call.SessionID())
			return e == nil && a.Slot == "r1" && a.Pending == "" && a.Generation >= 2
		}, 2*time.Second, 10*time.Millisecond, "request cancellation must complete the higher-generation old-slot restart")
		require.Eventually(t, func() bool { return call.selectedRemoteAddr() == relays[0].PublicAddr().String() }, time.Second, 10*time.Millisecond)
		before := call.rec.report()
		require.Eventually(t, func() bool {
			after := call.rec.report()
			for _, track := range after.Tracks {
				for _, old := range before.Tracks {
					if track.Kind == old.Kind && track.Packets <= old.Packets {
						return false
					}
				}
			}
			return true
		}, time.Second, 10*time.Millisecond, "caller media must recover after the operator disconnects")
		require.NoError(t, <-sent)
		failures, err := w.SessionDecryptFailures(call.SessionID())
		require.NoError(t, err)
		require.Zero(t, failures)
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		require.Zero(t, report.DecryptionFailures.Total())
		for _, track := range report.Tracks {
			require.Zero(t, track.DuplicatePackets)
			t.Logf("CANCEL_RECOVERY track=%s caller_gap=%s", track.Kind, track.MediaGap)
		}
	})
}
