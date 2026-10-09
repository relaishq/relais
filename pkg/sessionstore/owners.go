// Package sessionstore is the session store: where session ownership (and,
// in later work, session state) lives outside the media workers.
//
// Today it holds only ownership: which media worker owns each session. The
// relay routes a caller's packets by it, so a worker records itself as a
// session's owner before the caller can send anything, and removes the
// record when the session ends. Ownership becomes a lease with expiry and a
// Redis implementation joins the in-memory one in later work; the interface
// stays small so both fit.
package sessionstore

import (
	"context"
	"errors"
	"net/netip"
	"sync"
)

// ErrNotFound is returned when a session has no owner.
var ErrNotFound = errors.New("sessionstore: session not found")

// Owners maps each session ID to the media worker that owns the session,
// identified by the private UDP address the relay forwards the session's
// packets to. A session ID is the worker's ICE username fragment, which is
// what the relay reads from a caller's STUN binding requests.
//
// Implementations are safe for concurrent use.
type Owners interface {
	// Claim records worker as the owner of a session, replacing any
	// previous owner.
	Claim(ctx context.Context, sessionID string, worker netip.AddrPort) error

	// Owner returns the worker that owns a session, or ErrNotFound.
	Owner(ctx context.Context, sessionID string) (netip.AddrPort, error)

	// Release removes a session's owner if it is still worker, so a worker
	// that no longer owns a session cannot remove its new owner. Releasing a
	// session that is not owned by worker does nothing.
	Release(ctx context.Context, sessionID string, worker netip.AddrPort) error
}

// Memory is an in-memory Owners for a single process: the call harness and
// the demo. The zero value is not usable; call NewMemory.
type Memory struct {
	mu     sync.RWMutex
	owners map[string]netip.AddrPort
}

var _ Owners = (*Memory)(nil)

// NewMemory returns an empty in-memory session-owner store.
func NewMemory() *Memory {
	return &Memory{owners: make(map[string]netip.AddrPort)}
}

// Claim implements Owners.
func (m *Memory) Claim(_ context.Context, sessionID string, worker netip.AddrPort) error {
	if sessionID == "" || !worker.IsValid() {
		return errors.New("sessionstore: claim needs a session ID and a worker address")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.owners[sessionID] = worker

	return nil
}

// Owner implements Owners.
func (m *Memory) Owner(_ context.Context, sessionID string) (netip.AddrPort, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	worker, ok := m.owners[sessionID]
	if !ok {
		return netip.AddrPort{}, ErrNotFound
	}

	return worker, nil
}

// Release implements Owners.
func (m *Memory) Release(_ context.Context, sessionID string, worker netip.AddrPort) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.owners[sessionID] == worker {
		delete(m.owners, sessionID)
	}

	return nil
}
