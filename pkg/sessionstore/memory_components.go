package sessionstore

import (
	"context"
	"time"
)

func (m *Memory) ReadComponents(ctx context.Context, id string) (ComponentTuple, error) {
	if err := ctx.Err(); err != nil {
		return ComponentTuple{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	lease, ok := m.leases[id]
	if !ok || !now.Before(lease.ExpiresAt) {
		delete(m.componentRecords, id)
		return ComponentTuple{}, ErrNotFound
	}
	record, ok := m.componentRecords[id]
	if !ok {
		return ComponentTuple{}, ErrCheckpointLayout
	}
	return tupleFromRecord(m.componentCrypto, id, record, lease, now)
}
func (m *Memory) CommitComponents(ctx context.Context, lease Lease, request ComponentCommit) (ComponentTuple, error) {
	if err := ctx.Err(); err != nil {
		return ComponentTuple{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.leases[lease.SessionID]
	if !ok || !same(lease, current) || !time.Now().Before(current.ExpiresAt) {
		return ComponentTuple{}, ErrLeaseLost
	}
	if _, ok := m.states[lease.SessionID]; ok {
		return ComponentTuple{}, ErrCheckpointLayout
	}
	var previous *componentRecord
	if record, ok := m.componentRecords[lease.SessionID]; ok {
		previous = &record
	}
	m.componentSequence++
	p, err := m.componentCrypto.prepareComponents(lease, request, previous, m.componentSequence)
	if err != nil {
		return ComponentTuple{}, err
	}
	p.record.stored = time.Now()
	m.componentRecords[lease.SessionID] = p.record
	checkpoint, err := checkpointAt(p.record.stored, p.record.stored)
	return ComponentTuple{Components: cloneComponents(p.plain), Metadata: p.record.metadata, Lease: current, Checkpoint: checkpoint, WrittenBytes: p.written, RotationDeferred: p.deferred}, err
}
func (m *Memory) ComponentKeyInventory(ctx context.Context) (map[byte]uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	counts := map[byte]uint64{}
	var unverifiable uint64
	now := time.Now()
	for id, record := range m.componentRecords {
		lease, ok := m.leases[id]
		if !ok || !now.Before(lease.ExpiresAt) {
			continue
		}
		if err := m.componentCrypto.authenticateInventoryMetadata(id, record, lease); err != nil {
			unverifiable++
			continue
		}
		for _, ref := range record.metadata.References {
			counts[ref.KeyID]++
		}
	}
	if unverifiable > 0 {
		return counts, &InventoryUnverifiableError{Count: unverifiable}
	}
	return counts, nil
}
