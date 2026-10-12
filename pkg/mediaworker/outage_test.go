package mediaworker

import (
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
	"net/netip"
	"testing"
	"time"
)

func TestOutageBudgetLeavesRenewalCadenceUnchanged(t *testing.T) {
	require.Equal(t, sessionstore.DefaultWorkerRenewInterval+ownershipTimeout+time.Second, sessionstore.WorkerLeaseHeadroom)
	for _, budget := range []time.Duration{0, 5 * time.Second, 45 * time.Second} {
		w, e := New(Config{Relay: &RelayConfig{Addr: netip.MustParseAddrPort("127.0.0.1:12345"), PublicAddr: netip.MustParseAddrPort("127.0.0.1:12346"), Owners: sessionstore.NewMemory(), StoreOutageBudget: budget}})
		require.NoError(t, e)
		require.Equal(t, sessionstore.StoreOutageBudget(budget)+sessionstore.WorkerLeaseHeadroom, w.cfg.Relay.LeaseTTL)
		require.Equal(t, time.Second, w.cfg.Relay.LeaseRenewInterval)
		require.NoError(t, w.Close())
	}
}
