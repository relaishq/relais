package operatorgates

import (
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

type HostReadTiming struct {
	Operation   operator.Operation `json:"operation"`
	Target      string             `json:"target"`
	Action      string             `json:"action"`
	StartedNS   int64              `json:"startedNs"`
	CompletedNS int64              `json:"completedNs"`
	StartedAt   time.Time          `json:"startedAt"`
	CompletedAt time.Time          `json:"completedAt"`
}

// One session-owned monotonic timestamp covers batches, serial fallback and
// process-status reads. Exchange records completion before releasing its send
// lock; lease acceptance extends it through the remaining lease processing.
func (s *Session) completeHostRead() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastHostReadDone = max(s.lastHostReadDone, s.clock.Elapsed())
	return s.lastHostReadDone
}

func (s *Session) hostReadCompleted() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastHostReadDone
}
