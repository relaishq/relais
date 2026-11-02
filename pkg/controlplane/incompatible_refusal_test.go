package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/agent"
	agentv2 "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type refusalProbeWorker struct {
	Worker
	attempts int
	state    []byte
	err      error
}

func TestTakeoverResumeFailureBudgetClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		uncertain bool
		refund    bool
	}{
		{"incompatible", agentv2.ErrIncompatibleAgent, false, true},
		{"restore-incompatible", errors.Join(agent.ErrRestore, agentv2.ErrIncompatibleAgent), false, true},
		{"restore", agent.ErrRestore, false, false},
		{"save", agent.ErrSave, false, false},
		{"version", agent.ErrVersion, false, false},
		{"state-too-large", agent.ErrStateTooLarge, false, false},
		{"unknown-reply", privateapi.ErrUncertain, true, false},
		{"timeout", context.DeadlineExceeded, true, false},
		{"uncertain-incompatible", privateapi.Uncertain(agentv2.ErrIncompatibleAgent), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			p, _, baseB, _ := setup(t)
			b := &rejectingAgentWorker{fakeWorker: baseB, err: tc.err}
			p.workers["b"].worker = b
			c := &rejectingAgentWorker{fakeWorker: &fakeWorker{store: p.store, addr: netip.MustParseAddrPort("127.0.0.1:3"), running: map[string]bool{}}, err: agent.ErrRestore}
			require.NoError(t, p.Register("c", c.addr, c))
			z := &takeoverWorker{fakeWorker: &fakeWorker{store: p.store, addr: netip.MustParseAddrPort("127.0.0.1:5"), running: map[string]bool{}}}
			compatible := &refusalProbeWorker{Worker: z}
			require.NoError(t, p.Register("z", z.addr, compatible))
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
			expireObservedWorker(t, p.workers["a"])
			result := p.recovery.recover(ctx, p.workers["a"], lease, time.Now())
			if tc.uncertain {
				require.Nil(t, result, "uncertainty must retain adoption for confirmation")
				for range 3 {
					next := pendingRecovery(p, p.workers["a"], id)
					require.NotNil(t, next)
					require.Nil(t, p.recovery.recover(ctx, p.workers["a"], *next, time.Now()))
				}
				require.Equal(t, 4, b.attempts, "confirmation must stay on the uncertain target")
				p.workers["b"].observation.markDraining()
				next := pendingRecovery(p, p.workers["a"], id)
				require.NotNil(t, next)
				result = p.recovery.recover(ctx, p.workers["a"], *next, time.Now())
			} else {
				require.Equal(t, 1, b.attempts)
			}
			require.NotNil(t, result)
			require.Equal(t, !tc.refund, result.Lost)
			require.Equal(t, 1, c.attempts)
			if tc.refund {
				require.Equal(t, 1, compatible.attempts, "incompatibility must leave an attempt after the restore failure")
				require.True(t, z.runs(id))
				require.NoError(t, p.End(ctx, id))
			} else {
				require.Contains(t, result.Error, "resume attempts exhausted")
				require.Zero(t, compatible.attempts, "two charged failures must exhaust this snapshot")
			}
		})
	}
}

func TestTakeoverIncompatibleRefusalsEndAtTargetDeadline(t *testing.T) {
	s := newStalledPool(t)
	s.targetReturns(t)
	b := &rejectingAgentWorker{fakeWorker: s.b.fakeWorker, err: agentv2.ErrIncompatibleAgent}
	s.p.workers["b"].worker = b
	c := &rejectingAgentWorker{fakeWorker: &fakeWorker{store: s.p.store, addr: netip.MustParseAddrPort("127.0.0.1:3"), running: map[string]bool{}}, err: agentv2.ErrIncompatibleAgent}
	require.NoError(t, s.p.Register("c", c.addr, c))
	waits := 0
	s.p.recovery.targetWait = func(context.Context, <-chan struct{}, time.Duration) error {
		waits++
		require.Equal(t, 1, waits)
		require.Equal(t, 1, b.attempts)
		require.Equal(t, 1, c.attempts)
		s.now = s.p.recovery.pending[s.p.workers["a"]][s.id].targetDeadline
		return nil
	}
	result := s.p.recovery.recover(context.Background(), s.p.workers["a"], s.lease, s.now)
	require.NotNil(t, result)
	require.True(t, result.Lost)
	require.Contains(t, result.Error, ErrTargetWaitExpired.Error())
	require.Contains(t, result.Error, agentv2.ErrIncompatibleAgent.Error())
	require.Equal(t, 1, waits)
	require.Empty(t, recoveryWork(s.p, s.p.workers["a"]))
}

func TestTakeoverConfirmationIncompatibilityKeepsUncertainAttemptCharged(t *testing.T) {
	ctx := context.Background()
	p, _, baseB, _ := setup(t)
	b := &rejectingAgentWorker{fakeWorker: baseB, err: privateapi.Uncertain(agentv2.ErrIncompatibleAgent)}
	p.workers["b"].worker = b
	c := &rejectingAgentWorker{fakeWorker: &fakeWorker{store: p.store, addr: netip.MustParseAddrPort("127.0.0.1:3"), running: map[string]bool{}}, err: agent.ErrRestore}
	require.NoError(t, p.Register("c", c.addr, c))
	z := &takeoverWorker{fakeWorker: &fakeWorker{store: p.store, addr: netip.MustParseAddrPort("127.0.0.1:5"), running: map[string]bool{}}}
	compatible := &refusalProbeWorker{Worker: z}
	require.NoError(t, p.Register("z", z.addr, compatible))
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
	expireObservedWorker(t, p.workers["a"])
	require.Nil(t, p.recovery.recover(ctx, p.workers["a"], lease, time.Now()))
	require.Equal(t, 1, b.attempts)
	require.Zero(t, c.attempts)
	b.err = errors.Join(agent.ErrRestore, agentv2.ErrIncompatibleAgent)
	next := pendingRecovery(p, p.workers["a"], id)
	require.NotNil(t, next)
	result := p.recovery.recover(ctx, p.workers["a"], *next, time.Now())
	require.NotNil(t, result)
	require.True(t, result.Lost, "a confirmation refusal cannot refund the uncertain first attempt")
	require.Contains(t, result.Error, "resume attempts exhausted")
	require.Equal(t, 2, b.attempts)
	require.Equal(t, 1, c.attempts)
	require.Zero(t, compatible.attempts)
}

func TestPlannedPendingIncompatibilityKeepsResumeAttemptCharged(t *testing.T) {
	ctx := context.Background()
	p, _, baseB, _ := setup(t)
	b := &rejectingAgentWorker{fakeWorker: baseB, err: agentv2.ErrIncompatibleAgent}
	p.workers["b"].worker = b
	c := &rejectingAgentWorker{fakeWorker: &fakeWorker{store: p.store, addr: netip.MustParseAddrPort("127.0.0.1:3"), running: map[string]bool{}}, err: agent.ErrRestore}
	require.NoError(t, p.Register("c", c.addr, c))
	z := &takeoverWorker{fakeWorker: &fakeWorker{store: p.store, addr: netip.MustParseAddrPort("127.0.0.1:5"), running: map[string]bool{}}}
	compatible := &refusalProbeWorker{Worker: z}
	require.NoError(t, p.Register("z", z.addr, compatible))
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
	queueUncertainExport(t, p, id)
	next := pendingRecovery(p, p.workers["a"], id)
	require.NotNil(t, next)
	result := p.recovery.recover(ctx, p.workers["a"], *next, time.Now())
	require.NotNil(t, result)
	require.True(t, result.Lost, "planned pending recovery must retain its existing attempt limit")
	require.Contains(t, result.Error, "resume attempts exhausted")
	require.Equal(t, 1, b.attempts)
	require.Equal(t, 1, c.attempts)
	require.Zero(t, compatible.attempts)
}

func (w *refusalProbeWorker) ResumeSession(state []byte, opts mediaworker.ResumeOptions) (string, error) {
	w.attempts++
	w.state = append([]byte(nil), state...)
	id, err := w.Worker.ResumeSession(state, opts)
	w.err = err
	return id, err
}

func TestTakeoverIncompatibleRefusalsPreserveResumeBudget(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "remote"}[remote], func(t *testing.T) {
			ctx := context.Background()
			store := sessionstore.NewMemory()
			p := New(&fakeRelay{}, store)
			a := &fakeWorker{store: store, addr: netip.MustParseAddrPort("127.0.0.1:1"), running: map[string]bool{}}
			require.NoError(t, p.Register("a", a.addr, a))
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			lease, err := store.Get(ctx, id)
			require.NoError(t, err)
			var snapshot map[string]any
			require.NoError(t, json.Unmarshal(takeoverSnapshot(t, id, 0), &snapshot))
			snapshot["Version"] = 8
			state := snapshot["State"].(map[string]any)
			state["Version"] = 8
			state["Agent"] = map[string]any{"V2": map[string]any{"Name": "unavailable/v1", "Contract": "agent/v2"}}
			data, err := json.Marshal(snapshot)
			require.NoError(t, err)
			require.NoError(t, store.PutState(ctx, lease, data))
			budget, err := mediaworker.SequenceResumeAttempts(data, 8192)
			require.NoError(t, err)
			require.Equal(t, 2, budget, "three refusals must exceed the snapshot's resume budget")

			var refusing []*refusalProbeWorker
			for _, name := range []string{"b", "c", "d"} {
				w, err := mediaworker.New(mediaworker.Config{})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, w.Close()) })
				var worker Worker = w
				if remote {
					server := httptest.NewServer(w.PrivateHandler())
					t.Cleanup(server.Close)
					worker = &RemoteWorker{URL: server.URL, Client: server.Client()}
				}
				probe := &refusalProbeWorker{Worker: worker}
				refusing = append(refusing, probe)
				require.NoError(t, p.Register(name, w.LocalAddr(), probe))
			}
			z := &takeoverWorker{fakeWorker: &fakeWorker{store: store, addr: netip.MustParseAddrPort("127.0.0.1:5"), running: map[string]bool{}}}
			compatible := &refusalProbeWorker{Worker: z}
			require.NoError(t, p.Register("z", z.addr, compatible))
			expireObservedWorker(t, p.workers["a"])
			detected := time.Now()
			result := p.recovery.recover(ctx, p.workers["a"], lease, detected)
			require.NotNil(t, result)
			require.False(t, result.Lost, "compatible target attempts=%d; result=%+v", compatible.attempts, result)
			require.True(t, result.End.Before(detected.Add(takeoverBudget)))
			for _, probe := range refusing {
				require.Equal(t, 1, probe.attempts, "a refusing target must be excluded")
				require.ErrorIs(t, probe.err, agentv2.ErrIncompatibleAgent)
				require.Equal(t, data, probe.state)
			}
			require.Equal(t, 1, compatible.attempts)
			require.Equal(t, data, compatible.state, "refusals must not publish adjusted counters")
			require.True(t, z.runs(id))
			status, err := p.Status(ctx)
			require.NoError(t, err)
			require.Zero(t, status.LostCount)
			require.Len(t, status.Calls, 1)
			require.Equal(t, "z", status.Calls[0].Owner)
			require.NoError(t, p.End(ctx, id))
		})
	}
}
