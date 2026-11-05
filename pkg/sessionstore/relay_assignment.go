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
//
// A CASRelay that clears a pending slot retires the pending generation
// without a switch. CommitRelay is the owning worker's switch commit. The store
// records each in the same atomic write (RelayCommit), so confirmation never
// mistakes a retirement for a switch (ADR 0016 sections 6 and 7).
type RelayAssignments interface {
	RelayOwners
	CASRelay(context.Context, Lease, RelayAssignment, RelayAssignment) error
	CommitRelay(context.Context, Lease, RelayAssignment, RelayAssignment) error
}

// RelayCommit records how pending generations were consumed: Switched is the
// highest generation a worker committed after switching (SwitchedAt is the
// store time of that write); Retired is the highest generation consumed
// without a switch.
type RelayCommit struct {
	Switched   uint64
	SwitchedAt time.Time
	Retired    uint64
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

func validRelayCommit(from, to RelayAssignment) bool {
	return validRelayTransition(from, to) && from.Pending != "" && to.Slot == from.Pending
}

func (m *Memory) CASRelay(ctx context.Context, lease Lease, from, to RelayAssignment) error {
	return m.casRelay(ctx, lease, from, to, false)
}

func (m *Memory) CommitRelay(ctx context.Context, lease Lease, from, to RelayAssignment) error {
	if !validRelayCommit(from, to) {
		return ErrRelayTransition
	}
	return m.casRelay(ctx, lease, from, to, true)
}

func (m *Memory) casRelay(ctx context.Context, lease Lease, from, to RelayAssignment, commit bool) error {
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
	if from.Pending != "" {
		if m.relayCommits == nil {
			m.relayCommits = make(map[string]RelayCommit)
		}
		record := m.relayCommits[lease.SessionID]
		if commit {
			record.Switched, record.SwitchedAt = to.Generation, time.Now()
		} else {
			record.Retired = to.Generation
		}
		m.relayCommits[lease.SessionID] = record
	}
	return nil
}

var _ RelayAssignments = (*Memory)(nil)
var _ RelayAssignments = (*Redis)(nil)
var _ RelayOwners = (*RedisOwners)(nil)
