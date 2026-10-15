package callharness_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestRelayOwnershipExpiredLoss(t *testing.T) {
	forSessionStores(t, func(t *testing.T, store sessionstore.Store) {
		if store == nil {
			store = sessionstore.NewMemory()
		}
		for _, crash := range []bool{false, true} {
			t.Run(strconv.FormatBool(crash), func(t *testing.T) {
				h := startHarness(t, callharness.Options{Relay: true, Workers: 2, SessionStore: store, DisableFrameCache: true})
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				call, err := h.Dial(ctx, callharness.CallOptions{Video: true})
				require.NoError(t, err)
				require.NoError(t, call.SendMedia(ctx, 300*time.Millisecond))
				lease, err := store.Get(ctx, call.SessionID())
				require.NoError(t, err)
				if crash {
					status, err := h.Status(ctx)
					require.NoError(t, err)
					owner, err := strconv.Atoi(status.Calls[0].Owner)
					require.NoError(t, err)
					require.NoError(t, h.Kill(owner))
				}
				_, err = store.Renew(ctx, lease, time.Millisecond)
				require.NoError(t, err)
				time.Sleep(5 * time.Millisecond)
				var status controlplane.Status
				require.Eventually(t, func() bool { status, err = h.Status(ctx); return err == nil && status.LostCount == 1 }, 2*time.Second, 10*time.Millisecond)
				require.Empty(t, status.Calls)
				require.Len(t, status.Takeovers, 1)
				event, err := json.Marshal(status.Takeovers[0])
				require.NoError(t, err)
				require.Contains(t, string(event), `"loss_cause":"ownership_expired"`)
				require.True(t, status.Takeovers[0].Lost)
				_, err = store.GetState(ctx, call.SessionID())
				require.ErrorIs(t, err, sessionstore.ErrNotFound)
				time.Sleep(200 * time.Millisecond)
				status, err = h.Status(ctx)
				require.NoError(t, err)
				require.EqualValues(t, 1, status.LostCount)
				require.Len(t, status.Takeovers, 1)
				report, hangup := call.Hangup(ctx)
				if hangup != nil {
					require.True(t, strings.Contains(hangup.Error(), "404"), hangup.Error())
				}
				require.NotNil(t, report)
				require.Zero(t, report.DecryptionFailures.Total())
				require.Zero(t, report.ICERestarts)
				require.Zero(t, report.Renegotiations)
				require.Len(t, report.Moves, 1)
				encoded, err := json.Marshal(report.Moves[0])
				require.NoError(t, err)
				require.Contains(t, string(encoded), `"loss_cause":"ownership_expired"`)
				require.Contains(t, string(encoded), `"lost":true`)
				require.GreaterOrEqual(t, report.Moves[0].Start, time.Duration(0))
			})
		}
	})
}
