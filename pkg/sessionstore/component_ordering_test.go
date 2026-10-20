package sessionstore

import (
	"context"
	"encoding/binary"
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"

	codec "github.com/relaishq/relais/internal/componentcodec"
	"github.com/relaishq/relais/internal/redistestcluster"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func checkComponentRevisionOrder(t *testing.T, r *Redis) {

	ctx := context.Background()
	order, err := r.client.Eval(ctx, "return 'B' > 'a' and 1 or 0", nil).Int64()
	require.NoError(t, err)
	require.EqualValues(t, 1, order, "Redis must actually run with non-C UTF-8 collation")
	// Both glibc and macOS en_US.UTF-8 collate letters by alphabet before
	// case: B follows a, although its byte value (66) precedes a (97).
	const workRevision, hostRevision uint64 = 'B', 'a'
	lease, err := r.Claim(ctx, "locale-order", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
	require.NoError(t, err)
	first, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
	require.NoError(t, err)
	require.NoError(t, r.client.HSet(ctx, r.keys(lease.SessionID)[0], "next_seq", strconv.FormatUint(workRevision-1, 10)).Err())
	c := cloneComponents(first.Components)
	c.Work = appendComponentBytes(t, c.Work, codec.Work, 1)
	work, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 1, Expected: &first.Metadata})
	require.NoError(t, err)
	require.Equal(t, workRevision, work.Metadata.References[1].Revision)
	// Exercise the old comparison on the actual fixed-width revision bytes,
	// including their shared NUL prefix, on this same Redis primary.
	oldAfter, err := r.client.Eval(ctx, "return ARGV[1] > ARGV[2] and 1 or 0", []string{r.keys(lease.SessionID)[0]},
		binary.BigEndian.AppendUint64(nil, work.Metadata.References[1].Revision),
		binary.BigEndian.AppendUint64(nil, hostRevision)).Int64()
	require.NoError(t, err)
	require.EqualValues(t, 1, oldAfter, "old Lua comparison must misorder revision %d before %d", workRevision, hostRevision)
	require.NoError(t, r.client.HSet(ctx, r.keys(lease.SessionID)[0], "next_seq", strconv.FormatUint(hostRevision-1, 10)).Err())
	c.Transport = advanceComponentTransport(t, c.Transport)
	hostOnly, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 1, Expected: &work.Metadata})
	require.NoError(t, err, "host-only commit at revision %d after a work change at revision %d", hostRevision, workRevision)
	require.Equal(t, hostRevision, hostOnly.Metadata.References[4].Revision)
}

func TestClusterComponentRevisionOrderingUnderUTF8(t *testing.T) {
	if testing.Short() {
		t.Skip("owned UTF-8 Redis and Cluster")
	}
	t.Run("standalone", func(t *testing.T) {
		r, err := NewRedis(context.Background(), storage.RedisConfig{Addr: redistestcluster.Standalone(t), Prefix: "utf8:"}, make([]byte, 32))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, r.Close()) })
		checkComponentRevisionOrder(t, r)
	})
	t.Run("cluster", func(t *testing.T) {
		r := componentCluster(t, redistestcluster.Addresses(t))
		checkComponentRevisionOrder(t, r)
	})
}
func TestComponentInitRefusesUnmarkedLegacyWrite(t *testing.T) {
	forComponentRedis(t, func(t *testing.T, r *Redis) {
		ctx := context.Background()
		lease, err := r.Claim(ctx, "unmarked-legacy", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		lua, err := os.ReadFile("testdata/v0.2-session.lua")
		require.NoError(t, err)
		_, err = r.client.Eval(ctx, string(lua), r.keys(lease.SessionID), "sequence", lease.Worker.String(), strconv.FormatUint(lease.Epoch, 10), 1, 300000).Result()
		require.NoError(t, err)
		// Execute the actual old put script, not a modern put followed by marker removal.
		result, err := r.client.Eval(ctx, string(lua), r.keys(lease.SessionID), "put", lease.Worker.String(), strconv.FormatUint(lease.Epoch, 10), 1, 300000, "old sealed fixture", "1", "old digest").Slice()
		require.NoError(t, err)
		require.EqualValues(t, 1, result[0])
		require.Empty(t, r.client.HGet(ctx, r.keys(lease.SessionID)[0], "layout").Val())
		before := snapshotComponentKeys(t, r, lease.SessionID)
		_, err = r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
		require.ErrorIs(t, err, ErrCheckpointLayout)
		// Reservation is allowed to advance next_seq, but no component/key conversion may occur.
		after := snapshotComponentKeys(t, r, lease.SessionID)
		delete(before, r.keys(lease.SessionID)[0])
		delete(after, r.keys(lease.SessionID)[0])
		require.Equal(t, before, after)
		require.Equal(t, "old sealed fixture", r.client.Get(ctx, r.keys(lease.SessionID)[1]).Val())
	})
}
