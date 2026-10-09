// Package sessionstore keeps fenced session ownership outside the media
// workers, together with their resumable state blobs. Each ownership change
// advances an epoch, so an old worker can
// neither renew nor release its successor's lease. Transitions compare the
// owner and epoch atomically across both Memory and Redis.
// The session ID is the worker's ICE username fragment, read by the relay
// from the caller's binding requests.
//
// PutState atomically checks an unexpired owner/epoch and copies the blob.
// GetState returns an isolated copy; Transfer retains it. Matching Release
// and expiry delete both lease and blob. Stale puts return ErrLeaseLost and
// stale releases cannot delete a successor's state. Memory stores copies in
// process; Redis seals state with AES-256-GCM.
package sessionstore

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"
)

var (
	// ErrNotFound means no live lease exists, including an expired lease.
	ErrNotFound = errors.New("sessionstore: session not found")
	// ErrLeaseHeld means Claim found an existing record, even if expired.
	ErrLeaseHeld = errors.New("sessionstore: lease held")
	// ErrLeaseLost means an epoch or owner is stale, or the lease expired.
	ErrLeaseLost = errors.New("sessionstore: lease lost")
)

// Lease identifies one tenure of a session's private relay-leg owner.
type Lease struct {
	SessionID string         `json:"session_id"`
	Worker    netip.AddrPort `json:"worker"`
	Epoch     uint64         `json:"epoch"`
	ExpiresAt time.Time      `json:"expires_at"`
}

// Owners is the address-only lookup used on the relay's asynchronous path.
type Owners interface {
	// Owner returns the live worker address, or ErrNotFound after expiry.
	Owner(ctx context.Context, sessionID string) (netip.AddrPort, error)
}

// Store transitions are atomic compare-and-set operations. Expired leases
// cannot renew or transfer. Claim refuses any existing record, including an
// expired one. Lookups prune their session; listing prunes only the listed
// worker. Redis metadata also expires after configured retention. After pruning,
// the ID can be claimed again with a fresh global epoch. This does not resume or recover
// the expired session. Stale release is ignored, and epochs strictly increase
// on reclaim.
type Store interface {
	Owners

	// PutState atomically checks the current unexpired lease and replaces the
	// resumable blob. Stale writers receive ErrLeaseLost. Bytes are copied.
	// Redis rejects a delayed superseded put with ErrStateSuperseded; that
	// error does not revoke the current lease.
	PutState(ctx context.Context, lease Lease, state []byte) error

	// GetState returns a copy of the latest blob for a live lease, or
	// ErrNotFound when there is no snapshot. Transfer preserves the blob.
	GetState(ctx context.Context, sessionID string) ([]byte, error)

	// Claim creates a lease for an unknown session. Any existing record,
	// including an expired one, returns ErrLeaseHeld. It never transfers a
	// recorded lease to a new owner. Expired records are pruned by lookups
	// and listing; claiming an ID after pruning receives a fresh global epoch.
	Claim(ctx context.Context, sessionID string, worker netip.AddrPort, ttl time.Duration) (Lease, error)

	// Renew extends expiry only for the current unexpired owner and epoch.
	// A stale or expired token returns ErrLeaseLost.
	Renew(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error)

	// Transfer atomically changes an unexpired lease's owner and increases
	// its epoch. A stale token returns ErrLeaseLost without changing anything.
	Transfer(ctx context.Context, from Lease, to netip.AddrPort, ttl time.Duration) (Lease, error)

	// Get returns the current unexpired lease, or ErrNotFound. Expired records
	// can be deleted here; the old token remains invalid after any new claim.
	Get(ctx context.Context, sessionID string) (Lease, error)

	// Release removes only the matching owner and epoch. Stale release is a
	// successful no-op, including when the address has become owner again.
	Release(ctx context.Context, lease Lease) error

	// ListByWorker returns only unexpired leases, pruning expired records only
	// for the listed worker. Redis metadata also expires after retention.
	ListByWorker(ctx context.Context, worker netip.AddrPort) ([]Lease, error)
}

// WorkerIndexRefresher is optional for stores with expiring worker indexes.
// The worker calls it once per renewal tick, alongside the lease renewals.
// Index lifetime must not depend on best-effort per-session repair jobs.
type WorkerIndexRefresher interface {
	RefreshWorkerIndex(context.Context, netip.AddrPort, time.Duration) error
}

// Memory is a single-process store. Use NewMemory, not its zero value.
type Memory struct {
	mu     sync.RWMutex
	leases map[string]Lease
	states map[string][]byte
	epoch  uint64 // global; no per-session history survives release
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty in-memory fenced store.
func NewMemory() *Memory {
	return &Memory{leases: make(map[string]Lease), states: make(map[string][]byte)}
}

func valid(id string, worker netip.AddrPort, ttl time.Duration) bool {
	return id != "" && worker.IsValid() && ttl > 0
}

func same(a, b Lease) bool {
	return a.SessionID == b.SessionID && a.Worker == b.Worker && a.Epoch == b.Epoch
}

func (m *Memory) next(id string, worker netip.AddrPort, ttl time.Duration) Lease {
	m.epoch++
	lease := Lease{SessionID: id, Worker: worker, Epoch: m.epoch, ExpiresAt: time.Now().Add(ttl)}
	m.leases[id] = lease
	return lease
}

// Claim implements Store.
func (m *Memory) Claim(ctx context.Context, id string, worker netip.AddrPort, ttl time.Duration) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	if !valid(id, worker, ttl) {
		return Lease{}, errors.New("sessionstore: claim needs a session ID, address and positive TTL")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.leases[id]; ok {
		return Lease{}, ErrLeaseHeld
	}

	return m.next(id, worker, ttl), nil
}

// Renew implements Store.
func (m *Memory) Renew(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	if ttl <= 0 {
		return Lease{}, errors.New("sessionstore: positive TTL required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	current, ok := m.leases[lease.SessionID]
	if !ok || !same(current, lease) || !time.Now().Before(current.ExpiresAt) {
		return Lease{}, ErrLeaseLost
	}

	current.ExpiresAt = time.Now().Add(ttl)
	m.leases[lease.SessionID] = current
	return current, nil
}

// Transfer implements Store.
func (m *Memory) Transfer(ctx context.Context, from Lease, to netip.AddrPort, ttl time.Duration) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	if !valid(from.SessionID, to, ttl) || from.Worker == to {
		return Lease{}, errors.New("sessionstore: transfer needs a different valid worker and positive TTL")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	current, ok := m.leases[from.SessionID]
	if !ok || !same(current, from) || !time.Now().Before(current.ExpiresAt) {
		return Lease{}, ErrLeaseLost
	}

	return m.next(from.SessionID, to, ttl), nil
}

// Get implements Store.
func (m *Memory) Get(ctx context.Context, id string) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	lease, ok := m.leases[id]
	if !ok || !time.Now().Before(lease.ExpiresAt) {
		delete(m.leases, id)
		delete(m.states, id)
		return Lease{}, ErrNotFound
	}

	return lease, nil
}

// Owner implements Store.
func (m *Memory) Owner(ctx context.Context, id string) (netip.AddrPort, error) {
	lease, err := m.Get(ctx, id)
	return lease.Worker, err
}

// Release implements Store.
func (m *Memory) Release(ctx context.Context, lease Lease) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if current, ok := m.leases[lease.SessionID]; ok && same(current, lease) {
		delete(m.leases, lease.SessionID)
		delete(m.states, lease.SessionID)
	}

	return nil
}

// ListByWorker implements Store.
func (m *Memory) ListByWorker(ctx context.Context, worker netip.AddrPort) ([]Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	leases := []Lease{}
	now := time.Now()
	for _, lease := range m.leases {
		if lease.Worker != worker {
			continue
		}
		if !now.Before(lease.ExpiresAt) {
			delete(m.leases, lease.SessionID)
			delete(m.states, lease.SessionID)
			continue
		}
		if lease.Worker == worker {
			leases = append(leases, lease)
		}
	}

	return leases, nil
}

// PutState implements Store. The lease check and blob write share a lock;
// Redis can implement the same operation with one script.
func (m *Memory) PutState(ctx context.Context, lease Lease, state []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.leases[lease.SessionID]
	if !ok || !same(current, lease) || !time.Now().Before(current.ExpiresAt) {
		return ErrLeaseLost
	}
	m.states[lease.SessionID] = bytes.Clone(state)
	return nil
}

// GetState implements Store.
func (m *Memory) GetState(ctx context.Context, id string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, ok := m.leases[id]
	if !ok || !time.Now().Before(lease.ExpiresAt) {
		delete(m.leases, id)
		delete(m.states, id)
		return nil, ErrNotFound
	}
	state, ok := m.states[id]
	if !ok {
		return nil, ErrNotFound
	}
	return bytes.Clone(state), nil
}
