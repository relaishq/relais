package sessionstore

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestComponentInventoryKeepsAuthenticatedCountsWithUnverifiableRecords(t *testing.T) {
	forComponentStores(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cs := s.(ComponentStore)
		engine := componentEngine(s)
		seed := func(id string) ComponentTuple {
			lease, err := s.Claim(ctx, id, netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
			require.NoError(t, err)
			tuple, err := cs.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(id), StateVersion: 1})
			require.NoError(t, err)
			return tuple
		}
		good := seed("inventory-good")
		bad := seed("inventory-bad")
		if m, ok := s.(*Memory); ok {
			rec := m.componentRecords[bad.Lease.SessionID]
			rec.sealed.Transport[0] ^= 1
			m.componentRecords[bad.Lease.SessionID] = rec
		} else {
			r := s.(*Redis)
			require.NoError(t, r.client.HSet(ctx, r.componentKeys(bad.Lease.SessionID)[6], "b", "unverifiable cipher").Err())
		}
		counts, err := cs.ComponentKeyInventory(ctx)
		var partial *InventoryUnverifiableError
		require.ErrorAs(t, err, &partial)
		require.EqualValues(t, 1, partial.Count)
		require.Equal(t, map[byte]uint64{0: 5}, counts)
		// A broken app ciphertext does not conceal authenticated key IDs: metadata
		// is authenticated by checkpoint AEAD, without opening component payloads.
		if m, ok := s.(*Memory); ok {
			rec := m.componentRecords[good.Lease.SessionID]
			rec.sealed.History[0] ^= 1
			m.componentRecords[good.Lease.SessionID] = rec
		} else {
			r := s.(*Redis)
			require.NoError(t, r.client.Set(ctx, r.componentKeys(good.Lease.SessionID)[9+int(good.Metadata.Banks[2])], "broken opaque history", time.Minute).Err())
		}
		counts, err = cs.ComponentKeyInventory(ctx)
		require.ErrorAs(t, err, &partial)
		require.EqualValues(t, 1, partial.Count)
		require.Equal(t, map[byte]uint64{0: 5}, counts)
		if r, ok := s.(*Redis); ok {
			engine.keysByID[9] = make([]byte, 32)
			engine.keysByID[9][0] = 9
			engine.activeKey = 9
			legacy, err := r.Claim(ctx, "inventory-retired", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
			require.NoError(t, err)
			require.NoError(t, r.PutState(ctx, legacy, []byte("legacy encrypted")))
			engine.activeKey = 0
			delete(engine.keysByID, 9)
			require.NoError(t, r.client.HSet(ctx, r.keys(good.Lease.SessionID)[0], "layout", "legacy").Err())
			counts, err = cs.ComponentKeyInventory(ctx)
			require.ErrorAs(t, err, &partial)
			require.EqualValues(t, 3, partial.Count)
			require.Empty(t, counts)
		}
		require.False(t, errors.Is(err, ErrTransient))
	})
}
func TestComponentInventoryAuthenticatesOldReferencesWithoutTheirDecryptKeys(t *testing.T) {
	forComponentStores(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cs := s.(ComponentStore)
		engine := componentEngine(s)
		engine.keysByID[9] = make([]byte, 32)
		engine.keysByID[9][0] = 9
		engine.activeKey = 9
		lease, err := s.Claim(ctx, "inventory-old-refs", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		first, err := cs.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
		require.NoError(t, err)
		old := storedComponentRecord(t, s, lease.SessionID)
		engine.activeKey = 0
		ordinary := first.WrittenBytes - len(old.sealed.Setup) - len(old.sealed.Work) - len(old.sealed.History)
		deferred, err := cs.CommitComponents(ctx, lease, ComponentCommit{Components: first.Components, StateVersion: 1, Expected: &first.Metadata, WriteBudgetBytes: ordinary})
		require.NoError(t, err)
		require.True(t, deferred.RotationDeferred)
		delete(engine.keysByID, 9)
		counts, err := cs.ComponentKeyInventory(ctx)
		require.NoError(t, err)
		require.Equal(t, map[byte]uint64{0: 1, 9: 4}, counts)
	})
}
