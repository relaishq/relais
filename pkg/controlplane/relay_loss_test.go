package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestRelayLeaseLossStartsChange(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, _, id := relayFailurePlane(t, store)
		// The standby mechanism owns each slot's lease. No renewal simulates both
		// processes lost; worker heartbeats continue independently.
		leases := store.(sessionstore.RelayLeases)
		identity := sessionstore.RelayProcess{Owner: "relay-test", PID: 42, Start: "start", Host: "host", Namespace: "namespace", Executable: "relay"}
		_, err := leases.ClaimRelay(context.Background(), "r1", identity, time.Minute)
		require.NoError(t, err)
		live, err := leases.ClaimRelay(context.Background(), "r2", identity, time.Minute)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- p.Run(ctx) }()
		defer func() { cancel(); require.NoError(t, <-done) }()
		go func() {
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					_ = p.Heartbeat(a.addr)
					live, _ = leases.RenewRelay(ctx, live, time.Minute)
				}
			}
		}()
		require.Eventually(t, func() bool {
			_, assignment, e := store.(sessionstore.RelayAssignments).RelayOwner(ctx, id)
			return e == nil && assignment.Pending == "r2"
		}, 4*time.Second, 10*time.Millisecond, "reachable unchanged slot lease must trigger every affected call")
	})
}
