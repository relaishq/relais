package mediaworker

import (
	"context"
	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/privateapi"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestV2ReviewExportBudgetRefusalRetainsSource(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: 1}, payload: []byte("m")}
	w, err := New(Config{Agent: AgentConfig{Factory: agenthost.Factory(agenthost.Registration{Name: "budget/v1", Factory: func(agent.FactoryArgs) agent.Agent { return a }})}})
	require.NoError(t, err)
	defer func() { require.NoError(t, w.Close()) }()
	call, client := dialDTLSCaller(t, w)
	defer func() { _ = client.Close() }()
	// Fill the remaining worker budget, as concurrent held exports can do.
	pressure := int64(v2MoveWorkerBytes) - w.moveBytes.Load()
	require.True(t, w.reserveMoveBytes(pressure))
	releasePressure := sync.OnceFunc(func() { w.moveBytes.Add(-pressure) })
	defer releasePressure()
	data, err := w.ExportSession(call.id)
	require.Nil(t, data)
	require.ErrorIs(t, err, ErrMoveBudget)
	require.ErrorIs(t, err, ErrExportNotFlushed)
	require.NotErrorIs(t, err, legacy.ErrSave)
	require.NotNil(t, w.session(call.id))
	require.False(t, w.session(call.id).fenced.Load())
	require.False(t, w.session(call.id).agent.paused)
	require.Zero(t, a.moveSaves, "budget refusal occurs before SaveMoveOnly")
	releasePressure()
	_, err = w.ExportSession(call.id)
	require.NoError(t, err)
	require.Equal(t, 1, a.moveSaves)
}

func TestV2ReviewRestoreBudgetIsNotAgentFailure(t *testing.T) {
	source := &v2UnitAgent{state: agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: 1}, payload: []byte("m")}
	_, s := v2UnitSession(t, source)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	target := &v2UnitAgent{}
	w, next := v2UnitSession(t, target)
	next.state = s.state
	next.state.Agent = cloneAgentState(s.state.Agent)
	require.True(t, w.reserveMoveBytes(v2MoveWorkerBytes))
	defer w.moveBytes.Add(-v2MoveWorkerBytes)
	err := next.initAgent(true, ResumeOptions{Kind: legacy.PlannedMove}, &movePayload{Bytes: []byte("m")})
	require.ErrorIs(t, err, ErrMoveBudget)
	require.NotErrorIs(t, err, legacy.ErrRestore)
	require.NotErrorIs(t, err, agent.ErrIncompatibleAgent)
	require.Zero(t, target.restores)
	require.Zero(t, w.AgentStats().RestoreFailures)
}

func TestV2ReviewFlushReservesSaveMoveOnlyDeadline(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1, HasMoveOnly: true}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.worker.cfg.Agent.CallbackTimeout = time.Second
	s.agent.queue = make(chan agentInput, 8)
	for range 8 {
		s.agent.queue <- agentInput{}
	}
	// With 1.5 callback budgets left, starting any queued Handle would spend
	// the full budget that must remain available for SaveMoveOnly. No sleeps
	// are needed: the assertion directly observes which callbacks were started.
	request := agentFlush{done: make(chan error, 1), deadline: time.Now().Add(1500 * time.Millisecond), moveOnly: make(chan agentMoveExport, 1)}
	require.True(t, s.finishAgentFlush(request))
	require.NoError(t, <-request.done)
	<-request.moveOnly
	release, err := s.claimV2ExportScratch()
	require.NoError(t, err)
	defer release()
	require.Zero(t, a.handles, "drain must keep two callback budgets before starting another Handle")
	require.EqualValues(t, 8, s.worker.AgentStats().InputDrops)
	require.Equal(t, 1, a.moveSaves)
}

func TestV2ReviewEchoMarkerDoesNotBypassApplicationProgress(t *testing.T) {
	state := sessionState{Agent: agentState{V2: &v2Envelope{}, State: legacy.State{Version: 0x4543484f}, Progress: legacy.Progress{SSRC: 42, Index: 10, Consumed: 1}}, Audio: trackState{Anchored: true, InboundSSRC: 42}, SRTP: srtpState{Inbound: map[uint32]uint64{42: 20}}}
	indexes, err := replayInboundIndexes(state)
	require.NoError(t, err)
	require.EqualValues(t, 10, indexes[42], "v2 must use accepted application progress even when its version equals the Echo marker")
}

func TestV2ReviewNewSessionBudgetIsNotAgentFailure(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: 1}}
	w, s := v2UnitSession(t, a)
	require.True(t, w.reserveMoveBytes(v2MoveWorkerBytes))
	defer w.moveBytes.Add(-v2MoveWorkerBytes)
	err := s.initAgent(false, ResumeOptions{})
	require.ErrorIs(t, err, ErrMoveBudget)
	require.NotErrorIs(t, err, legacy.ErrSave)
	require.NotErrorIs(t, err, legacy.ErrRestore)
	require.Equal(t, AgentStats{}, w.AgentStats(), "worker pressure must not increment agent fault metrics")
}

func TestV2ReviewBudgetHTTPPreservesNoFlushAndResourceIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { workerAPIError(w, ErrExportNotFlushed) }))
	defer server.Close()
	err := privateapi.Do(context.Background(), server.Client(), server.URL, http.MethodPost, "/export", nil, nil, RemoteErrors)
	require.ErrorIs(t, err, ErrExportNotFlushed)
	require.ErrorIs(t, err, ErrMoveBudget)
	require.NotErrorIs(t, err, legacy.ErrRestore)
	require.NotErrorIs(t, err, agent.ErrIncompatibleAgent)
}
