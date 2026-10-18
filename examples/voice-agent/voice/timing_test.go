package voice

import (
	"context"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestTurnTimingUsesHostClockAndSurvivesMove(t *testing.T) {
	ctx := context.Background()
	a := Registration(NewOpenAI("fixture")).Factory(agent.FactoryArgs{}).(*Agent)
	release := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	a.state = State{Turn: 10, Phase: "recording"}
	a.buffer = recording(5)
	a.length = len(a.buffer)
	_, err := a.Handle(ctx, agent.Event{At: release, Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.stop"}})
	require.NoError(t, err)
	chunk := agent.Chunk{Data: []byte("Hello")}
	_, err = a.Handle(ctx, agent.Event{At: release.Add(time.Second), Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 10, TaskID: "stt", Sequence: 1, Chunk: chunk}})
	require.NoError(t, err)
	saved, err := a.Save(ctx)
	require.NoError(t, err)
	b := Registration(NewOpenAI("fixture")).Factory(agent.FactoryArgs{}).(*Agent)
	require.NoError(t, b.Restore(ctx, saved))
	require.Equal(t, a.state.Timings, b.state.Timings)
	_, err = b.Handle(ctx, agent.Event{At: release.Add(2 * time.Second), Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 10, TaskID: "stt", Sequence: 2, Final: true, Outcome: agent.OK}})
	require.NoError(t, err)
	require.EqualValues(t, 0, b.state.Timings.TranscribeStarted)
	require.EqualValues(t, time.Second, b.state.Timings.TranscribeFinished)
	require.EqualValues(t, 0, b.state.Timings.RecordingClosed)
	saved.Version = 3
	require.ErrorIs(t, b.Restore(ctx, saved), agent.ErrVersion)
}
func TestFirstSpeechAndQueuedTimingKeepOriginalAcceptedValues(t *testing.T) {
	a := Registration(NewOpenAI("fixture")).Factory(agent.FactoryArgs{}).(*Agent)
	at := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	a.state = State{Turn: 10, Phase: "synthesizing", ReleaseAt: at.UnixNano(), Reply: "A short reply with enough words.", Timings: Timings{SpeechStarted: int64(2 * time.Second)}}
	audio := recording(5)
	for _, e := range []agent.Event{
		{At: at.Add(4 * time.Second), Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 11, TaskID: "speech", Sequence: 1, Chunk: agent.Chunk{Data: audio}}},
		{At: at.Add(5 * time.Second), Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 11, TaskID: "speech", Sequence: 2, Final: true, Outcome: agent.OK}},
		{At: at.Add(6 * time.Second), Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 11, TaskID: "play", Sequence: 1, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Packets: [][]byte{{0xf8, 0xff, 0xfe}}}}}},
		{At: at.Add(7 * time.Second), Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 11, TaskID: "play", Sequence: 2, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Packets: [][]byte{{0xf8, 0xff, 0xfe}}}}}},
	} {
		_, err := a.Handle(context.Background(), e)
		require.NoError(t, err)
	}
	require.EqualValues(t, 2*time.Second, a.state.Timings.SpeechStarted)
	require.EqualValues(t, 4*time.Second, a.state.Timings.SpeechFinished)
	require.EqualValues(t, 100*time.Millisecond, a.state.Timings.SpeechAudio)
	require.EqualValues(t, 6*time.Second, a.state.Timings.FirstQueued)
}

func TestStageStartsComeFromCallbacksAndClockCanStepBackward(t *testing.T) {
	ctx := context.Background()
	a := Registration(NewOpenAI("fixture")).Factory(agent.FactoryArgs{}).(*Agent)
	at := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	a.state = State{Turn: 10, Phase: "transcribing", Transcript: "Hello", ReleaseAt: at.UnixNano()}
	effects, err := a.Handle(ctx, agent.Event{At: at.Add(time.Second), Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 10, TaskID: "stt", Sequence: 2, Final: true, Outcome: agent.OK}})
	require.NoError(t, err)
	require.Equal(t, ReplyKind, effects.StartTasks[0].Kind)
	require.EqualValues(t, time.Second, a.state.Timings.ReplyStarted)
	_, err = a.Handle(ctx, agent.Event{At: at.Add(500 * time.Millisecond), Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 10, TaskID: "reply", Sequence: 1, Chunk: agent.Chunk{Data: []byte("A short reply with enough words.")}}})
	require.NoError(t, err)
	require.EqualValues(t, 500*time.Millisecond, a.state.Timings.ReplyFinished)
	effects, err = a.Handle(ctx, agent.Event{At: at.Add(2 * time.Second), Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 10, TaskID: "reply", Sequence: 2, Final: true, Outcome: agent.OK}})
	require.NoError(t, err)
	require.Equal(t, SpeechKind, effects.StartTasks[0].Kind)
	require.EqualValues(t, 2*time.Second, a.state.Timings.SpeechStarted)
}
