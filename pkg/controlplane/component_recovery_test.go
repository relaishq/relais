package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	codec "github.com/relaishq/relais/internal/componentcodec"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type tupleReadStore struct {
	sessionstore.Store
	tuple                             sessionstore.ComponentTuple
	err                               error
	reads, legacyReads, metadataReads int
}

func (s *tupleReadStore) ReadComponents(context.Context, string) (sessionstore.ComponentTuple, error) {
	s.reads++
	return s.tuple, s.err
}
func (s *tupleReadStore) CommitComponents(context.Context, sessionstore.Lease, sessionstore.ComponentCommit) (sessionstore.ComponentTuple, error) {
	panic("unexpected commit")
}
func (s *tupleReadStore) ComponentKeyInventory(context.Context) (map[byte]uint64, error) {
	return nil, nil
}
func (s *tupleReadStore) GetState(context.Context, string) ([]byte, error) {
	s.legacyReads++
	return []byte("legacy"), nil
}
func (s *tupleReadStore) Checkpoint(context.Context, string, []byte) (sessionstore.Checkpoint, error) {
	s.metadataReads++
	return sessionstore.Checkpoint{}, nil
}
func TestComponentRecoveryCarriesOneTupleIntoDecision(t *testing.T) {
	data, err := os.ReadFile("../mediaworker/testdata/v10-control.json")
	require.NoError(t, err)
	var wire struct {
		Components   sessionstore.Components
		StateVersion uint32
	}
	require.NoError(t, json.Unmarshal(data, &wire))
	var setup codec.ConnectionSetup
	require.NoError(t, codec.Unmarshal(wire.Components.Setup, codec.Setup, &setup))
	now := time.Now()
	store := &tupleReadStore{tuple: sessionstore.ComponentTuple{Components: wire.Components, Lease: sessionstore.Lease{SessionID: setup.ID}, Metadata: sessionstore.ComponentMetadata{StateVersion: wire.StateVersion}, Checkpoint: sessionstore.Checkpoint{Now: now, StoredAt: now.Add(-time.Millisecond), Age: time.Millisecond}}}
	state, metadata, err := mediaworker.ReadRecoverySnapshot(context.Background(), store, setup.ID)
	require.NoError(t, err)
	p := NewWithConfig(nil, store, Config{})
	decision, err := p.checkpointDecision(context.Background(), setup.ID, state, 0, metadata)
	require.NoError(t, err)
	require.Equal(t, store.tuple.Checkpoint.StoredAt, decision.storedAt)
	require.GreaterOrEqual(t, decision.age, time.Millisecond)
	require.Equal(t, 1, store.reads)
	require.Zero(t, store.legacyReads)
	require.Zero(t, store.metadataReads)
	for _, failure := range []error{sessionstore.ErrUnsafeCheckpoint, sessionstore.ErrUnsafeCheckpointClock, sessionstore.ErrTransient} {
		store.err = failure
		_, _, err = mediaworker.ReadRecoverySnapshot(context.Background(), store, setup.ID)
		require.ErrorIs(t, err, failure)
		require.Zero(t, store.legacyReads, "unsafe or uncertain tuple never falls back to legacy state")
	}
}

type inventorySampleStore struct {
	sessionstore.Store
	mu               sync.Mutex
	counts           map[byte]uint64
	err              error
	entered, unblock chan struct{}
}

func (s *inventorySampleStore) ComponentKeyInventory(ctx context.Context) (map[byte]uint64, error) {
	s.mu.Lock()
	counts, err, entered, unblock := s.counts, s.err, s.entered, s.unblock
	s.mu.Unlock()
	if entered != nil {
		close(entered)
		select {
		case <-unblock:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return counts, err
}
func TestComponentInventoryStatusPreservesUnknownAndStaleSamples(t *testing.T) {
	ctx := context.Background()
	store := &inventorySampleStore{err: errors.New("scan failed")}
	p := NewWithConfig(nil, store, Config{})
	status := func() KeyInventoryStatus {
		value, err := p.Status(ctx)
		require.NoError(t, err)
		return value.SessionStoreKeys
	}
	first := status()
	require.Equal(t, "failed", first.State)
	require.Nil(t, first.Counts)
	require.True(t, first.ObservedAt.IsZero())
	store.mu.Lock()
	store.err = nil
	store.counts = map[byte]uint64{2: 5}
	store.mu.Unlock()
	p.inventory.mu.Lock()
	p.inventory.attempted = time.Time{}
	p.inventory.mu.Unlock()
	fresh := status()
	require.Equal(t, "current", fresh.State)
	require.Equal(t, map[byte]uint64{2: 5}, fresh.Counts)
	entered, unblock := make(chan struct{}), make(chan struct{})
	store.mu.Lock()
	store.err = errors.New("next scan failed")
	store.entered = entered
	store.unblock = unblock
	store.mu.Unlock()
	p.inventory.mu.Lock()
	p.inventory.attempted = time.Now().Add(-2 * keyInventoryInterval)
	p.inventory.value.ObservedAt = p.inventory.attempted
	p.inventory.mu.Unlock()
	stale := status()
	require.Equal(t, "stale", stale.State)
	require.Equal(t, fresh.Counts, stale.Counts)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh never started")
	}
	close(unblock)
	require.Eventually(t, func() bool { return status().State == "failed" }, 5*time.Second, 10*time.Millisecond)
	failed := status()
	require.Equal(t, fresh.Counts, failed.Counts)
	require.False(t, failed.ObservedAt.IsZero())
	require.NotEmpty(t, failed.Error)
}

func TestComponentUnsafeTakeoverRecordsLossAndReleasesLease(t *testing.T) {
	p, _, target, _ := setup(t)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	wrapped := &tupleReadStore{Store: p.store, err: sessionstore.ErrUnsafeCheckpoint}
	p.store = wrapped
	source := p.workers["a"]
	source.dead = true
	p.takeover(ctx, source, lease, time.Now())
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Takeovers, 1)
	require.True(t, status.Takeovers[0].Lost)
	require.Equal(t, "checkpoint_unsafe", status.Takeovers[0].LossCause)
	require.EqualValues(t, 1, status.LostCount)
	require.Empty(t, status.Calls)
	require.False(t, target.runs(id))
	require.Zero(t, wrapped.legacyReads)
	_, err = wrapped.Get(ctx, id)
	require.ErrorIs(t, err, sessionstore.ErrEnded)
}
