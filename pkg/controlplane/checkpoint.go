package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
)

var errUnsafeCheckpointState = errors.New("controlplane: invalid checkpoint state")

type checkpointDecision struct {
	age, snapshotAge time.Duration
	storedAt         time.Time
	info             mediaworker.CheckpointState
	margin           uint16
	rtcpMargin       uint32
	outside          bool
	reserve          uint32
}

func (p *Plane) checkpointDecision(ctx context.Context, id string, state []byte, outage time.Duration) (checkpointDecision, error) {
	// A concurrent replacement must never date older bytes with newer metadata.
	started := time.Now()
	checkpoint, err := p.store.Checkpoint(ctx, id, state)
	if err != nil {
		return checkpointDecision{outside: errors.Is(err, sessionstore.ErrUnsafeCheckpointClock)}, err
	}
	info, err := mediaworker.SnapshotCheckpoint(state)
	if err != nil {
		return checkpointDecision{}, fmt.Errorf("%w: %w", errUnsafeCheckpointState, err)
	}
	decision := checkpointDecision{age: checkpoint.Age, snapshotAge: checkpoint.Age, storedAt: checkpoint.StoredAt, info: info}
	if !info.CapturedAt.IsZero() {
		if info.CapturedAt.After(checkpoint.StoredAt) {
			decision.outside = true
			return decision, mediaworker.ErrSequenceBudgetExhausted
		}
		decision.snapshotAge += checkpoint.StoredAt.Sub(info.CapturedAt)
	}
	// Include reply latency conservatively; only elapsed local time is used.
	latency := time.Since(started)
	decision.age += latency
	decision.snapshotAge += latency
	decision.margin, decision.rtcpMargin, decision.outside, err = p.config.CheckpointEnvelope.CheckpointMargins(decision.snapshotAge, info, p.config.SequenceMargin, p.config.SRTCPIndexMargin)
	if err == nil {
		decision.reserve = p.config.CheckpointEnvelope.CallerSequenceReserve(info, outage)
		decision.outside = decision.outside || decision.reserve > p.config.CheckpointEnvelope.CallerSequenceReserve(info, 0)
		if decision.reserve >= 1<<15 {
			err = mediaworker.ErrSequenceBudgetExhausted
		}
	}
	return decision, err
}

// Reserve through the current adoption deadline as well as the original
// outage. Retry snapshots cannot reset the caller's uninterrupted source gap.
func checkpointOutageBudget(ctx context.Context, started time.Time) time.Duration {
	elapsed := time.Since(started)
	if deadline, ok := ctx.Deadline(); ok {
		elapsed += max(time.Until(deadline), 0)
	}
	return elapsed
}
