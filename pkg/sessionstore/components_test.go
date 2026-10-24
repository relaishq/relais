package sessionstore

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	codec "github.com/relaishq/relais/internal/componentcodec"
	"github.com/relaishq/relais/internal/redistestcluster"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func componentFixture(id string) Components {
	setup, _ := codec.Marshal(codec.Setup, codec.ConnectionSetup{ID: id, LocalUfrag: id, LocalPwd: "password"})
	transport, _ := codec.Marshal(codec.Transport, codec.TransportState{LegacyVersion: 9, RemoteAddr: "127.0.0.1:12345", DTLS: codec.DTLSProgress{LocalEpoch: 1}, Profile: 1})
	work, _ := codec.Marshal(codec.Work, []byte("working notes"))
	history, _ := codec.Marshal(codec.History, []byte("non-empty history"))
	return Components{Setup: setup, Transport: transport, Work: work, History: history}
}
func advanceComponentTransport(t *testing.T, b []byte) []byte {
	t.Helper()
	var state codec.TransportState
	require.NoError(t, codec.Unmarshal(b, codec.Transport, &state))
	state.AgentRevision++
	out, err := codec.Marshal(codec.Transport, state)
	require.NoError(t, err)
	return out
}
func appendComponentBytes(t *testing.T, b []byte, kind byte, v byte) []byte {
	t.Helper()
	var value []byte
	require.NoError(t, codec.Unmarshal(b, kind, &value))
	value = append(value, v)
	out, err := codec.Marshal(kind, value)
	require.NoError(t, err)
	return out
}
func componentCluster(t *testing.T, addrs []string) *Redis {
	t.Helper()
	r, err := NewRedis(context.Background(), storage.RedisConfig{Addrs: addrs, Cluster: true, Prefix: fmt.Sprintf("component:%d:", time.Now().UnixNano())}, make([]byte, 32))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, redistestcluster.WaitForStable(ctx, r.client.(*redis.ClusterClient)))
	t.Cleanup(func() {
		r.maintenance.Wait()
		ctx := context.Background()
		require.NoError(t, r.client.(*redis.ClusterClient).ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
			var cursor uint64
			for {
				keys, next, e := node.Scan(ctx, cursor, r.prefix+"*", 100).Result()
				if e != nil {
					return e
				}
				for _, key := range keys {
					if e = node.Del(ctx, key).Err(); e != nil {
						return e
					}
				}
				cursor = next
				if cursor == 0 {
					return nil
				}
			}
		}))
		require.NoError(t, r.Close())
	})
	return r
}
func forComponentStores(t *testing.T, test func(*testing.T, Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { test(t, NewMemory()) })
	t.Run("redis", func(t *testing.T) { test(t, testRedis(t)) })
	t.Run("cluster", func(t *testing.T) { addrs := redistestcluster.Addresses(t); test(t, componentCluster(t, addrs)) })
}
func componentEngine(s Store) *Redis {
	if m, ok := s.(*Memory); ok {
		return m.componentCrypto
	}
	return s.(*Redis)
}
func storedComponentRecord(t *testing.T, s Store, id string) componentRecord {
	t.Helper()
	if m, ok := s.(*Memory); ok {
		return m.componentRecords[id]
	}
	r := s.(*Redis)
	record, _, _, err := r.readComponentRecord(context.Background(), id)
	require.NoError(t, err)
	return record
}
func TestComponentStoreContract(t *testing.T) {
	forComponentStores(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cs := s.(ComponentStore)
		worker := netip.MustParseAddrPort("127.0.0.1:23456")
		lease, err := s.Claim(ctx, "a{component}", worker, time.Minute)
		require.NoError(t, err)
		c := componentFixture(lease.SessionID)
		first, err := cs.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 3})
		require.NoError(t, err)
		require.Equal(t, c, first.Components)
		require.Equal(t, uint64(lease.Epoch), first.Metadata.SetupGeneration)
		require.False(t, first.Checkpoint.StoredAt.IsZero())
		require.Positive(t, first.WrittenBytes)
		if r, ok := s.(*Redis); ok {
			keys := r.componentKeys(lease.SessionID)
			require.Len(t, keys, 11)
			require.Equal(t, 8, len(r.client.HGet(ctx, keys[6], "t").Val()), "commit time uses the codec eight-byte bound")
			for _, key := range keys[5:] {
				require.EqualValues(t, 1, r.client.Exists(ctx, key).Val())
			}
			require.EqualValues(t, 0, r.client.Exists(ctx, r.keys(lease.SessionID)[1]).Val())
		}
		old := storedComponentRecord(t, s, lease.SessionID)
		c.Transport = advanceComponentTransport(t, c.Transport)
		progress, err := cs.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 3, Expected: &first.Metadata})
		require.NoError(t, err)
		now := storedComponentRecord(t, s, lease.SessionID)
		require.Equal(t, old.sealed.History, now.sealed.History, "progress commits retain exact history ciphertext")
		require.Equal(t, old.sealed.Setup, now.sealed.Setup)
		require.Equal(t, old.sealed.Work, now.sealed.Work)
		require.Equal(t, first.Metadata.References[:4], progress.Metadata.References[:4])
		c.History = appendComponentBytes(t, c.History, codec.History, 2)
		history, err := cs.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 3, Expected: &progress.Metadata})
		require.NoError(t, err)
		require.NotEqual(t, history.Metadata.References[3], progress.Metadata.References[3])
		require.NotEqual(t, history.Metadata.Banks[2], progress.Metadata.Banks[2])
		// A host-only capture based on the old history/root is a refusal, not an
		// invitation to merge its stale app pair into a newer tuple.
		_, err = cs.CommitComponents(ctx, lease, ComponentCommit{Components: progress.Components, StateVersion: 3, Expected: &progress.Metadata})
		require.ErrorIs(t, err, ErrStateSuperseded)
		after, err := cs.ReadComponents(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, history.Components, after.Components)
		after.Components.History[2] ^= 1
		after, err = cs.ReadComponents(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, history.Components, after.Components, "read returns owned copies")
		changed := cloneComponents(c)
		var setup codec.ConnectionSetup
		require.NoError(t, codec.Unmarshal(changed.Setup, codec.Setup, &setup))
		setup.LocalPwd = "changed"
		changed.Setup, err = codec.Marshal(codec.Setup, setup)
		require.NoError(t, err)
		_, err = cs.CommitComponents(ctx, lease, ComponentCommit{Components: changed, StateVersion: 3, Expected: &history.Metadata})
		require.ErrorIs(t, err, ErrSetupChanged)
		_, err = cs.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 3, Expected: &history.Metadata, WriteBudgetBytes: 1})
		require.ErrorIs(t, err, ErrComponentCapacity)
		next, err := s.Transfer(ctx, lease, netip.MustParseAddrPort("127.0.0.1:23457"), 2*time.Minute)
		require.NoError(t, err)
		require.ErrorIs(t, s.PutState(ctx, next, []byte("legacy")), ErrCheckpointLayout)
		_, err = s.GetState(ctx, lease.SessionID)
		require.ErrorIs(t, err, ErrCheckpointLayout)
		_, err = cs.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 3, Expected: &history.Metadata})
		require.ErrorIs(t, err, ErrLeaseLost)
		require.NoError(t, s.Release(ctx, lease))
		taken, err := cs.ReadComponents(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, next.Epoch, taken.Lease.Epoch)
		require.Equal(t, history.Components, taken.Components)
		resumed, err := cs.CommitComponents(ctx, next, ComponentCommit{Components: c, StateVersion: 3, Expected: &history.Metadata})
		require.NoError(t, err)
		require.Equal(t, next.Epoch, resumed.Metadata.OwnerEpoch)
		renewed, err := s.Renew(ctx, next, 3*time.Minute)
		require.NoError(t, err)
		if r, ok := s.(*Redis); ok {
			for _, key := range r.componentKeys(lease.SessionID)[5:] {
				expiry, err := r.client.Do(ctx, "PEXPIRETIME", key).Int64()
				require.NoError(t, err)
				require.Equal(t, renewed.ExpiresAt.UnixMilli(), expiry)
			}
		}
		counts, err := cs.ComponentKeyInventory(ctx)
		require.NoError(t, err)
		require.Equal(t, map[byte]uint64{0: 5}, counts)
		require.NoError(t, s.Release(ctx, renewed))
		if r, ok := s.(*Redis); ok {
			for _, key := range r.componentKeys(lease.SessionID)[5:] {
				require.Zero(t, r.client.Exists(ctx, key).Val())
			}
		}
		_, err = cs.ReadComponents(ctx, lease.SessionID)
		require.ErrorIs(t, err, ErrNotFound)
		reclaimed, err := s.Claim(ctx, lease.SessionID, worker, time.Minute)
		require.NoError(t, err)
		fresh, err := cs.CommitComponents(ctx, reclaimed, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 3})
		require.NoError(t, err)
		require.NotEqual(t, first.Metadata.SetupGeneration, fresh.Metadata.SetupGeneration)
		require.NoError(t, s.Release(ctx, renewed))
		_, err = cs.ReadComponents(ctx, lease.SessionID)
		require.NoError(t, err)
	})
}
func TestComponentRotationAndDeferredBudget(t *testing.T) {
	forComponentStores(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cs := s.(ComponentStore)
		engine := componentEngine(s)
		lease, err := s.Claim(ctx, "rotation", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		c := componentFixture(lease.SessionID)
		first, err := cs.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 2})
		require.NoError(t, err)
		old := storedComponentRecord(t, s, lease.SessionID)
		engine.keysByID[1] = make([]byte, 32)
		engine.keysByID[1][0] = 1
		engine.activeKey = 1
		deferred, err := cs.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 2, Expected: &first.Metadata, WriteBudgetBytes: first.WrittenBytes - len(old.sealed.Setup) - len(old.sealed.Work) - len(old.sealed.History)})
		require.NoError(t, err)
		require.True(t, deferred.RotationDeferred)
		for _, ref := range deferred.Metadata.References[:4] {
			require.EqualValues(t, 0, ref.KeyID)
		}
		require.Equal(t, old.sealed.History, storedComponentRecord(t, s, lease.SessionID).sealed.History)
		rotated, err := cs.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 2, Expected: &deferred.Metadata})
		require.NoError(t, err)
		require.False(t, rotated.RotationDeferred)
		for _, ref := range rotated.Metadata.References {
			require.EqualValues(t, 1, ref.KeyID)
		}
		fresh := storedComponentRecord(t, s, lease.SessionID)
		require.NotEqual(t, old.sealed.Setup, fresh.sealed.Setup)
		require.NotEqual(t, old.sealed.History, fresh.sealed.History)
		delete(engine.keysByID, 0)
		restored, err := cs.ReadComponents(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, c, restored.Components)
		counts, err := cs.ComponentKeyInventory(ctx)
		require.NoError(t, err)
		require.Equal(t, map[byte]uint64{1: 5}, counts)
	})
}
func TestComponentUnsafePartsFailClosed(t *testing.T) {
	forComponentStores(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cs := s.(ComponentStore)
		for _, kind := range []string{"setup", "work", "history", "transport", "metadata", "generation", "key-id", "version"} {
			t.Run(kind, func(t *testing.T) {
				lease, err := s.Claim(ctx, "unsafe-"+kind, netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
				require.NoError(t, err)
				tuple, err := cs.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
				require.NoError(t, err)
				if m, ok := s.(*Memory); ok {
					record := m.componentRecords[lease.SessionID]
					switch kind {
					case "setup":
						record.sealed.Setup = nil
					case "work":
						record.sealed.Work = nil
					case "history":
						record.sealed.History = nil
					case "transport":
						record.sealed.Transport[14] ^= 1
					case "metadata":
						record.metadata.References[3].Digest[0] ^= 1
					case "generation":
						record.metadata.SetupGeneration++
					case "key-id":
						record.metadata.References[2].KeyID = 255
					case "version":
						record.metadata.StateVersion++
					}
					m.componentRecords[lease.SessionID] = record
				} else {
					r := s.(*Redis)
					keys := r.componentKeys(lease.SessionID)
					switch kind {
					case "setup":
						require.NoError(t, r.client.HDel(ctx, keys[5], fmt.Sprint(tuple.Metadata.Banks[0])).Err())
					case "work":
						require.NoError(t, r.client.Del(ctx, keys[7+int(tuple.Metadata.Banks[1])]).Err())
					case "history":
						require.NoError(t, r.client.Del(ctx, keys[9+int(tuple.Metadata.Banks[2])]).Err())
					case "transport":
						require.NoError(t, r.client.HSet(ctx, keys[6], "b", "corrupt").Err())
					default:
						m := tuple.Metadata
						switch kind {
						case "metadata":
							m.References[3].Digest[0] ^= 1
						case "generation":
							m.SetupGeneration++
						case "key-id":
							m.References[2].KeyID = 255
						case "version":
							m.StateVersion++
						}
						packed, e := m.Marshal()
						require.NoError(t, e)
						require.NoError(t, r.client.HSet(ctx, keys[6], "m", packed).Err())
					}
				}
				_, err = cs.ReadComponents(ctx, lease.SessionID)
				require.ErrorIs(t, err, ErrUnsafeCheckpoint)
				require.NoError(t, s.Release(ctx, lease))
			})
		}
	})
}

func TestComponentEmptyHistoryAndSchemaChange(t *testing.T) {
	forComponentStores(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cs := s.(ComponentStore)
		lease, err := s.Claim(ctx, "empty-history", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		c := componentFixture(lease.SessionID)
		c.History, err = codec.Marshal(codec.History, []byte(nil))
		require.NoError(t, err)
		first, err := cs.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 1})
		require.NoError(t, err)
		restored, err := cs.ReadComponents(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, c.History, restored.Components.History)
		// State.Version describes the joint app schema, so both app banks must be
		// sealed under that version even when the history bytes remain empty.
		next, err := cs.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 2, Expected: &first.Metadata})
		require.NoError(t, err)
		require.NotEqual(t, first.Metadata.References[1], next.Metadata.References[1])
		require.NotEqual(t, first.Metadata.References[2], next.Metadata.References[2])
		restored, err = cs.ReadComponents(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, c, restored.Components)
		require.EqualValues(t, 2, restored.Metadata.StateVersion)
		invalid := cloneComponents(c)
		var setup codec.ConnectionSetup
		require.NoError(t, codec.Unmarshal(invalid.Setup, codec.Setup, &setup))
		setup.ID = "another-session"
		invalid.Setup, err = codec.Marshal(codec.Setup, setup)
		require.NoError(t, err)
		_, err = cs.CommitComponents(ctx, lease, ComponentCommit{Components: invalid, StateVersion: 2, Expected: &next.Metadata})
		require.ErrorIs(t, err, ErrUnsafeCheckpoint)
		restored, err = cs.ReadComponents(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, next.Metadata, restored.Metadata)
	})
}
