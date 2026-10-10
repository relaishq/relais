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

func TestCheckpointInvalidStateIsTerminal(t *testing.T) {
	now := time.Now()
	p := NewWithConfig(nil, &checkpointClockStore{checkpoint: sessionstore.Checkpoint{Now: now, StoredAt: now}}, Config{})
	_, err := p.checkpointDecision(context.Background(), "bad", []byte("not JSON"), 0)
	require.ErrorIs(t, err, errUnsafeCheckpointState)
}
