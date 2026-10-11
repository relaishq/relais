package sessionstore

import (
	"context"
	"errors"
	"time"
)

// RelayProcess identifies a same-host process. Owner is a fresh instance token;
// Start is the kernel process start time, not a wall-clock estimate.
type RelayProcess struct {
	Owner string `json:"owner"`
	PID   int    `json:"pid"`
	Start string `json:"start"`
}

// RelayLease retains the last possible forwarder even while a new holder is
// fencing it. Expiry never deletes identity evidence or resets the epoch.
type RelayLease struct {
	Key            string       `json:"key"`
	Holder         RelayProcess `json:"holder"`
	Epoch          uint64       `json:"epoch"`
	Expired        bool         `json:"expired"` // Evaluated by the store clock on read.
	ExpiresAt      time.Time    `json:"expires_at"`
	Forwarder      RelayProcess `json:"forwarder"`
	PreviousHolder RelayProcess `json:"previous_holder"`
}

// RelayLeases is independent of media snapshot access. Ordinary claims require
// expiry (or the identical holder); ClaimDeadRelay requires verified exit.
// Activate must precede
// binding, after both predecessor identities have been fenced. Renew can revive
// an expired tenure only when no successor has claimed it: expiry alone is not
// evidence of a second owner. All decisions are atomic in the store's clock.
type RelayLeases interface {
	ClaimRelay(context.Context, string, RelayProcess, time.Duration) (RelayLease, error)
	// ClaimDeadRelay bypasses expiry only after the caller verifies both the
	// recorded holder and forwarder are gone. CAS checks all identity evidence
	// and the epoch; a delayed renewal cannot revive the replaced tenure.
	ClaimDeadRelay(context.Context, RelayLease, RelayProcess, time.Duration) (RelayLease, error)
	RenewRelay(context.Context, RelayLease, time.Duration) (RelayLease, error)
	TransferRelay(context.Context, RelayLease, RelayProcess, time.Duration) (RelayLease, error)
	ActivateRelay(context.Context, RelayLease) (RelayLease, error)
	ReleaseRelay(context.Context, RelayLease) error
	GetRelay(context.Context, string) (RelayLease, error)
}

func validRelay(key string, p RelayProcess, ttl time.Duration) bool {
	return key != "" && p.Owner != "" && p.PID > 0 && p.Start != "" && ttl >= time.Millisecond
}
func sameRelay(a, b RelayLease) bool {
	return a.Key == b.Key && a.Holder == b.Holder && a.Epoch == b.Epoch
}

func (m *Memory) ClaimRelay(ctx context.Context, key string, p RelayProcess, ttl time.Duration) (RelayLease, error) {
	return m.changeRelay(ctx, "claim", RelayLease{Key: key}, p, ttl)
}
func (m *Memory) ClaimDeadRelay(ctx context.Context, l RelayLease, p RelayProcess, ttl time.Duration) (RelayLease, error) {
	return m.changeRelay(ctx, "claim_dead", l, p, ttl)
}
func (m *Memory) RenewRelay(ctx context.Context, l RelayLease, ttl time.Duration) (RelayLease, error) {
	return m.changeRelay(ctx, "renew", l, l.Holder, ttl)
}
func (m *Memory) TransferRelay(ctx context.Context, l RelayLease, p RelayProcess, ttl time.Duration) (RelayLease, error) {
	return m.changeRelay(ctx, "transfer", l, p, ttl)
}
func (m *Memory) ActivateRelay(ctx context.Context, l RelayLease) (RelayLease, error) {
	return m.changeRelay(ctx, "activate", l, l.Holder, time.Millisecond)
}

// ReleaseRelay retains the epoch but clears identities only after forwarding
// and sockets have stopped. A stale release cannot clear a successor.
func (m *Memory) ReleaseRelay(ctx context.Context, l RelayLease) error {
	_, err := m.changeRelay(ctx, "release", l, l.Holder, time.Millisecond)
	return err
}
func (m *Memory) GetRelay(ctx context.Context, key string) (RelayLease, error) {
	if err := ctx.Err(); err != nil {
		return RelayLease{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	l, ok := m.relayLeases[key]
	if !ok {
		return RelayLease{}, ErrNotFound
	}
	l.Expired = !time.Now().Before(l.ExpiresAt)
	return l, nil
}
func (m *Memory) changeRelay(ctx context.Context, op string, expected RelayLease, p RelayProcess, ttl time.Duration) (RelayLease, error) {
	if err := ctx.Err(); err != nil {
		return RelayLease{}, err
	}
	if !validRelay(expected.Key, p, ttl) {
		return RelayLease{}, errors.New("sessionstore: invalid relay lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.relayLeases[expected.Key]
	now := time.Now()
	if op == "claim_dead" {
		if !ok || !sameRelay(l, expected) || l.Forwarder != expected.Forwarder || l.PreviousHolder != expected.PreviousHolder {
			return RelayLease{}, ErrLeaseLost
		}
	} else if op == "claim" {
		if ok && l.Holder == p {
			return l, nil
		}
		if ok && now.Before(l.ExpiresAt) {
			return RelayLease{}, ErrLeaseHeld
		}
	} else if op == "renew" {
		if !ok {
			// No recorded successor: restore this tenure atomically, keeping its epoch.
			l = expected
			l.Forwarder = l.Holder
			l.PreviousHolder = RelayProcess{}
		} else if l.Holder != expected.Holder || l.Epoch > expected.Epoch {
			return RelayLease{}, ErrLeaseLost
		}
		l.Epoch = expected.Epoch
	} else if !ok || !sameRelay(l, expected) || op != "renew" && op != "release" && !now.Before(l.ExpiresAt) {
		return RelayLease{}, ErrLeaseLost
	}
	switch op {
	case "claim", "claim_dead", "transfer":
		l.PreviousHolder = l.Holder
		l.Holder = p
		l.Key = expected.Key
		l.Epoch++
		l.ExpiresAt = now.Add(ttl)
	case "renew":
		l.ExpiresAt = now.Add(ttl)
	case "release":
		l.Holder = RelayProcess{}
		l.Forwarder = RelayProcess{}
		l.PreviousHolder = RelayProcess{}
		l.ExpiresAt = now
	case "activate":
		l.Forwarder = l.Holder
		l.PreviousHolder = RelayProcess{}
	}
	if m.relayLeases == nil {
		m.relayLeases = make(map[string]RelayLease)
	}
	l.Expired = !now.Before(l.ExpiresAt)
	m.relayLeases[l.Key] = l
	return l, nil
}
