package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
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

type observedResumeWorker struct {
	*fakeWorker
	opts  mediaworker.ResumeOptions
	state []byte
	err   error
}

func (w *observedResumeWorker) ResumeSession(state []byte, opts mediaworker.ResumeOptions) (string, error) {
	w.opts, w.state = opts, append([]byte(nil), state...)
	if w.err != nil {
		return "", w.err
	}
	if _, err := w.store.Renew(context.Background(), opts.Lease, time.Minute); err != nil {
		return "", err
	}
	w.mu.Lock()
	w.running[opts.Lease.SessionID] = true
	w.mu.Unlock()
	return opts.Lease.SessionID, nil
}
func checkpointRecovery(t *testing.T, age time.Duration) (*Plane, *observedResumeWorker, sessionstore.Lease) {
	t.Helper()
	p, _, b, _ := setup(t)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	// Store-clock samples are deliberately unrelated to wall-clock time.
	now := time.Unix(1234567, 0)
	state := takeoverSnapshot(t, id, 0)
	var wire map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(state, &wire))
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire["State"], &body))
	body["Checkpoint"], err = json.Marshal(mediaworker.CheckpointState{CapturedAt: now.Add(-age), Attempts: 4, Successes: 3, Failures: 1})
	require.NoError(t, err)
	wire["State"], err = json.Marshal(body)
	require.NoError(t, err)
	state, err = json.Marshal(wire)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(ctx, lease, state))
	p.store = &checkpointClockStore{Store: p.store, checkpoint: sessionstore.Checkpoint{Now: now, StoredAt: now.Add(-100 * time.Millisecond), Age: 100 * time.Millisecond}}
	target := &observedResumeWorker{fakeWorker: b}
	p.workers["b"].worker = target
	expireObservedWorker(t, p.workers["a"])
	return p, target, lease
}
func TestCheckpointDecisionUsesStoreClockAndCopyAge(t *testing.T) {
	for _, scenario := range []string{"scaled", "unsafe-age", "backward-clock"} {
		t.Run(scenario, func(t *testing.T) {
			age := 1500 * time.Millisecond
			if scenario == "unsafe-age" {
				age = 10 * time.Second
			}
			p, target, lease := checkpointRecovery(t, age)
			if scenario == "backward-clock" {
				p.store.(*checkpointClockStore).err = sessionstore.ErrUnsafeCheckpointClock
			}
			outcome := p.recovery.recover(context.Background(), p.workers["a"], lease, time.Now())
			require.NotNil(t, outcome)
			if scenario != "scaled" {
				require.True(t, outcome.Lost)
				require.Empty(t, target.state)
				require.Equal(t, "definitive-loss", outcome.CheckpointPolicy)
				return
			}
			require.False(t, outcome.Lost)
			require.InDelta(t, float64(100*time.Millisecond), float64(target.opts.CheckpointAge), float64(50*time.Millisecond))
			require.InDelta(t, float64(age), float64(target.opts.SnapshotAge), float64(50*time.Millisecond))
			require.Greater(t, target.opts.SequenceMargin, uint16(8192))
			require.InDelta(t, 15064, target.opts.CallerSequenceReserve, 100, "reserve includes copy age plus adoption deadline")
			require.Equal(t, 0.75, outcome.Checkpoint.SuccessRate())
		})
	}
}
func TestEnvelopeFlagDoesNotRelabelUnrelatedLoss(t *testing.T) {
	for _, cause := range []error{sessionstore.ErrNotFound, ErrNoTarget, mediaworker.ErrSequenceBudgetExhausted, mediaworker.ErrDTLSSequenceExhausted} {
		t.Run(cause.Error(), func(t *testing.T) {
			p, target, lease := checkpointRecovery(t, 1500*time.Millisecond)
			target.err = cause
			before := checkpointPolicyFromScrape(t, p, "definitive-loss")
			outcome := p.recovery.recover(context.Background(), p.workers["a"], lease, time.Now())
			require.NotNil(t, outcome)
			require.True(t, outcome.Lost)
			if cause == mediaworker.ErrSequenceBudgetExhausted || cause == mediaworker.ErrDTLSSequenceExhausted {
				require.Equal(t, "definitive-loss", outcome.CheckpointPolicy)
				require.Equal(t, before+1, checkpointPolicyFromScrape(t, p, "definitive-loss"))
			} else {
				require.Empty(t, outcome.CheckpointPolicy)
				require.Equal(t, before, checkpointPolicyFromScrape(t, p, "definitive-loss"))
			}
		})
	}
}
func TestCheckpointInvalidStateIsTerminal(t *testing.T) {
	p, target, lease := checkpointRecovery(t, time.Millisecond)
	require.NoError(t, p.store.PutState(context.Background(), lease, []byte("not JSON")))
	outcome := p.recovery.recover(context.Background(), p.workers["a"], lease, time.Now())
	require.NotNil(t, outcome)
	require.True(t, outcome.Lost)
	require.True(t, strings.Contains(outcome.Error, errUnsafeCheckpointState.Error()))
	require.Empty(t, target.state)
}

// A newer counter copy must not erase the caller's original outage. Rebuild
// supplies that durable start time through the same interface as a restart.
func TestCallerReserveKeepsOriginalOutage(t *testing.T) {
	for _, outage := range []time.Duration{0, 3 * time.Second, 10 * time.Second} {
		t.Run(outage.String(), func(t *testing.T) {
			p, target, lease := checkpointRecovery(t, 150*time.Millisecond)
			clock := p.store.(*checkpointClockStore)
			p.store = clock.Store
			authorizeTestControl(t, p)
			p.store = clock
			ctx := context.Background()
			intent := sessionstore.ControlIntent{ID: lease.SessionID, Token: "outage", Kind: "move", Lease: lease, Started: time.Now().Add(-outage)}
			require.NoError(t, p.restart.store.PutControlIntent(ctx, intent))
			require.NoError(t, p.recovery.rebuild(ctx, intent, nil))
			status, err := p.Status(ctx)
			require.NoError(t, err)
			if outage == 10*time.Second {
				require.Empty(t, target.state, "an exhausted original outage must never resume a fresh copy")
				require.EqualValues(t, 1, status.LostCount)
				require.Len(t, status.Takeovers, 1)
				require.Equal(t, "checkpoint_unsafe", status.Takeovers[0].LossCause)
				return
			}
			require.Zero(t, status.LostCount)
			require.NotEmpty(t, target.state)
			if outage == 0 {
				require.EqualValues(t, 10064, target.opts.CallerSequenceReserve, "fresh recovery keeps the configured minimum reserve")
			} else {
				require.InDelta(t, 22564, target.opts.CallerSequenceReserve, 100, "reserve covers the original outage plus the adoption deadline")
			}
		})
	}
}
