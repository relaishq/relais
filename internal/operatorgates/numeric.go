package operatorgates

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
)

// TimingRecorder writes independent diagnostic JSON Lines. Errors are retained
// for the owner to report after the session; they never enter media policy.
// At most 16 cumulative snapshots and 256 four-host refreshes are retained.
type TimingRecorder struct {
	Writer                             io.Writer
	mu                                 sync.Mutex
	err                                error
	latest                             *operatorevidence.NumericTiming
	snapshots, refreshes               int
	omittedSnapshots, omittedRefreshes int
}

func (r *TimingRecorder) write(value any) {
	if r.err == nil && r.Writer != nil {
		r.err = json.NewEncoder(r.Writer).Encode(value)
	}
}

func (r *TimingRecorder) RecordPool(pool operatorevidence.Pool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshots++
	latest := operatorevidence.TimingDiagnostics(pool)
	r.latest = &latest
	// Reserve one of the sixteen writes for the final cumulative snapshot.
	if r.snapshots <= 15 {
		r.writeSnapshot(false)
	} else if r.snapshots > 16 {
		r.omittedSnapshots++
	}
}

func (r *TimingRecorder) writeSnapshot(final bool) {
	r.write(struct {
		Type     string                          `json:"type"`
		Snapshot int                             `json:"snapshot"`
		Final    bool                            `json:"final"`
		Evidence *operatorevidence.NumericTiming `json:"evidence"`
	}{"numeric-timing", r.snapshots, final, r.latest})
}

func (r *TimingRecorder) RecordClocks(e clocks.Evidence) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refreshes >= 256 {
		r.omittedRefreshes++
		return
	}
	r.refreshes++
	rows := map[string]clocks.Sample{}
	for _, slot := range []string{"relay", "shared", "w1", "w2"} {
		if sample, ok := e.Clocks[slot]; ok {
			sample = clocks.PublicSample(sample)
			rows[slot] = sample
		}
	}
	e.Clocks = rows
	r.write(struct {
		Type     string          `json:"type"`
		Refresh  int             `json:"refresh"`
		Evidence clocks.Evidence `json:"evidence"`
	}{"host-clocks", r.refreshes, e})
}

// RecordLeaseRead retains the gate-owned send/receive ages even when the guard
// refuses the observation. PublicEvidence removes source labels and opaque cursors.
func (r *TimingRecorder) RecordLeaseRead(read LeaseRead, received time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refreshes >= 256 {
		r.omittedRefreshes++
		return
	}
	r.refreshes++
	sent := map[string]int64{}
	for _, slot := range []string{"relay", "shared", "w1", "w2"} {
		if at, ok := read.Sent[slot]; ok {
			sent[slot] = int64(at)
		}
	}
	r.write(struct {
		Type       string           `json:"type"`
		Refresh    int              `json:"refresh"`
		SentNS     map[string]int64 `json:"sentNs"`
		ReceivedNS int64            `json:"receivedNs"`
		Evidence   clocks.Evidence  `json:"evidence"`
	}{"clock-lease-observation", r.refreshes, sent, int64(received), clocks.PublicEvidence(read.Evidence)})
}

func (r *TimingRecorder) Finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.latest != nil {
		r.writeSnapshot(true)
		r.latest = nil
	}
	r.write(struct {
		Type             string `json:"type"`
		OmittedSnapshots int    `json:"omitted_snapshots"`
		OmittedRefreshes int    `json:"omitted_refreshes"`
	}{"numeric-timing-end", r.omittedSnapshots, r.omittedRefreshes})
}

func (r *TimingRecorder) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

type timingJudge struct {
	MediaJudge
	recorder *TimingRecorder
}

func (j timingJudge) Judge(ctx context.Context, pool operatorevidence.Pool) (operatorevidence.Summary, error) {
	j.recorder.RecordPool(pool)
	return j.MediaJudge.Judge(ctx, pool)
}
