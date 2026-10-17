package loopback

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"testing"

	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func input(index uint64, payload []byte) agent.Event {
	return agent.Event{Kind: agent.AudioInput, Input: &legacy.Input{Progress: legacy.Progress{SSRC: 7, Index: index}, Payload: payload}}
}
func TestSparseWindow(t *testing.T) {
	var w Window
	for _, n := range []uint64{65534, 65536, 65535, 65537, 65662, 65661} {
		require.True(t, w.Accept(7, n))
		require.False(t, w.Accept(7, n))
	}
	require.False(t, w.Accept(7, 65534))
	require.False(t, w.Accept(8, 65663))
	require.True(t, w.Accept(7, math.MaxUint64))
	require.False(t, w.Accept(7, math.MaxUint64))
	require.Equal(t, 1, w.Count())
}
func TestRecordingTimerMoveAndUnknown(t *testing.T) {
	ctx := context.Background()
	a := New(agent.FactoryArgs{}).(*Agent)
	packet := []byte{0xf8, 0xff, 0xfe}
	effects, err := a.Handle(ctx, input(10, packet))
	require.NoError(t, err)
	require.Len(t, effects.SetTimers, 1)
	require.Equal(t, RecordTime, effects.SetTimers[0].After)
	packet[1] = 0 // Input memory belongs to the callback; app must own its recording.
	_, err = a.Handle(ctx, input(12, []byte{0xf8, 0xff, 0xfe}))
	require.NoError(t, err)
	_, err = a.Handle(ctx, input(11, []byte{0xf8, 0xff, 0xfe}))
	require.NoError(t, err)
	_, err = a.Handle(ctx, input(11, []byte{0xf8, 0xff, 0xfe}))
	require.NoError(t, err)
	saved, err := a.Save(ctx)
	require.NoError(t, err)
	require.Less(t, len(saved.Bytes), 1024)
	require.True(t, saved.HasMoveOnly)
	moved, err := a.SaveMoveOnly(ctx)
	require.NoError(t, err)
	require.Equal(t, saved.MoveOnlyLength, len(moved))
	require.Equal(t, []byte{1, 0, 3, 0xf8, 0xff, 0xfe, 0, 3, 0xf8, 0xff, 0xfe, 0, 3, 0xf8, 0xff, 0xfe}, moved)
	require.NotContains(t, string(saved.Bytes), "packets_data")
	b := New(agent.FactoryArgs{}).(*Agent)
	require.NoError(t, b.Restore(ctx, saved))
	require.NoError(t, b.RestoreMoveOnly(ctx, moved))
	effects, err = b.Handle(ctx, agent.Event{Kind: agent.TimerFired, Timer: &agent.TimerNotice{ID: recordTimer}})
	require.NoError(t, err)
	require.Len(t, effects.StartTasks, 1)
	require.Equal(t, moved, effects.StartTasks[0].Request)
	require.Equal(t, agent.ReadOnly, effects.StartTasks[0].Class)
	after, err := b.Save(ctx)
	require.NoError(t, err)
	require.False(t, after.HasMoveOnly)
	require.Zero(t, after.MoveOnlyLength)
	// Crash with a large lost length: Save after Resumed must reserve zero on a
	// target with a smaller move budget, before any later input or task event.
	saved.MoveOnlyLength = MaxRecordingBytes
	c := New(agent.FactoryArgs{}).(*Agent)
	require.NoError(t, c.Restore(ctx, saved))
	effects, err = c.Handle(ctx, agent.Event{Kind: agent.Resumed, Resume: &agent.ResumeNotice{MoveOnly: agent.OutcomeUnknown}})
	require.NoError(t, err)
	require.Empty(t, effects.StartTasks)
	require.EqualValues(t, 2, *effects.ClearAudioBefore)
	require.EqualValues(t, 2, *effects.CancelBefore)
	after, err = c.Save(ctx)
	require.NoError(t, err)
	require.False(t, after.HasMoveOnly)
	require.Zero(t, after.MoveOnlyLength)
	require.Equal(t, agent.OutcomeUnknown, c.state.Outcome)
	require.NoError(t, c.Restore(ctx, after))
	require.ErrorIs(t, c.Restore(ctx, agent.State{Version: 99}), agent.ErrVersion)
}
func TestBoundAndUnknownEvent(t *testing.T) {
	ctx := context.Background()
	a := New(agent.FactoryArgs{}).(*Agent)
	before, err := a.Save(ctx)
	require.NoError(t, err)
	effects, err := a.Handle(ctx, agent.Event{Kind: "future"})
	require.NoError(t, err)
	require.Equal(t, agent.Effects{}, effects)
	after, err := a.Save(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	for i := range 60 {
		effects, err = a.Handle(ctx, input(uint64(i), bytes.Repeat([]byte{0xf8}, 1200)))
		require.NoError(t, err)
		if len(effects.StartTasks) > 0 {
			require.LessOrEqual(t, len(effects.StartTasks[0].Request), MaxRecordingBytes)
			require.Equal(t, "processing", a.state.Phase)
			return
		}
	}
	t.Fatal("byte bound did not end recording")
}
func TestStateWorstCaseBudget(t *testing.T) {
	a := New(agent.FactoryArgs{}).(*Agent)
	a.state = State{Turn: math.MaxUint64, Phase: "processing", Cursor: math.MaxUint64, Packets: math.MaxUint32, Outcome: agent.OutcomeUnknown, Seen: Window{SSRC: math.MaxUint32, High: math.MaxUint64, Mask: [2]uint64{math.MaxUint64, math.MaxUint64}, Set: true}, Resume: &ResumeSummary{Kind: string(legacy.Takeover), CheckpointAge: math.MaxInt64, SnapshotAge: math.MaxInt64, InputMayBeDuplicated: true, MoveOnly: agent.OutcomeUnknown}}
	s, err := a.Save(context.Background())
	require.NoError(t, err)
	require.LessOrEqual(t, len(s.Bytes), 1024)
	var got State
	require.NoError(t, json.Unmarshal(s.Bytes, &got))
	require.Equal(t, a.state, got)
}

func TestAdmissionRollbackKeepsRecording(t *testing.T) {
	ctx := context.Background()
	a := New(agent.FactoryArgs{}).(*Agent)
	_, err := a.Handle(ctx, input(1, []byte{0xf8, 0xff, 0xfe}))
	require.NoError(t, err)
	saved, err := a.Save(ctx)
	require.NoError(t, err)
	original, err := a.SaveMoveOnly(ctx)
	require.NoError(t, err)
	original = bytes.Clone(original)
	// A refused audio callback restores the earlier accepted prefix.
	_, err = a.Handle(ctx, input(2, []byte{0xf8, 0xff, 0xfe}))
	require.NoError(t, err)
	require.NoError(t, a.Restore(ctx, saved))
	restored, err := a.SaveMoveOnly(ctx)
	require.NoError(t, err)
	require.Equal(t, original, restored)
	// Task admission can be refused AFTER Handle and Save. Restore must retain
	// the real recording, so a move or the next Capacity event stays honest.
	_, err = a.Handle(ctx, agent.Event{Kind: agent.TimerFired, Timer: &agent.TimerNotice{ID: recordTimer}})
	require.NoError(t, err)
	_, err = a.Save(ctx)
	require.NoError(t, err)
	require.NoError(t, a.Restore(ctx, saved))
	restored, err = a.SaveMoveOnly(ctx)
	require.NoError(t, err)
	require.Equal(t, original, restored)
}

func TestTurnlessSnapshotCapacityIsVisible(t *testing.T) {
	a := New(agent.FactoryArgs{}).(*Agent)
	_, err := a.Handle(context.Background(), input(1, []byte{0xf8, 0xff, 0xfe}))
	require.NoError(t, err)
	effects, err := a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{TaskID: "host-refusal", Turn: 0, Sequence: 1, Outcome: agent.Capacity, Final: true}})
	require.NoError(t, err)
	require.EqualValues(t, 2, *effects.ClearAudioBefore)
	require.Equal(t, agent.Capacity, a.state.Outcome)
	require.EqualValues(t, 1, a.state.Refused)
	saved, err := a.Save(context.Background())
	require.NoError(t, err)
	require.Zero(t, saved.MoveOnlyLength)
}
