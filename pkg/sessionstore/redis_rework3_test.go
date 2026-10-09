package sessionstore

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRedisRenewingWorkerIndexSurvivesDroppedRepairs(t *testing.T) {
	r := testRedis(t, RedisOptions{Retention: 100 * time.Millisecond, IndexGrace: 2100 * time.Millisecond})
	ctx := context.Background()
	worker := netip.MustParseAddrPort("127.0.0.1:1")
	lease, err := r.Claim(ctx, "still-renewing", worker, time.Minute)
	require.NoError(t, err)
	r.maintenance.Wait()
	// Saturate every maintenance slot. Renew cannot enqueue an index repair.
	for range cap(r.maintenanceSlots) {
		r.maintenanceSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for range cap(r.maintenanceSlots) {
			<-r.maintenanceSlots
		}
	})
	// Accelerate only the index's aging, keeping ample headroom on the lease.
	// A worker without new calls must survive even this impending index expiry.
	require.NoError(t, r.client.PExpire(ctx, r.indexKey(worker), 2200*time.Millisecond).Err())
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		lease, err = r.Renew(ctx, lease, time.Minute)
		require.NoError(t, err)
		if refresher, ok := any(r).(interface {
			RefreshWorkerIndex(context.Context, netip.AddrPort, time.Duration) error
		}); ok {
			require.NoError(t, refresher.RefreshWorkerIndex(ctx, worker, time.Minute))
		}
		time.Sleep(100 * time.Millisecond)
	}
	current, err := r.Get(ctx, lease.SessionID)
	require.NoError(t, err)
	listed, err := r.ListByWorker(ctx, worker)
	require.NoError(t, err)
	require.Equal(t, []Lease{current}, listed, "live renewed leases remain discoverable after the original index expiry")
}
