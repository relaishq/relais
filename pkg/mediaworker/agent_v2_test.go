package mediaworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/privateapi"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

type v2UnitAgent struct {
	state        agent.State
	handles      int
	saves        int
	restores     int
	moveSaves    int
	moveRestores int
	lastEvent    agent.Event
	effects      agent.Effects
	moveCallback func(context.Context) error
	callback     func(context.Context)
	payload      []byte
}

func (a *v2UnitAgent) Handle(ctx context.Context, event agent.Event) (agent.Effects, error) {
	a.handles++
	a.lastEvent = event
	a.state.Bytes = []byte("rejected mutation")
	if a.callback != nil {
		a.callback(ctx)
	}
	return a.effects, nil
}
func (a *v2UnitAgent) Save(context.Context) (agent.State, error) { a.saves++; return a.state, nil }
func (a *v2UnitAgent) Restore(_ context.Context, state agent.State) error {
	a.restores++
	a.state = state
	return nil
}
func (a *v2UnitAgent) SaveMoveOnly(ctx context.Context) ([]byte, error) {
	a.moveSaves++
	if a.moveCallback != nil {
		if err := a.moveCallback(ctx); err != nil {
			return nil, err
		}
	}
	return a.payload, nil
}
func (a *v2UnitAgent) RestoreMoveOnly(_ context.Context, data []byte) error {
	a.moveRestores++
	a.payload = data
	return nil
}
func v2UnitSession(t *testing.T, a agent.Agent) (*Worker, *session) {
	t.Helper()
	w, err := New(Config{Agent: AgentConfig{Factory: agenthost.Factory(agenthost.Registration{Name: "unit/v1", Factory: func(agent.FactoryArgs) agent.Agent { return a }})}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	s := sessionFromState(w, sessionState{Version: 7, ID: "v2-unit"})
	t.Cleanup(s.close)
	return w, s
}
func TestV2UnknownAndControlIgnored(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1, Bytes: []byte("accepted")}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	before := cloneAgentState(s.state.Agent)
	for _, kind := range []agent.EventKind{"future_kind", agent.Control} {
		require.NoError(t, s.handleV2Event(agent.Event{Kind: kind}))
	}
	require.Zero(t, a.handles)
	require.Equal(t, 1, a.saves, "unknown and reserved kinds must not even Save")
	require.Equal(t, before, s.state.Agent)
}
func TestV2UnsupportedEffectsRollback(t *testing.T) {
	turn := agent.Turn(1)
	cases := []struct {
		name    string
		kind    agent.EventKind
		effects agent.Effects
		message string
	}{
		{"resumed-start", agent.Resumed, agent.Effects{StartTasks: []agent.TaskSpec{{Kind: "stt/v1"}}}, "StartTasks from Resumed is forbidden"},
		{"start", agent.Started, agent.Effects{StartTasks: []agent.TaskSpec{{}}}, "StartTasks not supported in this slice"},
		{"cancel", agent.Started, agent.Effects{CancelTasks: []agent.TaskID{"task"}}, "task cancellation not supported in this slice"},
		{"cancel-before", agent.Started, agent.Effects{CancelBefore: &turn}, "task cancellation not supported in this slice"},
		{"timer", agent.Started, agent.Effects{SetTimers: []agent.TimerSpec{{ID: "timer"}}}, "timers not supported in this slice"},
		{"cancel-timer", agent.Started, agent.Effects{CancelTimers: []string{"timer"}}, "timers not supported in this slice"},
		{"audio", agent.Started, agent.Effects{Audio: []agent.Audio{{}}}, "pacer effects not supported in this slice"},
		{"clear", agent.Started, agent.Effects{ClearAudioBefore: &turn}, "pacer effects not supported in this slice"},
		{"notify", agent.Started, agent.Effects{NotifyPlaybackFinished: &turn}, "pacer effects not supported in this slice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &v2UnitAgent{state: agent.State{Version: 1, Bytes: []byte("accepted")}, effects: tc.effects}
			_, s := v2UnitSession(t, a)
			require.NoError(t, s.initAgent(false, ResumeOptions{}))
			before := cloneAgentState(s.state.Agent)
			err := s.handleV2Event(agent.Event{Kind: tc.kind})
			require.ErrorContains(t, err, tc.message)
			require.Equal(t, before, s.state.Agent, "rejected effect must not advance accepted state")
			require.Equal(t, before.State.Bytes, a.state.Bytes, "rejected state is restored")
			require.Equal(t, 1, a.restores)
			require.Equal(t, 1, a.saves, "rejected effects never Save")
		})
	}
}
func TestV2LateCallbackQuarantinedBeforeRestore(t *testing.T) {
	var returned atomic.Bool
	a := &v2UnitAgent{state: agent.State{Version: 1, Bytes: []byte("accepted")}, callback: func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(10 * time.Millisecond)
		returned.Store(true)
	}}
	_, s := v2UnitSession(t, a)
	s.worker.cfg.Agent.CallbackTimeout = 10 * time.Millisecond
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	err := s.handleV2Event(agent.Event{Kind: agent.Started})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, returned.Load(), "Restore must wait for late Handle")
	require.Equal(t, []byte("accepted"), a.state.Bytes)
	require.False(t, s.state.Agent.V2.Started)
}

// Deliberately lacks the optional move-only interface.
type v2SmallOnly struct{ state agent.State }

func (a *v2SmallOnly) Handle(context.Context, agent.Event) (agent.Effects, error) {
	return agent.Effects{}, nil
}
func (a *v2SmallOnly) Save(context.Context) (agent.State, error)  { return a.state, nil }
func (a *v2SmallOnly) Restore(context.Context, agent.State) error { return nil }
func TestV2StateAdmission(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   agent.State
		message string
	}{
		{"version", agent.State{}, "unknown state version"},
		{"small-state", agent.State{Version: 1, Bytes: make([]byte, 1025)}, "state exceeds size limit"},
		{"negative", agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: -1}, "invalid move-only metadata"},
		{"inconsistent", agent.State{Version: 1, MoveOnlyLength: 1}, "invalid move-only metadata"},
		{"move-limit", agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: 1<<20 + 1}, "session budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &v2UnitAgent{state: tc.state}
			w, s := v2UnitSession(t, a)
			require.ErrorContains(t, s.initAgent(false, ResumeOptions{}), tc.message)
			s.close()
			require.Zero(t, w.moveBytes.Load())
		})
	}
	a := &v2SmallOnly{state: agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: 1}}
	_, s := v2UnitSession(t, a)
	require.ErrorContains(t, s.initAgent(false, ResumeOptions{}), "HasMoveOnly requires MoveOnlyState")
}
func TestV2MoveBudgetAndExportLength(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: 1}, payload: []byte("bad length")}
	w, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	require.EqualValues(t, 1, w.moveBytes.Load())
	require.True(t, w.reserveMoveBytes(v2MoveWorkerBytes-1))
	require.ErrorContains(t, s.validateV2State(agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: 2}), ErrMoveBudget.Error())
	w.moveBytes.Add(-(v2MoveWorkerBytes - 1))
	_, err := s.saveV2MoveOnly(context.Background())
	require.ErrorContains(t, err, "differs from accepted Save")
	require.Equal(t, 1, a.moveSaves)
	s.close()
	require.Zero(t, w.moveBytes.Load())
	require.ErrorIs(t, s.reserveMoveBytes(1), context.Canceled)
}
func TestV2IncompatibleHTTPErrorIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerAPIError(w, errors.Join(legacy.ErrRestore, agent.ErrIncompatibleAgent))
	}))
	defer server.Close()
	err := privateapi.Do(context.Background(), server.Client(), server.URL, "POST", "/resume", ResumeRequest{}, nil, RemoteErrors)
	require.ErrorIs(t, err, agent.ErrIncompatibleAgent)
}

// The current legacy writer's full v7 snapshot must round-trip byte for byte,
// including agent state, then restore through the actual transport boundary.
func TestV7SnapshotWireRoundTripUnchanged(t *testing.T) {
	// Captured by 787127e004cd95decfd64c8d63da679ac5822e16's live v7 writer
	// in a scratch checkout. The cert/key are generated for this test fixture.
	golden, err := os.ReadFile("testdata/v7-787127e.json")
	require.NoError(t, err)
	saved, err := decodeSnapshot(golden)
	require.NoError(t, err)
	reencoded, err := json.Marshal(saved)
	require.NoError(t, err)
	require.Equal(t, string(golden), string(reencoded), "main's golden v7 snapshot must cross-encode identically")
	fixtureWorker := newTestWorker(t)
	_, err = fixtureWorker.ResumeSession(golden, ResumeOptions{})
	require.NoError(t, err)

	w := newTestWorker(t)
	call, client := dialDTLSCaller(t, w)
	defer func() { _ = client.Close() }()
	data, err := w.ExportSession(call.id)
	require.NoError(t, err)
	require.False(t, bytes.Contains(data, []byte("\"V2\"")))
	require.False(t, bytes.Contains(data, []byte("AgentMoveOnly")))
	snap, err := decodeSnapshot(data)
	require.NoError(t, err)
	require.Equal(t, 7, snap.Version)
	require.Equal(t, 7, snap.State.Version)
	encoded, err := json.Marshal(snap)
	require.NoError(t, err)
	require.Equal(t, string(data), string(encoded), "legacy wire form remains unchanged")
	_, err = w.ResumeSession(data, ResumeOptions{})
	require.NoError(t, err)
	require.True(t, call.endpoint(t).check(t, false))
}

func TestV2RestoreMoveOnlyOnlyForExactResume(t *testing.T) {
	for _, takeover := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact", true: "crash"}[takeover], func(t *testing.T) {
			source := &v2UnitAgent{state: agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: 4}, payload: []byte("move")}
			_, s := v2UnitSession(t, source)
			require.NoError(t, s.initAgent(false, ResumeOptions{}))
			target := &v2UnitAgent{}
			_, next := v2UnitSession(t, target)
			next.state = s.state
			next.state.Agent = cloneAgentState(s.state.Agent)
			opts := ResumeOptions{Kind: legacy.PlannedMove}
			if takeover {
				opts.Kind = legacy.Takeover
				opts.SequenceMargin = 8192
			}
			require.NoError(t, next.initAgent(true, opts, &movePayload{Bytes: []byte("move")}))
			if takeover {
				require.Zero(t, target.moveRestores)
				require.Equal(t, agent.OutcomeUnknown, target.lastEvent.Resume.MoveOnly)
			} else {
				require.Equal(t, 1, target.moveRestores)
				require.Equal(t, []byte("move"), target.payload)
				require.Equal(t, agent.OK, target.lastEvent.Resume.MoveOnly)
			}
		})
	}
}

func TestV2RestoreMismatchRefusesBeforeFactory(t *testing.T) {
	source := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, source)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	w, err := New(Config{Agent: AgentConfig{Factory: agenthost.Factory(agenthost.Registration{Name: "different/v1", Factory: func(agent.FactoryArgs) agent.Agent { t.Error("incompatible factory must not run"); return source }})}})
	require.NoError(t, err)
	defer func() { require.NoError(t, w.Close()) }()
	next := sessionFromState(w, s.state)
	defer next.close()
	require.ErrorIs(t, next.initAgent(true, ResumeOptions{}), agent.ErrIncompatibleAgent)
	require.Zero(t, source.handles)
	require.Zero(t, source.restores)
}

func TestV2StateBoundaryAndFactoryArgs(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1, Bytes: make([]byte, 1024), HasMoveOnly: true, MoveOnlyLength: v2MoveSessionBytes}}
	var args agent.FactoryArgs
	w, err := New(Config{Agent: AgentConfig{Factory: agenthost.Factory(agenthost.Registration{Name: "budget/v1", Factory: func(got agent.FactoryArgs) agent.Agent { args = got; return a }})}})
	require.NoError(t, err)
	defer func() { require.NoError(t, w.Close()) }()
	s := sessionFromState(w, sessionState{Version: 7, ID: "factory-budget"})
	defer s.close()
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	require.Equal(t, agent.FactoryArgs{SessionID: "factory-budget", AgentName: "budget/v1", MaxStateBytes: 1024}, args)
	require.EqualValues(t, v2MoveSessionBytes, w.moveBytes.Load())
	require.Zero(t, a.moveSaves, "state admission must only inspect metadata")
	require.ErrorIs(t, s.validateV2State(agent.State{Version: 1, Bytes: make([]byte, 1025)}), agent.ErrStateTooLarge)
	require.ErrorContains(t, s.validateV2State(agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: v2MoveSessionBytes + 1}), "session budget")
}

type heldExportWriter struct {
	header  http.Header
	wrote   chan struct{}
	release chan struct{}
}

func (w *heldExportWriter) Header() http.Header { return w.header }
func (*heldExportWriter) WriteHeader(int)       {}
func (w *heldExportWriter) Write(data []byte) (int, error) {
	close(w.wrote)
	<-w.release
	return len(data), nil
}

func TestV2HTTPHeldExportCountsAgainstWorkerBudget(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: 1}, payload: []byte("m")}
	w, err := New(Config{Agent: AgentConfig{Factory: agenthost.Factory(agenthost.Registration{Name: "http/v1", Factory: func(agent.FactoryArgs) agent.Agent { return a }})}})
	require.NoError(t, err)
	defer func() { require.NoError(t, w.Close()) }()
	call, client := dialDTLSCaller(t, w)
	defer func() { _ = client.Close() }()
	writer := &heldExportWriter{header: make(http.Header), wrote: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.PrivateHandler().ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "/sessions/"+call.id+"/export", nil))
	}()
	defer func() {
		close(writer.release)
		<-done
		require.Zero(t, w.moveBytes.Load(), "HTTP completion releases export scratch")
	}()
	select {
	case <-writer.wrote:
	case <-time.After(time.Second):
		t.Fatal("export did not reach HTTP write")
	}
	require.Zero(t, w.SessionCount())
	require.EqualValues(t, 8, w.moveBytes.Load(), "export serialization remains reserved until HTTP write returns")
	require.Equal(t, 1, a.moveSaves)
}
