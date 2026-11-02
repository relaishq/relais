package agentbridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func newAdapter(t *testing.T) *adapter {
	t.Helper()
	b, err := New(Config{URL: "ws://127.0.0.1:9102/bridge", Token: "isolated-test-token"})
	require.NoError(t, err)
	return b.Registration().Factory(agent.FactoryArgs{SessionID: "session", MaxStateBytes: 1024, MaxHistoryBytes: 8192}).(*adapter)
}
func TestLoopbackConfigRefusesRemoteAndCredentialsInURL(t *testing.T) {
	for _, url := range []string{"ws://0.0.0.0:1/bridge", "ws://192.168.1.1:1/bridge", "ws://localhost:1/bridge", "wss://127.0.0.1:1/bridge", "ws://user:pass@127.0.0.1:1/bridge", "ws://127.0.0.1:1/bridge?token=x", "ws://127.0.0.1:1/bridge#x"} {
		require.Error(t, (Config{URL: url, Token: "isolated-test-token"}).Validate())
	}
	require.NoError(t, (Config{URL: "ws://[::1]:9102/bridge", Token: "isolated-test-token"}).Validate())
}
func TestBlockedTransportKeepsSaveRestoreLocalAndExportLengthStable(t *testing.T) {
	a := newAdapter(t)
	ctx := context.Background()
	// No socket consumer. Each accepted transaction publishes an immutable image.
	for i := uint64(0); i < 1000; i++ {
		_, err := a.Handle(ctx, agent.Event{Kind: agent.AudioInput, Input: &legacy.Input{Payload: make([]byte, 1200), Progress: legacy.Progress{Index: i}}})
		require.NoError(t, err)
		a.Accepted()
	}
	require.LessOrEqual(t, len(a.accepted.Records), 32)
	require.LessOrEqual(t, len(a.accepted.encoded), outboxBytes)
	require.Positive(t, a.accepted.GapID)
	require.EqualValues(t, 999, a.accepted.Last)
	candidate := checkpoint{Sequence: 1, Version: 1, Working: []byte("local"), History: []byte("history")}
	raw, err := json.Marshal(candidate)
	require.NoError(t, err)
	require.NoError(t, a.candidate(raw))
	update, err := json.Marshal(adapterUpdate{Checkpoint: &candidate})
	require.NoError(t, err)
	_, err = a.Handle(ctx, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Chunk: agent.Chunk{Data: update}}})
	require.NoError(t, err)
	a.Accepted()
	before, err := a.Save(ctx)
	require.NoError(t, err)
	frozen, err := a.SaveMoveOnly(ctx)
	require.NoError(t, err)
	require.True(t, before.HasMoveOnly)
	require.Equal(t, len(frozen), before.MoveOnlyLength)
	again, err := a.SaveMoveOnly(ctx)
	require.NoError(t, err)
	require.Equal(t, frozen, again)
	// An unaccepted delivery retirement is rolled back without losing queued input.
	update, err = json.Marshal(adapterUpdate{Delivered: a.current.Revision})
	require.NoError(t, err)
	_, err = a.Handle(ctx, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Chunk: agent.Chunk{Data: update}}})
	require.NoError(t, err)
	require.NoError(t, a.Restore(ctx, before))
	after, err := a.Save(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	restored := newAdapter(t)
	require.NoError(t, restored.Restore(ctx, before))
	require.Error(t, restored.RestoreMoveOnly(ctx, frozen[:len(frozen)-1]))
	require.NoError(t, restored.RestoreMoveOnly(ctx, frozen))
	copied, err := restored.SaveMoveOnly(ctx)
	require.NoError(t, err)
	require.Equal(t, frozen, copied)
	_, err = a.Handle(ctx, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Chunk: agent.Chunk{Data: update}}})
	require.NoError(t, err)
	a.Accepted()
	require.Empty(t, a.accepted.Records)
	require.Equal(t, frozen, copied, "retiring the live queue cannot mutate the frozen export")
	before.Bytes[0] = 'x'
	require.NotEqual(t, before.Bytes, after.Bytes, "Save owns its bytes")
	require.ErrorIs(t, a.Restore(ctx, agent.State{Version: 2}), agent.ErrVersion)
	a.Closed()
	_, ok := a.bridge.agents.Load(a.id)
	require.False(t, ok)
}
func TestCheckpointBoundsAndExactAcknowledgement(t *testing.T) {
	a := newAdapter(t)
	for _, c := range []checkpoint{{Sequence: 1, Version: 2}, {Sequence: 0, Version: 1}, {Sequence: 1, Version: 1, Working: []byte(strings.Repeat("x", 1024))}, {Sequence: 1, Version: 1, History: make([]byte, 8193)}} {
		raw, err := json.Marshal(c)
		require.NoError(t, err)
		require.Error(t, a.candidate(raw))
	}
	a.Checkpointed(agenthost.BridgeCheckpoint{Sequence: 1, State: agent.State{Version: 1, Bytes: []byte("captured")}})
	ack := <-a.acks
	require.Equal(t, "captured", string(ack.State.Bytes))
	require.EqualValues(t, 1, ack.Sequence)
	a.Checkpointed(agenthost.BridgeCheckpoint{Sequence: 2, State: agent.State{Version: 1, Bytes: []byte("new captured")}})
	a.Checkpointed(agenthost.BridgeCheckpoint{Sequence: 1, State: agent.State{Version: 1, Bytes: []byte("older captured")}})
	ack = <-a.acks
	require.EqualValues(t, 2, ack.Sequence)
	require.Equal(t, "new captured", string(ack.State.Bytes))
}

func TestRestoreMoveOnlyRejectsInvalidImages(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*outboxImage)
	}{
		{"session", func(i *outboxImage) { i.SessionID = "other" }},
		{"revision", func(i *outboxImage) { i.Revision++ }},
		{"order", func(i *outboxImage) { i.Records[0].ID = 2; i.Records[1].ID = 1 }},
		{"record-id", func(i *outboxImage) { i.Records[1].ID = i.Revision + 1 }},
		{"gap-id", func(i *outboxImage) { i.GapID = i.Revision + 1 }},
		{"gap-range", func(i *outboxImage) { i.GapID = i.Revision; i.First = 9; i.Last = 8 }},
		{"event-kind", func(i *outboxImage) { i.Records[0].Event.Kind = "unknown" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdapter(t)
			image := outboxImage{SessionID: a.id, Revision: 3, Records: []outboxRecord{{ID: 1, Event: agent.Event{Kind: agent.PlaybackFinished, Playback: &agent.PlaybackNotice{Turn: 1}}}, {ID: 2, Event: agent.Event{Kind: agent.PlaybackFinished, Playback: &agent.PlaybackNotice{Turn: 2}}}}}
			tc.change(&image)
			raw, err := json.Marshal(image)
			require.NoError(t, err)
			a.current = &outboxImage{Revision: 3}
			a.restoreLength = len(raw)
			before := a.accepted
			require.Error(t, a.RestoreMoveOnly(context.Background(), raw))
			require.Same(t, before, a.accepted, "invalid input cannot publish a partial restore")
		})
	}
	t.Run("corrupt-json-correct-length", func(t *testing.T) {
		a := newAdapter(t)
		raw, err := a.SaveMoveOnly(context.Background())
		require.NoError(t, err)
		a.restoreLength = len(raw)
		raw[0] = '!'
		require.Error(t, a.RestoreMoveOnly(context.Background(), raw))
	})
}
