package controlplane

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type checkpointClockStore struct {
	sessionstore.Store
	checkpoint sessionstore.Checkpoint
	err        error
}

func (s *checkpointClockStore) Checkpoint(context.Context, string, []byte) (sessionstore.Checkpoint, error) {
	return s.checkpoint, s.err
}

func TestCheckpointDecisionUsesStoreClockAndCopyAge(t *testing.T) {
	// The store clock is years away from the worker's wall clock.
	now := time.Unix(1234567, 0)
	state := takeoverSnapshot(t, "clock", 0)
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(state, &envelope))
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(envelope["State"], &fields))
	checkpoint, err := json.Marshal(mediaworker.CheckpointState{CapturedAt: now.Add(-1500 * time.Millisecond), Attempts: 4, Successes: 3, Failures: 1})
	require.NoError(t, err)
	fields["Checkpoint"] = checkpoint
	envelope["State"], err = json.Marshal(fields)
	require.NoError(t, err)
	state, err = json.Marshal(envelope)
	require.NoError(t, err)
	store := &checkpointClockStore{checkpoint: sessionstore.Checkpoint{Now: now, StoredAt: now.Add(-100 * time.Millisecond), Age: 100 * time.Millisecond}}
	p := NewWithConfig(nil, store, Config{})
	decision, err := p.checkpointDecision(context.Background(), "clock", state, 0)
	require.NoError(t, err)
	require.InDelta(t, float64(100*time.Millisecond), float64(decision.age), float64(50*time.Millisecond))
	require.InDelta(t, float64(1500*time.Millisecond), float64(decision.snapshotAge), float64(50*time.Millisecond))
	require.True(t, decision.outside)
	require.Greater(t, decision.margin, uint16(8192))
	require.EqualValues(t, 10064, decision.reserve)
	require.Equal(t, 0.75, decision.info.SuccessRate())
	deadlineCtx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	copyReserve, err := p.checkpointDecision(deadlineCtx, "clock", state, 200*time.Millisecond)
	require.NoError(t, err)
	require.InDelta(t, 15064, copyReserve.reserve, 100, "reserve includes old copy age plus remaining adoption time, despite a recent heartbeat")
	longer, err := p.checkpointDecision(context.Background(), "clock", state, 3*time.Second)
	require.NoError(t, err)
	require.EqualValues(t, 15064, longer.reserve, "retry checkpoints cannot reset the caller outage")
	_, err = p.checkpointDecision(context.Background(), "clock", state, 10*time.Second)
	require.ErrorIs(t, err, mediaworker.ErrSequenceBudgetExhausted)
	store.err = sessionstore.ErrUnsafeCheckpointClock
	decision, err = p.checkpointDecision(context.Background(), "clock", state, 0)
	require.ErrorIs(t, err, sessionstore.ErrUnsafeCheckpointClock)
	require.True(t, decision.outside)
}

func TestEnvelopeFlagDoesNotRelabelUnrelatedLoss(t *testing.T) {
	for _, cause := range []error{sessionstore.ErrNotFound, ErrNoTarget, mediaworker.ErrSequenceBudgetExhausted} {
		t.Run(cause.Error(), func(t *testing.T) {
			p, _, _, _ := setup(t)
			id, _, err := p.Create(context.Background(), "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(context.Background(), id)
			require.NoError(t, err)
			source := p.workers["a"]
			source.pending = map[string]*takeoverState{id: {envelope: true}}
			res := MoveResult{Kind: "takeover", ID: id, Start: time.Now()}
			p.completeTakeover(source, p.calls[id], lease, &res, true, cause)
			if cause == mediaworker.ErrSequenceBudgetExhausted {
				require.Equal(t, "definitive-loss", res.CheckpointPolicy)
			} else {
				require.Empty(t, res.CheckpointPolicy)
			}
		})
	}
}

func TestCheckpointInvalidStateIsTerminal(t *testing.T) {
	now := time.Now()
	p := NewWithConfig(nil, &checkpointClockStore{checkpoint: sessionstore.Checkpoint{Now: now, StoredAt: now}}, Config{})
	_, err := p.checkpointDecision(context.Background(), "bad", []byte("not JSON"), 0)
	require.ErrorIs(t, err, errUnsafeCheckpointState)
}
