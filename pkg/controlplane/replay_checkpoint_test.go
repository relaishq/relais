package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type checkpointReplayRelay struct {
	*fakeRelay
	gated, forgotten bool
	incompletePlan   bool
	prepared         []map[uint32]uint64
	sentOnLoss       int
	replayResult     relay.ReplayResult
	replayErr        error
}

func (r *checkpointReplayRelay) BeginReplay(_ context.Context, _ string, _ netip.AddrPort, inbound map[uint32]uint64) (relay.ReplayPlan, error) {
	r.gated = true
	if inbound != nil {
		r.prepared = append(r.prepared, inbound)
	}
	return relay.ReplayPlan{Complete: inbound != nil && !r.incompletePlan}, nil
}
func (r *checkpointReplayRelay) ReplaySession(context.Context, string, netip.AddrPort) (relay.ReplayResult, error) {
	r.gated = false
	if r.replayErr != nil || r.replayResult.Dropped > 0 || r.replayResult.SendFailures > 0 {
		return r.replayResult, r.replayErr
	}
	return relay.ReplayResult{Packets: 1, Complete: true}, nil
}
func (r *checkpointReplayRelay) ForgetSession(id string) {
	r.gated, r.forgotten = false, true
	r.fakeRelay.ForgetSession(id)
}
func (r *checkpointReplayRelay) ReleaseSession(string, netip.AddrPort) (int, error) {
	if r.gated {
		r.sentOnLoss++
	}
	r.gated = false
	return 0, nil
}

type replayCheckpointStore struct {
	sessionstore.Store
	t           *testing.T
	r           *checkpointReplayRelay
	target      netip.AddrPort
	replacement []byte
	reads       int
	validated   []byte
}

type replayCheckpointWorker struct {
	*takeoverWorker
	state       []byte
	opts        mediaworker.ResumeOptions
	keyframes   int
	keyframeErr error
}

func (w *replayCheckpointWorker) ResumeSession(state []byte, opts mediaworker.ResumeOptions) (string, error) {
	w.state, w.opts = bytes.Clone(state), opts
	return w.takeoverWorker.ResumeSession(state, opts)
}

func (s *replayCheckpointStore) GetState(ctx context.Context, id string) ([]byte, error) {
	require.True(s.t, s.r.gated, "gate precedes snapshot I/O")
	require.NotEmpty(s.t, s.r.moves, "route fencing precedes snapshot I/O")
	require.Equal(s.t, s.target, s.r.moves[len(s.r.moves)-1][1])
	s.reads++
	return s.Store.GetState(ctx, id)
}
func (s *replayCheckpointStore) Checkpoint(ctx context.Context, id string, state []byte) (sessionstore.Checkpoint, error) {
	if s.replacement != nil {
		lease, err := s.Get(ctx, id)
		require.NoError(s.t, err)
		require.NoError(s.t, s.PutState(ctx, lease, s.replacement))
		s.replacement = nil
		return sessionstore.Checkpoint{}, sessionstore.ErrStateSuperseded
	}
	s.validated = bytes.Clone(state)
	return s.Store.Checkpoint(ctx, id, state)
}

func replayCheckpointSnapshot(t *testing.T, id string, index uint64) []byte {
	t.Helper()
	var snapshot map[string]any
	require.NoError(t, json.Unmarshal(takeoverSnapshot(t, id, 0), &snapshot))
	state := snapshot["State"].(map[string]any)
	state["SRTP"].(map[string]any)["Inbound"] = map[uint32]uint64{42: index}
	// Counter copy predates the successful write: snapshot age, rather than
	// write age, must bound both the margin and possible duplicated input.
	state["Checkpoint"] = mediaworker.CheckpointState{CapturedAt: time.Now().Add(-1500 * time.Millisecond)}
	data, err := json.Marshal(snapshot)
	require.NoError(t, err)
	return data
}

func TestCheckpointReplayValidatesOneReadBeforeFiltering(t *testing.T) {
	p, _, baseB, baseR := setup(t)
	r := &checkpointReplayRelay{fakeRelay: baseR}
	p.relay = r
	target := &replayCheckpointWorker{takeoverWorker: &takeoverWorker{fakeWorker: baseB}}
	p.workers["b"].worker = target
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(ctx, lease, replayCheckpointSnapshot(t, id, 10)))
	store := &replayCheckpointStore{Store: p.store, t: t, r: r, target: baseB.addr, replacement: replayCheckpointSnapshot(t, id, 20)}
	p.store = store
	source := p.workers["a"]
	source.dead = true
	p.takeover(ctx, source, lease, time.Now())
	require.Empty(t, r.prepared, "a superseded checkpoint must not fix the replay filter")
	require.Empty(t, p.recentTakeovers(), "metadata mismatch is retried")
	lease, err = store.Get(ctx, id)
	require.NoError(t, err)
	p.takeover(ctx, source, lease, time.Now())
	require.Equal(t, 2, store.reads, "one blob read per attempt")
	require.Equal(t, []map[uint32]uint64{{42: 20}}, r.prepared)
	inbound, err := mediaworker.SnapshotInboundIndexes(store.validated)
	require.NoError(t, err)
	require.Equal(t, inbound, r.prepared[0])
	require.Equal(t, store.validated, target.state, "resume uses the metadata-validated replay checkpoint")
	events := p.recentTakeovers()
	require.Len(t, events, 1)
	require.False(t, events[0].Lost)
	require.Equal(t, "scaled", events[0].CheckpointPolicy)
	require.Greater(t, events[0].SnapshotAge, 1500*time.Millisecond)
	require.Greater(t, events[0].SequenceMargin, uint16(8192))
	require.Equal(t, events[0].SequenceMargin, target.opts.SequenceMargin)
	require.Equal(t, events[0].CheckpointAge, target.opts.CheckpointAge)
	require.Equal(t, events[0].SnapshotAge, target.opts.SnapshotAge)
	require.True(t, target.opts.RelayReplay)
	require.True(t, events[0].Result.InputMayBeDuplicated)
	require.Equal(t, events[0].SnapshotAge, events[0].Result.InputDuplicationWindow)
	require.Greater(t, events[0].Result.InputDuplicationWindow, events[0].CheckpointAge,
		"a delayed write must not shrink the repeated-input window")
}

func TestCheckpointReplayTerminalFailuresDiscardBeforeRelease(t *testing.T) {
	for _, failure := range []string{"envelope", "missing-snapshot", "invalid-snapshot", "resume", "no-target", "vanished-lease"} {
		t.Run(failure, func(t *testing.T) {
			p, _, baseB, baseR := setup(t)
			r := &checkpointReplayRelay{fakeRelay: baseR}
			p.relay = r
			p.workers["b"].worker = &takeoverWorker{fakeWorker: baseB, resumeFail: failure == "resume"}
			ctx := context.Background()
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			state := takeoverSnapshot(t, id, 0)
			if failure == "invalid-snapshot" {
				state = []byte("invalid")
			}
			if failure != "missing-snapshot" {
				require.NoError(t, p.store.PutState(ctx, lease, state))
			}
			if failure == "envelope" {
				now := time.Now()
				p.store = &checkpointClockStore{Store: p.store, checkpoint: sessionstore.Checkpoint{Now: now, StoredAt: now.Add(-4 * time.Second), Age: 4 * time.Second}}
			}
			if failure == "no-target" {
				p.workers["b"].draining = true
			}
			source := p.workers["a"]
			source.dead = true
			// A terminal retry finds an allocated gate. Forget must discard it
			// before a legacy release can send queued input to the failed leg.
			r.gated = true
			source.pending = map[string]*takeoverState{id: {call: p.calls[id], lease: lease, routed: lease.Worker, held: true,
				replayPlan: &relay.ReplayPlan{}, excluded: map[netip.AddrPort]bool{source.addr: true}, attemptLimit: maxResumeAttempts}}
			if failure == "vanished-lease" {
				require.NoError(t, p.store.Release(ctx, lease))
			}
			p.takeover(ctx, source, lease, time.Now())
			require.True(t, r.forgotten)
			require.False(t, r.gated)
			require.Zero(t, r.sentOnLoss, "terminal cleanup must not flush input to the failed target")
			require.Empty(t, p.calls)
			require.Empty(t, source.pending)
			require.Len(t, p.recentTakeovers(), 1)
			require.True(t, p.recentTakeovers()[0].Lost)
			if failure == "envelope" {
				require.Equal(t, "definitive-loss", p.recentTakeovers()[0].CheckpointPolicy)
			}
		})
	}
}

type replayCheckpointFailedTarget struct {
	*fakeWorker
	age time.Duration
}

func (w *replayCheckpointFailedTarget) ResumeSession(data []byte, opts mediaworker.ResumeOptions) (string, error) {
	w.age = opts.SnapshotAge
	var snapshot map[string]any
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return "", err
	}
	state := snapshot["State"].(map[string]any)
	audio := state["Audio"].(map[string]any)
	audio["AdvanceSinceSend"] = audio["AdvanceSinceSend"].(float64) + float64(opts.SequenceMargin)
	now, err := w.store.Clock(context.Background(), opts.Lease.SessionID)
	if err != nil {
		return "", err
	}
	state["Checkpoint"] = mediaworker.CheckpointState{CapturedAt: now}
	adjusted, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	if err = w.store.PutState(context.Background(), opts.Lease, adjusted); err != nil {
		return "", err
	}
	return "", mediaworker.ErrClosed // persisted reservation, failed adoption
}

func TestCheckpointReplayRetryCannotShrinkDuplicationWindow(t *testing.T) {
	p, _, baseB, baseR := setup(t)
	p.relay = &checkpointReplayRelay{fakeRelay: baseR}
	failed := &replayCheckpointFailedTarget{fakeWorker: baseB}
	p.workers["b"].worker = failed
	c := &fakeWorker{store: p.store, addr: netip.MustParseAddrPort("127.0.0.1:3"), running: map[string]bool{}}
	require.NoError(t, p.Register("c", c.addr, &takeoverWorker{fakeWorker: c}))
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	var snapshot map[string]any
	require.NoError(t, json.Unmarshal(replayCheckpointSnapshot(t, id, 10), &snapshot))
	snapshot["State"].(map[string]any)["Checkpoint"] = mediaworker.CheckpointState{CapturedAt: time.Now().Add(-time.Second)}
	state, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(ctx, lease, state))
	p.workers["a"].dead = true
	p.takeover(ctx, p.workers["a"], lease, time.Now())
	events := p.recentTakeovers()
	require.Len(t, events, 1)
	require.False(t, events[0].Lost)
	require.Equal(t, "c", events[0].To)
	require.Greater(t, failed.age, time.Second)
	require.Less(t, events[0].SnapshotAge, 100*time.Millisecond)
	require.True(t, events[0].Result.InputMayBeDuplicated)
	require.Equal(t, failed.age, events[0].Result.InputDuplicationWindow,
		"the old input queue still covers the original checkpoint, despite a new reservation write")
}

func (w *replayCheckpointWorker) RequestKeyframe(context.Context, string) error {
	w.keyframes++
	return w.keyframeErr
}

func TestCheckpointReplayLossRequestsWorkerKeyframe(t *testing.T) {
	for _, failure := range []string{"live-drop", "send-failure", "expiry", "relay-restart"} {
		t.Run(failure, func(t *testing.T) {
			p, _, baseB, baseR := setup(t)
			r := &checkpointReplayRelay{fakeRelay: baseR}
			switch failure {
			case "live-drop":
				r.replayResult.Dropped = 1
			case "send-failure":
				r.replayResult.SendFailures = 1
			default:
				r.replayResult.Expired = true
				r.replayErr = relay.ErrHoldExpired
			}
			p.relay = r
			target := &replayCheckpointWorker{takeoverWorker: &takeoverWorker{fakeWorker: baseB}}
			p.workers["b"].worker = target
			ctx := context.Background()
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
			p.workers["a"].dead = true
			p.takeover(ctx, p.workers["a"], lease, time.Now())
			require.True(t, target.opts.RelayReplay, "loss happens after adopting a complete plan")
			require.Equal(t, 1, target.keyframes, "video recovery must not wait for receiver feedback")
			require.Len(t, p.recentTakeovers(), 1)
			require.False(t, p.recentTakeovers()[0].Result.RelayReplayComplete)
		})
	}
}

func TestCheckpointReplayRecoveryFailureRemainsRetryable(t *testing.T) {
	p, _, baseB, baseR := setup(t)
	r := &checkpointReplayRelay{fakeRelay: baseR, replayResult: relay.ReplayResult{SendFailures: 1}}
	p.relay = r
	target := &replayCheckpointWorker{takeoverWorker: &takeoverWorker{fakeWorker: baseB}, keyframeErr: context.DeadlineExceeded}
	p.workers["b"].worker = target
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
	source := p.workers["a"]
	source.dead = true
	p.takeover(ctx, source, lease, time.Now())
	require.Empty(t, p.recentTakeovers(), "a failed recovery request cannot complete the takeover")
	require.Len(t, source.pending, 1)
	target.keyframeErr = nil
	lease, err = p.store.Get(ctx, id)
	require.NoError(t, err)
	p.takeover(ctx, source, lease, time.Now())
	require.Equal(t, 2, target.keyframes)
	require.Len(t, p.recentTakeovers(), 1)
	require.False(t, p.recentTakeovers()[0].Lost)
	require.True(t, p.recentTakeovers()[0].Result.RelayReplayRecoveryPLI)
	require.Equal(t, 1, p.recentTakeovers()[0].Result.RelayReplaySendFailures)
}

func TestCheckpointIncompleteReplayPlanUsesResumeRecoveryOnly(t *testing.T) {
	p, _, baseB, baseR := setup(t)
	p.relay = &checkpointReplayRelay{fakeRelay: baseR, incompletePlan: true, replayResult: relay.ReplayResult{Dropped: 1}}
	target := &replayCheckpointWorker{takeoverWorker: &takeoverWorker{fakeWorker: baseB}}
	p.workers["b"].worker = target
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
	p.workers["a"].dead = true
	p.takeover(ctx, p.workers["a"], lease, time.Now())
	require.False(t, target.opts.RelayReplay, "resume already uses its frame-cache/PLI recovery")
	require.Zero(t, target.keyframes, "an incomplete plan must not request a second recovery PLI")
	require.Len(t, p.recentTakeovers(), 1)
	require.False(t, p.recentTakeovers()[0].Result.RelayReplayComplete)
	require.False(t, p.recentTakeovers()[0].Result.RelayReplayRecoveryPLI)
}
