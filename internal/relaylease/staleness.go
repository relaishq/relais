package relaylease

import (
	"errors"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
)

// staleness counts only intervals bounded by two successful polls of the same
// unchanged lease, capped at one renewal interval each. Brief failures add no
// time; a continuous failure streak of at least the renewal interval discards
// the evidence. All elapsed time uses
// the candidate's monotonic clock, never the store expiry clock.
type staleness struct {
	lease           sessionstore.RelayLease
	missing         bool
	lastSuccess     time.Time
	elapsed         time.Duration
	failureSince    time.Time
	previousSuccess bool
}

func (s *staleness) reset() { *s = staleness{} }
func (s *staleness) clearEvidence() {
	s.lease = sessionstore.RelayLease{}
	s.lastSuccess = time.Time{}
	s.elapsed = 0
	s.previousSuccess = false
}
func (s *staleness) observe(l sessionstore.RelayLease, missing bool, pollErr error, now time.Time, ttl, renew time.Duration) bool {
	if pollErr != nil && !missing {
		if s.failureSince.IsZero() {
			s.failureSince = now
		}
		if now.Sub(s.failureSince) >= renew {
			s.clearEvidence()
		}
		s.previousSuccess = false
		return false
	}
	if !s.failureSince.IsZero() {
		if now.Sub(s.failureSince) >= renew {
			s.clearEvidence()
		}
		s.failureSince = time.Time{}
	}
	if s.lastSuccess.IsZero() || s.missing != missing || l.Epoch != s.lease.Epoch || l.Holder != s.lease.Holder || l.Forwarder != s.lease.Forwarder || l.PreviousHolder != s.lease.PreviousHolder || !l.ExpiresAt.Equal(s.lease.ExpiresAt) {
		s.lease, s.missing, s.elapsed = l, missing, 0
	} else if s.previousSuccess {
		s.elapsed += min(now.Sub(s.lastSuccess), renew)
	}
	s.lastSuccess, s.previousSuccess = now, true
	return s.elapsed >= ttl
}

// Observation shares ADR 0004's reachable-observation rule with control's
// cross-slot loss detector. It grants no lease or forwarding authority.
type Observation struct{ staleness }

func (s *Observation) Observe(l sessionstore.RelayLease, err error, now time.Time, threshold time.Duration) bool {
	return s.observe(l, errors.Is(err, sessionstore.ErrNotFound), err, now, threshold, DefaultRenew)
}
