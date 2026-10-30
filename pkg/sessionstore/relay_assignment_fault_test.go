package sessionstore

import (
	"context"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/redistestenv"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func TestRedisRelayAssignmentLostReplyRemainsUncertain(t *testing.T) {
	for _, stage := range []string{"initialize", "begin", "commit", "rollback"} {
		t.Run(stage, func(t *testing.T) {
			r := testRedis(t)
			ctx := context.Background()
			lease, err := r.Claim(ctx, "relay-lost-reply", netip.MustParseAddrPort("127.0.0.1:17001"), time.Minute)
			require.NoError(t, err)
			from, to := RelayAssignment{}, RelayAssignment{Slot: "r1"}
			if stage != "initialize" {
				require.NoError(t, r.CASRelay(ctx, lease, from, to))
				from, to = to, RelayAssignment{Slot: "r1", Pending: "r2"}
			}
			if stage == "commit" || stage == "rollback" {
				require.NoError(t, r.CASRelay(ctx, lease, from, to))
				from, to = to, RelayAssignment{Slot: "r2", Generation: 1}
				if stage == "rollback" {
					to.Slot = "r1"
				}
			}
			hook := &lostResponseHook{op: "cas"}
			r.client.AddHook(redisFaultHook{hook})
			require.ErrorIs(t, r.CASRelay(ctx, lease, from, to), ErrTransient)
			require.Equal(t, 1, hook.calls, "an uncertain write must never replay")
			gotLease, got, err := r.RelayOwner(ctx, lease.SessionID)
			require.NoError(t, err)
			require.Equal(t, lease.Epoch, gotLease.Epoch)
			require.Equal(t, to, got, "the caller can settle the committed assignment")
		})
	}
}

func TestRedisRelayAssignmentPreservesLargeGenerationAndKeylessRead(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	lease, err := r.Claim(ctx, "relay-large-generation", netip.MustParseAddrPort("127.0.0.1:17001"), time.Minute)
	require.NoError(t, err)
	// Seed a long-lived call above Lua's exact integer range. The next real
	// begin/commit must preserve every generation bit.
	const generation = uint64(1<<53 + 7)
	require.NoError(t, r.client.HSet(ctx, r.keys(lease.SessionID)[0], "relay", "r1", "relay_gen", strconv.FormatUint(generation, 10)).Err())
	from := RelayAssignment{Slot: "r1", Generation: generation}
	pending := RelayAssignment{Slot: "r1", Generation: generation, Pending: "r2"}
	require.NoError(t, r.CASRelay(ctx, lease, from, pending))
	next := RelayAssignment{Slot: "r2", Generation: generation + 1}
	require.NoError(t, r.CASRelay(ctx, lease, pending, next))
	owners, err := NewRedisOwners(ctx, storage.RedisConfig{Addr: redistestenv.Address(t), Prefix: r.prefix})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owners.Close()) })
	gotLease, got, err := owners.RelayOwner(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, lease.Epoch, gotLease.Epoch)
	require.Equal(t, next, got)
	_, canReadSnapshots := any(owners).(Store)
	require.False(t, canReadSnapshots)
}
