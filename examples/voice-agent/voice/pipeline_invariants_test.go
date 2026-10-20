package voice

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestPipelineAcceptanceCursorsAndMoveRollback(t *testing.T) {
	ctx := context.Background()
	a := Registration(NewOpenAI("not-a-key")).Factory(agent.FactoryArgs{}).(*Agent)
	a.state = State{Turn: 10, Phase: "synthesizing", Reply: "The first sentence has enough words. The second sentence also has enough words."}
	first := recording(10)
	_, err := a.Handle(ctx, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 11, TaskID: "speech-0", Sequence: 1, Chunk: agent.Chunk{Data: first}}})
	require.NoError(t, err)
	first[1] = 0 // callbacks retain their own accepted copy
	accepted, err := a.SaveMoveOnly(ctx)
	require.NoError(t, err)
	require.Equal(t, recording(10), accepted)
	effects, err := a.Handle(ctx, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 11, TaskID: "speech-0", Sequence: 2, Final: true, Outcome: agent.OK}})
	require.NoError(t, err)
	require.Len(t, effects.StartTasks, 2, "play and next render must start in the same callback consuming speech Final")
	require.Equal(t, PlayKind, effects.StartTasks[0].Kind)
	require.Equal(t, recording(10), effects.StartTasks[0].Request)
	require.Equal(t, SpeechKind, effects.StartTasks[1].Kind)
	// Later speech and current playback must independently accept sequence 1.
	_, err = a.Handle(ctx, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 12, TaskID: "speech-1", Sequence: 1, Chunk: agent.Chunk{Data: recording(5)}}})
	require.NoError(t, err)
	effects, err = a.Handle(ctx, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 11, TaskID: "play-0", Sequence: 1, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{{0xf8, 0xff, 0xfe}}}}}})
	require.NoError(t, err)
	require.Len(t, effects.Audio, 1)
	require.Equal(t, agent.TaskID("play-0"), effects.Audio[0].Source.TaskID)
	require.EqualValues(t, 1, a.state.RenderCursor)
	require.EqualValues(t, 1, a.state.PlayCursor)
	saved, err := a.Save(ctx)
	require.NoError(t, err)
	move, err := a.SaveMoveOnly(ctx)
	require.NoError(t, err)
	require.True(t, saved.HasMoveOnly)
	target := Registration(NewOpenAI("not-a-key")).Factory(agent.FactoryArgs{}).(*Agent)
	require.NoError(t, target.Restore(ctx, saved))
	require.NoError(t, target.RestoreMoveOnly(ctx, move))
	// A refused barge-in restores the pending segment, including its exact length.
	_, err = a.Handle(ctx, agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.start"}})
	require.NoError(t, err)
	require.NoError(t, a.Restore(ctx, saved))
	again, err := a.SaveMoveOnly(ctx)
	require.NoError(t, err)
	require.Equal(t, move, again)
}
func TestPipelineWorstStateFitsDeclaredBudget(t *testing.T) {
	a := worstStateAgent("x")
	saved, err := a.Save(context.Background())
	require.NoError(t, err)
	// 5,632 bounded text bytes + 106 notice bytes + 22 error bytes +
	// 1,327 bytes of field names, punctuation, IDs, numbers and flags.
	require.Equal(t, 5632+106+22+1327, len(saved.Bytes))
	require.LessOrEqual(t, len(saved.Bytes), MaxStateBytes)
	var roundTrip State
	require.NoError(t, json.Unmarshal(saved.Bytes, &roundTrip))
	require.Equal(t, a.state, roundTrip)
	t.Logf("worst pipeline app state %d / %d bytes", len(saved.Bytes), MaxStateBytes)
}

func worstStateAgent(alphabet string) *Agent {
	a := Registration(NewOpenAI("")).Factory(agent.FactoryArgs{}).(*Agent)
	text := strings.Repeat(alphabet, 4096)
	a.state = State{Turn: math.MaxUint64 - 30, Phase: "synthesizing", Transcript: boundedText(text, 1024), Reply: boundedText(text, MaxReplyBytes), Outcome: agent.OutcomeUnknown, Error: "provider_configuration", Notice: strings.Repeat("N", 106), RestartNotice: true, Cursor: math.MaxUint64, RenderCursor: math.MaxUint64, PlayCursor: math.MaxUint64, Task: agent.TaskID(strings.Repeat("A", 43)), RenderTask: agent.TaskID(strings.Repeat("B", 43)), PlayTask: agent.TaskID(strings.Repeat("C", 43)), Segment: 7, Render: 7, RenderReady: true, PlayLength: MaxSegmentBytes, RetryPlay: true, RetryRender: true, PlayRetryInFlight: true, RenderRetryInFlight: true, CapacityWindow: true, CapacityExpired: true, StopAfterPlayback: true, SpeechBytes: MaxSpeechBytes, SpeechNanos: SpeechTime, Seen: Window{SSRC: math.MaxUint32, High: math.MaxUint64, Mask: [2]uint64{math.MaxUint64, math.MaxUint64}, Set: true}}
	for range 4 {
		a.state.History = append(a.state.History, Exchange{boundedText(text, 512), boundedText(text, 512)})
	}
	a.state.ReleaseAt = math.MaxInt64
	a.state.Timings = Timings{TranscribeFinished: math.MaxInt64, ReplyStarted: math.MaxInt64, ReplyFinished: math.MaxInt64, ReplyCharacters: MaxReplyBytes, FirstSentenceCharacters: MaxSegmentRunes, SpeechStarted: math.MaxInt64, SpeechFinished: math.MaxInt64, SpeechAudio: math.MaxInt64, FirstQueued: math.MaxInt64}
	return a
}
