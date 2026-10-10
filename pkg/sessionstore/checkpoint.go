package sessionstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"
)

var ErrUnsafeCheckpointClock = errors.New("sessionstore: checkpoint store clock moved backwards")

// Checkpoint describes a successfully stored blob. Age and Now come from the
// store clock. The digest binds the metadata to the exact state read by takeover.
type Checkpoint struct {
	StoredAt time.Time     `json:"stored_at"`
	Now      time.Time     `json:"now"`
	Age      time.Duration `json:"age"`
	digest   [32]byte
}

func (m *Memory) Clock(ctx context.Context, _ string) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	return time.Now(), nil
}

func (m *Memory) Checkpoint(ctx context.Context, id string, state []byte) (Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return Checkpoint{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	lease, ok := m.leases[id]
	if !ok || !now.Before(lease.ExpiresAt) {
		delete(m.leases, id)
		delete(m.states, id)
		delete(m.checkpoints, id)
		return Checkpoint{}, ErrNotFound
	}
	checkpoint, ok := m.checkpoints[id]
	if !ok {
		return Checkpoint{}, ErrNotFound
	}
	if checkpoint.digest != sha256.Sum256(state) {
		return Checkpoint{}, ErrStateSuperseded
	}
	if now.Before(checkpoint.StoredAt) {
		return Checkpoint{}, ErrUnsafeCheckpointClock
	}
	checkpoint.Now = now
	checkpoint.Age = now.Sub(checkpoint.StoredAt)
	return checkpoint, nil
}
