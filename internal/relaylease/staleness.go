package relaylease

import (
	"time"

	"github.com/relais/pkg/sessionstore"
)

// staleness observes one unchanged lease across successful store polls. Its
// elapsed time comes from local monotonic time, never the Redis expiry clock.
// A failed poll discards all evidence: time with an unreachable store cannot
// authorize fencing when access returns.
type staleness struct {
	lease   sessionstore.RelayLease
	since   time.Time
	missing bool
}

func (s *staleness) reset() { *s = staleness{} }
func (s *staleness) observe(l sessionstore.RelayLease, missing bool, pollErr error, now time.Time, ttl time.Duration) bool {
	if pollErr != nil && !missing {
		s.reset()
		return false
	}
	if s.since.IsZero() || s.missing != missing || l.Epoch != s.lease.Epoch || l.Holder != s.lease.Holder || l.Forwarder != s.lease.Forwarder || l.PreviousHolder != s.lease.PreviousHolder || !l.ExpiresAt.Equal(s.lease.ExpiresAt) {
		s.lease, s.missing, s.since = l, missing, now
		return false
	}
	return now.Sub(s.since) >= ttl
}
