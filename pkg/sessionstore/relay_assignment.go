package sessionstore

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
)

// RelayAssignment authorizes a call on its current and optional pending slot.
// Generation is independent of the worker lease epoch. Pending names the slot
// for Generation+1. A zero assignment is the legacy, unassigned form.
type RelayAssignment struct {
	Slot       string `json:"relay"`
	Generation uint64 `json:"relay_gen"`
	Pending    string `json:"relay_pending,omitempty"`
}

var ErrRelayConflict = errors.New("sessionstore: relay assignment changed")
var ErrRelayTransition = errors.New("sessionstore: invalid relay assignment transition")

// RelayAssignments is an optional capability so legacy Store implementations
// remain usable. Reads expose no media keys. CASRelay is fenced by both the
// live worker tenure and the complete expected assignment. An uncertain write
// must be settled with RelayOwner; it must not be treated as a refusal.
type RelayAssignments interface {
	RelayOwners
	CASRelay(context.Context, Lease, RelayAssignment, RelayAssignment) error
}

// RelayOwners is the keyless, atomic owner-and-assignment lookup for relays.
type RelayOwners interface {
	RelayOwner(context.Context, string) (Lease, RelayAssignment, error)
}

func (a RelayAssignment) Allows(slot string) bool {
	return slot != "" && (a.Slot == slot || a.Pending == slot)
}

func validRelayAssignment(a RelayAssignment) bool {
	if a.Slot == "" {
		return a == (RelayAssignment{})
	}
	return len(a.Slot) <= 256 && len(a.Pending) <= 256 &&
		!strings.ContainsAny(a.Slot+a.Pending, "\x00\r\n")
}

func validRelayTransition(from, to RelayAssignment) bool {
	if !validRelayAssignment(from) || !validRelayAssignment(to) || to.Slot == "" {
		return false
	}
	if from.Slot == "" {
		return to.Generation == 0 && to.Pending == ""
	}
	if from.Generation == math.MaxUint64 {
		return false
	}
	if from.Pending == "" {
		return to.Slot == from.Slot && to.Generation == from.Generation && to.Pending != ""
	}
	return to.Pending == "" && to.Generation == from.Generation+1 &&
		(to.Slot == from.Pending || to.Slot == from.Slot)
}

func (m *Memory) RelayOwner(ctx context.Context, id string) (Lease, RelayAssignment, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, RelayAssignment{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	lease, ok := m.leases[id]
	if !ok || !time.Now().Before(lease.ExpiresAt) {
		return Lease{}, RelayAssignment{}, ErrNotFound
	}
	return lease, m.relayAssignments[id], nil
}

func (m *Memory) CASRelay(ctx context.Context, lease Lease, from, to RelayAssignment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validRelayTransition(from, to) {
		return ErrRelayTransition
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.leases[lease.SessionID]
	if !ok || !same(current, lease) || !time.Now().Before(current.ExpiresAt) {
		return ErrLeaseLost
	}
	if m.relayAssignments[lease.SessionID] != from {
		return ErrRelayConflict
	}
	if m.relayAssignments == nil {
		m.relayAssignments = make(map[string]RelayAssignment)
	}
	m.relayAssignments[lease.SessionID] = to
	return nil
}

var _ RelayAssignments = (*Memory)(nil)
var _ RelayAssignments = (*Redis)(nil)
var _ RelayOwners = (*RedisOwners)(nil)
