package mediaworker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

type bridgeHookAgent struct {
	*acceptedEventAgent
	accepted          int
	acknowledgements  []agenthost.BridgeCheckpoint
	session           *session
	publishedRevision uint64
	durableRevision   uint64
}

func (a *bridgeHookAgent) Accepted() {
	// A hook is outside the packet lock and sees publication already complete.
	a.session.mu.Lock()
	a.publishedRevision = a.session.state.Agent.Revision
	a.session.mu.Unlock()
	a.accepted++
}
func (a *bridgeHookAgent) Checkpointed(c agenthost.BridgeCheckpoint) {
	a.session.checkpoint.snapshotMu.Lock()
	defer a.session.checkpoint.snapshotMu.Unlock()
	a.session.mu.Lock()
	a.durableRevision = a.session.checkpoint.durableAgent.Revision
	a.session.mu.Unlock()
	a.acknowledgements = append(a.acknowledgements, c)
}
func (*bridgeHookAgent) Closed() {}
func TestBridgeHooksPublishOnlyAcceptedEventsAndCapturedAcknowledgements(t *testing.T) {
	rig, _ := newBridgeRig(t, false)
	s := rig.s
	app := &bridgeHookAgent{acceptedEventAgent: rig.app, session: s}
	s.agent.v2.instance = app
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
	require.NoError(t, err)
	app.accepted = 0
	// Refused Control is rolled back. No transport event may escape.
	app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
		return agent.Effects{}, errors.New("refuse")
	}
	_, err = (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: "rejected", Sequence: 1, Name: "test"}}, eventLive)
	require.ErrorIs(t, err, agent.ErrControlRejected)
	require.Empty(t, app.accepted)
	app.handle = nil
	input := &legacy.Input{Payload: []byte{0xf8, 1}, Progress: legacy.Progress{Index: 7}}
	_, err = (acceptedEvents{s}).submit(agent.Event{Kind: agent.AudioInput, Input: input}, eventLive)
	require.NoError(t, err)
	require.Equal(t, 1, app.accepted)
	require.Equal(t, s.state.Agent.Revision, app.publishedRevision)

	captured := cloneAgentState(s.state.Agent)
	captured.State.Bytes = []byte("old acknowledged pair")
	s.state.Agent.State.Bytes = []byte("new unacknowledged pair")
	s.checkpoint.snapshotMu.Lock()
	notify := s.checkpoint.ackSnapshot(captured, dtlsSequenceCheckpoint{}, time.Now(), 1, false)
	require.Empty(t, app.acknowledgements, "publication waits for the serialization lock release")
	s.checkpoint.snapshotMu.Unlock()
	notify()
	require.Len(t, app.acknowledgements, 1)
	require.Equal(t, captured.Revision, app.durableRevision)
	require.Equal(t, "old acknowledged pair", string(app.acknowledgements[0].State.Bytes))
	require.NotEmpty(t, app.acknowledgements[0].Output)
	// The opt-in hook cannot run for other-v2 agents, even if they implement it.
	s.agent.v2.bridge = nil
	_, err = (acceptedEvents{s}).submit(agent.Event{Kind: agent.AudioInput, Input: input}, eventLive)
	require.NoError(t, err)
	require.Equal(t, 1, app.accepted)
}
