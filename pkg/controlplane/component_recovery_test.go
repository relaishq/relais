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
	stop := p.startKeyInventory(context.Background())
	defer stop()
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
	stop := p.startKeyInventory(context.Background())
	defer stop()
	status := func() KeyInventoryStatus {
		value, err := p.Status(ctx)
		require.NoError(t, err)
		return value.SessionStoreKeys
	}
	require.Equal(t, "not_yet_scanned", status().State)
	require.Eventually(t, func() bool { return status().State == "failed" }, time.Second, time.Millisecond)
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
	require.Eventually(t, func() bool { return status().State == "current" }, time.Second, time.Millisecond)
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

func TestComponentInventoryStatusReportsUnverifiableCount(t *testing.T) {
	store := &inventorySampleStore{counts: map[byte]uint64{2: 5}, err: &sessionstore.InventoryUnverifiableError{Count: 3}}
	p := NewWithConfig(nil, store, Config{})
	stop := p.startKeyInventory(context.Background())
	defer stop()
	require.Eventually(t, func() bool {
		value, err := p.Status(context.Background())
		return err == nil && value.SessionStoreKeys.State == "unverifiable"
	}, time.Second, time.Millisecond)
	value, err := p.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, "unverifiable", value.SessionStoreKeys.State)
	require.EqualValues(t, 3, value.SessionStoreKeys.Unverifiable)
	require.Equal(t, map[byte]uint64{2: 5}, value.SessionStoreKeys.Counts)
	require.False(t, value.SessionStoreKeys.ObservedAt.IsZero())
}

func TestComponentInventoryStatusNeverWaitsAndStopsWithPlane(t *testing.T) {
	entered, unblock := make(chan struct{}), make(chan struct{})
	store := &inventorySampleStore{entered: entered, unblock: unblock}
	p := NewWithConfig(nil, store, Config{CheckInterval: time.Hour})
	require.Equal(t, "not_yet_scanned", p.keyInventoryStatus(context.Background()).State)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	defer cancel()
	require.Eventually(t, func() bool {
		p.inventory.mu.Lock()
		defer p.inventory.mu.Unlock()
		return p.inventory.lifetime != nil
	}, time.Second, time.Millisecond)
	response := make(chan KeyInventoryStatus, 1)
	go func() { response <- p.keyInventoryStatus(context.Background()) }()
	select {
	case value := <-response:
		require.Equal(t, "not_yet_scanned", value.State)
		require.Nil(t, value.Counts)
	case <-time.After(time.Second):
		t.Fatal("status waited for its inventory scan")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("scan did not start")
	}
	// The store is still blocked. Run must cancel that scan and join it.
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("scan outlived plane")
	}
	require.Equal(t, "failed", p.keyInventoryStatus(context.Background()).State)
	p.inventory.mu.Lock()
	require.Nil(t, p.inventory.lifetime)
	require.False(t, p.inventory.loading)
	p.inventory.mu.Unlock()
}

type finishingInventoryStore struct {
	sessionstore.Store
	entered, canceled, finish chan struct{}
}

func (s *finishingInventoryStore) ComponentKeyInventory(ctx context.Context) (map[byte]uint64, error) {
	close(s.entered)
	<-ctx.Done()
	close(s.canceled)
	// Model a bounded scan's cleanup after it observes cancellation.
	<-s.finish
	return nil, ctx.Err()
}

func TestComponentInventoryPlaneWaitsForCanceledScanCleanup(t *testing.T) {
	store := &finishingInventoryStore{entered: make(chan struct{}), canceled: make(chan struct{}), finish: make(chan struct{})}
	p := NewWithConfig(nil, store, Config{CheckInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { defer close(done); done <- p.Run(ctx) }()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(store.finish) }) }
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("plane did not exit after scan cleanup")
		}
	})
	require.Eventually(t, func() bool {
		p.inventory.mu.Lock()
		defer p.inventory.mu.Unlock()
		return p.inventory.lifetime != nil
	}, time.Second, time.Millisecond)
	p.keyInventoryStatus(context.Background())
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("inventory scan did not start")
	}
	cancel()
	select {
	case <-store.canceled:
	case <-time.After(time.Second):
		t.Fatal("scan did not observe plane cancellation")
	}
	select {
	case <-done:
		t.Fatal("plane returned while its canceled scan was still cleaning up")
	case <-time.After(time.Second):
	}
	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("plane did not join its completed scan")
	}
	require.Equal(t, "failed", p.keyInventoryStatus(context.Background()).State)
}
