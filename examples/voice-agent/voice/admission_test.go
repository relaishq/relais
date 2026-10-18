package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func playingAgent(t *testing.T) *Agent {
	t.Helper()
	a := Registration(NewOpenAI("fixture")).Factory(agent.FactoryArgs{}).(*Agent)
	a.state = State{Turn: 10, Phase: "synthesizing", Reply: "The first sentence has enough words. The second sentence also has enough words."}
	for _, r := range []agent.TaskResult{{Turn: 11, TaskID: "speech0", Sequence: 1, Chunk: agent.Chunk{Data: recording(10)}}, {Turn: 11, TaskID: "speech0", Sequence: 2, Final: true, Outcome: agent.OK}} {
		_, err := a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &r})
		require.NoError(t, err)
	}
	return a
}
func TestRenderCapacityKeepsPlayingAndRetries(t *testing.T) {
	a := playingAgent(t)
	effects, err := a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 12, TaskID: "refused", Sequence: 1, Final: true, Outcome: agent.Capacity, Detail: "task admission capacity"}})
	require.NoError(t, err)
	require.Equal(t, "playing", a.state.Phase)
	require.Nil(t, effects.ClearAudioBefore)
	require.Nil(t, effects.CancelBefore)
	timers := map[string]time.Duration{}
	for _, timer := range effects.SetTimers {
		timers[timer.ID] = timer.After
	}
	require.Equal(t, 30*time.Second, timers["voice/capacity-deadline/v1"])
	require.Equal(t, 500*time.Millisecond, timers["voice/capacity-retry/v1"])
	effects, err = a.Handle(context.Background(), agent.Event{Kind: agent.PlaybackFinished, Playback: &agent.PlaybackNotice{Turn: 11}})
	require.NoError(t, err)
	require.Len(t, effects.StartTasks, 1)
	require.Equal(t, SpeechKind, effects.StartTasks[0].Kind)
	require.Equal(t, agent.Turn(12), effects.StartTasks[0].Turn)
	effects, err = a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 12, TaskID: "refused-again", Sequence: 1, Final: true, Outcome: agent.Capacity}})
	require.NoError(t, err)
	require.Len(t, effects.SetTimers, 1, "another refusal must not extend the first 30 s window")
	require.Equal(t, "voice/capacity-retry/v1", effects.SetTimers[0].ID)
}
func TestLaterRenderFailureDrainsPlayingSegment(t *testing.T) {
	a := playingAgent(t)
	effects, err := a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 12, TaskID: "speech1", Sequence: 1, Final: true, Outcome: agent.OutcomeUnknown, Detail: "provider_uncertain"}})
	require.NoError(t, err)
	require.Equal(t, "playing", a.state.Phase)
	require.Nil(t, effects.ClearAudioBefore)
	require.Nil(t, effects.CancelBefore)
	effects, err = a.Handle(context.Background(), agent.Event{Kind: agent.PlaybackFinished, Playback: &agent.PlaybackNotice{Turn: 11}})
	require.NoError(t, err)
	require.Equal(t, "idle", a.state.Phase)
	require.Equal(t, agent.OutcomeUnknown, a.state.Outcome)
	require.NotEmpty(t, a.state.Notice)
	require.Empty(t, effects.StartTasks)
}
func TestExactSpeechBudgetDoesNotRenderAgain(t *testing.T) {
	a := Registration(NewOpenAI("fixture")).Factory(agent.FactoryArgs{}).(*Agent)
	a.state = State{Turn: 10, Phase: "synthesizing", Reply: "The first sentence has enough words. The second sentence also has enough words.", SpeechNanos: SpeechTime - 200*time.Millisecond}
	_, err := a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 11, TaskID: "speech0", Sequence: 1, Chunk: agent.Chunk{Data: recording(10)}}})
	require.NoError(t, err)
	effects, err := a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 11, TaskID: "speech0", Sequence: 2, Final: true, Outcome: agent.OK}})
	require.NoError(t, err)
	require.Len(t, effects.StartTasks, 1)
	require.Equal(t, PlayKind, effects.StartTasks[0].Kind)
	require.Contains(t, strings.ToLower(a.state.Notice), "reply truncated")
}
func TestWarmCanceledBeforeTranscription(t *testing.T) {
	a := Registration(NewOpenAI("fixture")).Factory(agent.FactoryArgs{}).(*Agent)
	start, err := a.Handle(context.Background(), agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.start"}})
	require.NoError(t, err)
	require.Len(t, start.StartTasks, 1)
	a.buffer = recording(5)
	a.length = len(a.buffer)
	stop, err := a.Handle(context.Background(), agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.stop"}})
	require.NoError(t, err)
	require.NotNil(t, stop.CancelBefore)
	require.Less(t, start.StartTasks[0].Turn, *stop.CancelBefore)
	require.GreaterOrEqual(t, stop.StartTasks[0].Turn, *stop.CancelBefore)
}
func TestRestartNoticeExpiresOnStoreTimer(t *testing.T) {
	a := Registration(NewOpenAI("fixture")).Factory(agent.FactoryArgs{}).(*Agent)
	effects, err := a.Handle(context.Background(), agent.Event{Kind: agent.Resumed, Resume: &agent.ResumeNotice{ResumeNotice: legacy.ResumeNotice{Kind: legacy.Takeover}, MoveOnly: agent.OK}})
	require.NoError(t, err)
	found := false
	for _, timer := range effects.SetTimers {
		if timer.ID == "voice/restart-notice/v1" {
			found = true
			require.Equal(t, 60*time.Second, timer.After)
		}
	}
	require.True(t, found)
	_, err = a.Handle(context.Background(), agent.Event{Kind: agent.TimerFired, Timer: &agent.TimerNotice{ID: "voice/restart-notice/v1"}})
	require.NoError(t, err)
	_, err = a.Handle(context.Background(), agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.start"}})
	require.NoError(t, err)
	require.NotContains(t, a.state.Notice, "earlier speech may be missing")
}
func TestSpeechRequestKindsAreVersioned(t *testing.T) {
	require.Equal(t, "voice/speech/v2", SpeechKind)
	require.Equal(t, "voice/play/v2", PlayKind)
	require.EqualValues(t, 4, stateVersion)
}
func TestReplyTokenBudgetFitsShortSpeech(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tokens int `json:"max_completion_tokens"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.LessOrEqual(t, body.Tokens, 48)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Hello."}}]}`))
	}))
	defer server.Close()
	p := NewOpenAI("fixture")
	p.origin = server.URL
	_, err := p.Reply(context.Background(), "stable", "hi", nil)
	require.NoError(t, err)
}

func TestPlayCapacityRetainsTwoAcceptedSegmentsAcrossMove(t *testing.T) {
	a := playingAgent(t)
	// Play(0) has not emitted yet. Render(1) can complete before its refusal.
	_, err := a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 12, TaskID: "speech1", Sequence: 1, Chunk: agent.Chunk{Data: recording(5)}}})
	require.NoError(t, err)
	_, err = a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 12, TaskID: "speech1", Sequence: 2, Final: true, Outcome: agent.OK}})
	require.NoError(t, err)
	effects, err := a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 11, TaskID: "refused-play", Sequence: 1, Final: true, Outcome: agent.Capacity}})
	require.NoError(t, err)
	require.Nil(t, effects.ClearAudioBefore)
	saved, err := a.Save(context.Background())
	require.NoError(t, err)
	move, err := a.SaveMoveOnly(context.Background())
	require.NoError(t, err)
	_, err = a.Handle(context.Background(), agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.start"}})
	require.NoError(t, err)
	require.NoError(t, a.Restore(context.Background(), saved))
	rollback, err := a.SaveMoveOnly(context.Background())
	require.NoError(t, err)
	require.Equal(t, move, rollback, "callback rollback must restore both accepted segments")
	target := Registration(NewOpenAI("fixture")).Factory(agent.FactoryArgs{}).(*Agent)
	require.NoError(t, target.Restore(context.Background(), saved))
	require.NoError(t, target.RestoreMoveOnly(context.Background(), move))
	effects, err = target.Handle(context.Background(), agent.Event{Kind: agent.TimerFired, Timer: &agent.TimerNotice{ID: "voice/capacity-retry/v1"}})
	require.NoError(t, err)
	require.Len(t, effects.StartTasks, 1)
	require.Equal(t, PlayKind, effects.StartTasks[0].Kind)
	require.Equal(t, recording(10), effects.StartTasks[0].Request)
	// A second retry tick cannot duplicate an attempt already in flight.
	duplicate, err := target.Handle(context.Background(), agent.Event{Kind: agent.TimerFired, Timer: &agent.TimerNotice{ID: "voice/capacity-retry/v1"}})
	require.NoError(t, err)
	require.Empty(t, duplicate.StartTasks)
	_, err = target.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 11, TaskID: "retry-play", Sequence: 1, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{{0xf8, 0xff, 0xfe}}}}}})
	require.NoError(t, err)
	effects, err = target.Handle(context.Background(), agent.Event{Kind: agent.PlaybackFinished, Playback: &agent.PlaybackNotice{Turn: 11}})
	require.NoError(t, err)
	require.Len(t, effects.StartTasks, 1)
	require.Equal(t, recording(5), effects.StartTasks[0].Request)
}
func TestCapacityOutageDeadlineWaitsForPlayingAudio(t *testing.T) {
	a := playingAgent(t)
	_, err := a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 11, TaskID: "play0", Sequence: 1, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{{0xf8, 0xff, 0xfe}}}}}})
	require.NoError(t, err)
	_, err = a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 12, TaskID: "refused", Sequence: 1, Final: true, Outcome: agent.Capacity}})
	require.NoError(t, err)
	effects, err := a.Handle(context.Background(), agent.Event{Kind: agent.TimerFired, Timer: &agent.TimerNotice{ID: "voice/capacity-deadline/v1"}})
	require.NoError(t, err)
	require.Nil(t, effects.ClearAudioBefore)
	require.Equal(t, "playing", a.state.Phase)
	effects, err = a.Handle(context.Background(), agent.Event{Kind: agent.PlaybackFinished, Playback: &agent.PlaybackNotice{Turn: 11}})
	require.NoError(t, err)
	require.Empty(t, effects.StartTasks)
	require.Equal(t, "idle", a.state.Phase)
	require.Equal(t, agent.Capacity, a.state.Outcome)
	require.Contains(t, a.state.Notice, "30 s")
}
func TestRemainingSpeechLimitFinishesCurrentSegment(t *testing.T) {
	a := playingAgent(t)
	effects, err := a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 12, TaskID: "speech1", Sequence: 1, Final: true, Outcome: agent.Failed, Detail: "speech_limit"}})
	require.NoError(t, err)
	require.Nil(t, effects.ClearAudioBefore)
	require.Equal(t, "playing", a.state.Phase)
	_, err = a.Handle(context.Background(), agent.Event{Kind: agent.PlaybackFinished, Playback: &agent.PlaybackNotice{Turn: 11}})
	require.NoError(t, err)
	require.Equal(t, agent.OK, a.state.Outcome)
	require.Contains(t, strings.ToLower(a.state.Notice), "reply truncated")
}

func TestSpeechLimitIsDefiniteWithoutRetry(t *testing.T) {
	fixture, err := os.ReadFile("testdata/speech.ogg")
	require.NoError(t, err)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); _, _ = w.Write(fixture) }))
	defer server.Close()
	p := NewOpenAI("fixture")
	p.origin = server.URL
	_, err = renderSpeech(context.Background(), p, "stable", speechRequest{Text: "Hello.", Bytes: MaxSegmentBytes, Nanos: 20 * time.Millisecond})
	var provider *ProviderError
	require.ErrorAs(t, err, &provider)
	require.Equal(t, "speech_limit", provider.Code)
	require.False(t, provider.Uncertain)
	require.EqualValues(t, 1, calls.Load())
}

func TestPlayFailureBeforeAudioDoesNotWaitForPlayback(t *testing.T) {
	a := playingAgent(t)
	_, err := a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 11, TaskID: "play0", Sequence: 1, Final: true, Outcome: agent.Failed, Detail: "invalid_audio"}})
	require.NoError(t, err)
	require.Equal(t, "idle", a.state.Phase)
	require.Equal(t, agent.Failed, a.state.Outcome)
}

func TestRenderFailureNoticeSurvivesPlayCapacity(t *testing.T) {
	a := playingAgent(t)
	for _, r := range []agent.TaskResult{
		{Turn: 12, TaskID: "speech1", Sequence: 1, Final: true, Outcome: agent.OutcomeUnknown, Detail: "provider_uncertain"},
		{Turn: 11, TaskID: "refused-play", Sequence: 1, Final: true, Outcome: agent.Capacity},
	} {
		_, err := a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &r})
		require.NoError(t, err)
	}
	_, err := a.Handle(context.Background(), agent.Event{Kind: agent.TimerFired, Timer: &agent.TimerNotice{ID: "voice/capacity-retry/v1"}})
	require.NoError(t, err)
	_, err = a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 11, TaskID: "play-retry", Sequence: 1, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{{0xf8, 0xff, 0xfe}}}}}})
	require.NoError(t, err)
	_, err = a.Handle(context.Background(), agent.Event{Kind: agent.PlaybackFinished, Playback: &agent.PlaybackNotice{Turn: 11}})
	require.NoError(t, err)
	require.Equal(t, "idle", a.state.Phase)
	require.Equal(t, agent.OutcomeUnknown, a.state.Outcome)
	require.Contains(t, a.state.Notice, "outcome_unknown")
}
