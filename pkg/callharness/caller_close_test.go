package callharness_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// A caller that closes its PeerConnection without DELETE is a normal hangup.
func TestCallerCloseWithoutDeleteIsNotLoss(t *testing.T) {
	forSessionStores(t, func(t *testing.T, store sessionstore.Store) {
		if store == nil {
			store = sessionstore.NewMemory()
		}
		for _, deleteAfterClose := range []bool{false, true} {
			t.Run(map[bool]string{false: "without-delete", true: "close-before-delete"}[deleteAfterClose], func(t *testing.T) {
				h := startHarness(t, callharness.Options{Relay: true, Workers: 2, SessionStore: store, DisableFrameCache: true})
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				call, err := h.Dial(ctx, callharness.CallOptions{Video: true})
				require.NoError(t, err)
				require.NoError(t, call.SendMedia(ctx, 300*time.Millisecond))
				require.NoError(t, call.CloseWithoutDelete())
				require.Eventually(t, func() bool {
					_, err := store.Get(ctx, call.SessionID())
					return err != nil // worker released live ownership; store tests verify the ended marker
				}, 2*time.Second, 10*time.Millisecond)
				if deleteAfterClose {
					report, err := call.Hangup(ctx)
					if err != nil {
						require.True(t, strings.Contains(err.Error(), "404"), err.Error())
					}
					require.Zero(t, report.DecryptionFailures.Total())
				}
				time.Sleep(1500 * time.Millisecond)
				status, err := h.Status(ctx)
				require.NoError(t, err)
				t.Logf("calls=%d lost_count=%d takeovers=%+v", len(status.Calls), status.LostCount, status.Takeovers)
				require.Empty(t, status.Calls)
				require.Empty(t, status.Takeovers)
				require.Zero(t, status.LostCount, "caller hangup counted as a definitive loss")
			})
		}
	})
}
