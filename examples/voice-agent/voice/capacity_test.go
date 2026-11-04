package voice

import (
	"context"
	"testing"
	"time"

	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func voiceEvent(t *testing.T, a *Agent, e agent.Event) agent.Effects {
	t.Helper()
	effects, err := a.Handle(context.Background(), e)
	require.NoError(t, err)
	return effects
}

func capacityResult(turn agent.Turn, task agent.TaskID) agent.Event {
	return agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: turn, TaskID: task, Sequence: 1, Final: true, Outcome: agent.Capacity}}
}

func voiceTimer(id string) agent.Event {
	return agent.Event{Kind: agent.TimerFired, Timer: &agent.TimerNotice{ID: id}}
}

func requireCapacityWindow(t *testing.T, effects agent.Effects) {
	t.Helper()
	require.Contains(t, effects.SetTimers, agent.TimerSpec{ID: capacityTimer, After: 30 * time.Second})
	require.Contains(t, effects.SetTimers, agent.TimerSpec{ID: retryTimer, After: 500 * time.Millisecond})
}

func TestCapacityRecoveryStartsFreshWindows(t *testing.T) {
	a := playingAgent(t)
	// Use the existing timer-event seam: recovery occurs before the first
	// deadline, and another refusal occurs later without ending this turn.
	requireCapacityWindow(t, voiceEvent(t, a, capacityResult(12, "refused")))
	voiceEvent(t, a, voiceTimer(retryTimer))
	effects := voiceEvent(t, a, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 12, TaskID: "recovered", Sequence: 1, Chunk: agent.Chunk{Data: recording(5)}}})
	require.Contains(t, effects.CancelTimers, capacityTimer, "recovery must invalidate the first deadline generation")
	require.False(t, a.state.CapacityWindow)
	require.False(t, a.state.CapacityExpired)
	// Even a delivered old notice while no window is open cannot poison it.
	voiceEvent(t, a, voiceTimer(capacityTimer))
	require.False(t, a.state.CapacityExpired)
	voiceEvent(t, a, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 12, TaskID: "recovered", Sequence: 2, Final: true, Outcome: agent.OK}})
	voiceEvent(t, a, agent.Event{Kind: agent.PlaybackFinished, Playback: &agent.PlaybackNotice{Turn: 11}})
	requireCapacityWindow(t, voiceEvent(t, a, capacityResult(12, "play-refused")))
	voiceEvent(t, a, voiceTimer(retryTimer))
	effects = voiceEvent(t, a, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 12, TaskID: "play-recovered", Sequence: 1, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Packets: [][]byte{{0xf8, 0xff, 0xfe}}}}}})
	require.Contains(t, effects.CancelTimers, capacityTimer)
	require.False(t, a.state.CapacityWindow)
	require.False(t, a.state.CapacityExpired)
	voiceEvent(t, a, agent.Event{Kind: agent.PlaybackFinished, Playback: &agent.PlaybackNotice{Turn: 12}})
	require.Equal(t, "idle", a.state.Phase)
	voiceEvent(t, a, agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.start"}})
	voiceEvent(t, a, agent.Event{Kind: agent.AudioInput, Input: &legacy.Input{Payload: []byte{0xf8, 0xff, 0xfe}, Progress: legacy.Progress{SSRC: 1, Index: 1}}})
	voiceEvent(t, a, agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.stop"}})
	for _, r := range []agent.TaskResult{
		{Turn: a.state.Turn, TaskID: "stt", Sequence: 1, Chunk: agent.Chunk{Data: []byte("Hello")}},
		{Turn: a.state.Turn, TaskID: "stt", Sequence: 2, Final: true, Outcome: agent.OK},
		{Turn: a.state.Turn, TaskID: "reply", Sequence: 1, Chunk: agent.Chunk{Data: []byte("A short reply with enough words.")}},
		{Turn: a.state.Turn, TaskID: "reply", Sequence: 2, Final: true, Outcome: agent.OK},
	} {
		voiceEvent(t, a, agent.Event{Kind: agent.TaskMessage, Result: &r})
	}
	requireCapacityWindow(t, voiceEvent(t, a, capacityResult(a.state.Turn+1, "next-turn-refused")))
}

func TestCapacityDeadlineKeepsAdmittedPlayBeforeFirstChunk(t *testing.T) {
	a := playingAgent(t)
	require.Positive(t, a.state.PlayLength)
	require.False(t, a.state.RetryPlay)
	voiceEvent(t, a, capacityResult(12, "render-refused"))
	effects := voiceEvent(t, a, voiceTimer(capacityTimer))
	require.Nil(t, effects.CancelBefore, "an admitted Play must drain even before its first chunk")
	require.Nil(t, effects.ClearAudioBefore)
	require.Equal(t, "playing", a.state.Phase)
	// The admitted task can still supply audio after the later render expires.
	effects = voiceEvent(t, a, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 11, TaskID: "play", Sequence: 1, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Packets: [][]byte{{0xf8, 0xff, 0xfe}}}}}})
	require.Len(t, effects.Audio, 1)
	voiceEvent(t, a, agent.Event{Kind: agent.PlaybackFinished, Playback: &agent.PlaybackNotice{Turn: 11}})
	require.Equal(t, "idle", a.state.Phase)
	require.Equal(t, agent.Capacity, a.state.Outcome)
	require.Equal(t, "capacity", a.state.Error)
}

func TestRenderRetryEndClosesCapacityWindow(t *testing.T) {
	for _, detail := range []string{"provider_uncertain", "speech_limit"} {
		for _, pendingPlay := range []bool{false, true} {
			name := detail + "/admitted-play"
			if pendingPlay {
				name = detail + "/pending-play"
			}
			t.Run(name, func(t *testing.T) {
				a := playingAgent(t)
				voiceEvent(t, a, capacityResult(12, "render-refused"))
				if pendingPlay {
					voiceEvent(t, a, capacityResult(11, "play-refused"))
				}
				voiceEvent(t, a, voiceTimer(retryTimer))
				outcome, wantOutcome := agent.OutcomeUnknown, agent.OutcomeUnknown
				if detail == "speech_limit" {
					outcome, wantOutcome = agent.Failed, agent.OK
				}
				effects := voiceEvent(t, a, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 12, TaskID: "render-retry", Sequence: 1, Final: true, Outcome: outcome, Detail: detail}})
				require.Nil(t, effects.CancelBefore)
				require.Nil(t, effects.ClearAudioBefore)
				if pendingPlay {
					require.True(t, a.state.CapacityWindow, "Play still needs the same outage window")
					require.NotContains(t, effects.CancelTimers, capacityTimer)
				} else {
					require.Contains(t, effects.CancelTimers, capacityTimer, "ending the last render retry must cancel its deadline")
					require.Contains(t, effects.CancelTimers, retryTimer)
					require.False(t, a.state.CapacityWindow)
					voiceEvent(t, a, voiceTimer(capacityTimer))
					require.False(t, a.state.CapacityExpired)
					// A late Play refusal gets a full new budget without replacing
					// the render failure or the successful truncation outcome.
					requireCapacityWindow(t, voiceEvent(t, a, capacityResult(11, "late-play-refused")))
					voiceEvent(t, a, voiceTimer(retryTimer))
				}
				effects = voiceEvent(t, a, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 11, TaskID: "play-retry", Sequence: 1, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Packets: [][]byte{{0xf8, 0xff, 0xfe}}}}}})
				require.Len(t, effects.Audio, 1)
				require.Contains(t, effects.CancelTimers, capacityTimer)
				voiceEvent(t, a, voiceTimer(capacityTimer))
				require.False(t, a.state.CapacityExpired)
				voiceEvent(t, a, agent.Event{Kind: agent.PlaybackFinished, Playback: &agent.PlaybackNotice{Turn: 11}})
				require.Equal(t, "idle", a.state.Phase)
				require.Equal(t, wantOutcome, a.state.Outcome)
				if detail == "speech_limit" {
					require.Contains(t, a.state.Notice, "Reply truncated")
				} else {
					require.Equal(t, detail, a.state.Error)
				}
			})
		}
	}
}

func TestTextAdmissionCapacityRetries(t *testing.T) {
	for _, phase := range []string{"transcribing", "replying"} {
		for _, recover := range []bool{true, false} {
			name := phase + "/expires"
			if recover {
				name = phase + "/recovers"
			}
			t.Run(name, func(t *testing.T) {
				a := Registration(NewOpenAI("fixture")).Factory(agent.FactoryArgs{}).(*Agent)
				voiceEvent(t, a, agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.start"}})
				voiceEvent(t, a, agent.Event{Kind: agent.AudioInput, Input: &legacy.Input{Payload: []byte{0xf8, 0xff, 0xfe}, Progress: legacy.Progress{SSRC: 1, Index: 1}}})
				effects := voiceEvent(t, a, agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.stop"}})
				if phase == "replying" {
					voiceEvent(t, a, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: a.state.Turn, TaskID: "stt", Sequence: 1, Chunk: agent.Chunk{Data: []byte("Please keep my speech.")}}})
					effects = voiceEvent(t, a, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: a.state.Turn, TaskID: "stt", Sequence: 2, Final: true, Outcome: agent.OK}})
				}
				require.Len(t, effects.StartTasks, 1)
				original := effects.StartTasks[0]
				effects = voiceEvent(t, a, capacityResult(a.state.Turn, "refused"))
				require.Equal(t, phase, a.state.Phase, "admission pressure must preserve the user's turn")
				requireCapacityWindow(t, effects)
				// Pending recording and retry state survive a move and callback rollback.
				saved, err := a.Save(context.Background())
				require.NoError(t, err)
				move, err := a.SaveMoveOnly(context.Background())
				require.NoError(t, err)
				if phase == "transcribing" {
					require.Equal(t, original.Request, move)
				}
				voiceEvent(t, a, agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{Name: "ptt.start"}})
				require.NoError(t, a.Restore(context.Background(), saved))
				rollback, err := a.SaveMoveOnly(context.Background())
				require.NoError(t, err)
				require.Equal(t, move, rollback)
				target := Registration(NewOpenAI("fixture")).Factory(agent.FactoryArgs{}).(*Agent)
				require.NoError(t, target.Restore(context.Background(), saved))
				require.NoError(t, target.RestoreMoveOnly(context.Background(), move))
				a = target
				effects = voiceEvent(t, a, voiceTimer(retryTimer))
				require.Equal(t, []agent.TaskSpec{original}, effects.StartTasks, "retry must use the identical recording or reply request")
				require.Empty(t, voiceEvent(t, a, voiceTimer(retryTimer)).StartTasks, "one attempt at a time")
				effects = voiceEvent(t, a, capacityResult(a.state.Turn, "refused-again"))
				require.Equal(t, []agent.TimerSpec{{ID: retryTimer, After: 500 * time.Millisecond}}, effects.SetTimers, "repeated refusals must not extend the window")
				if !recover {
					effects = voiceEvent(t, a, voiceTimer(capacityTimer))
					require.Equal(t, "idle", a.state.Phase)
					require.Equal(t, agent.Capacity, a.state.Outcome)
					require.Equal(t, "capacity", a.state.Error)
					require.Contains(t, a.state.Notice, "30 s")
					require.Empty(t, effects.StartTasks)
					require.Empty(t, voiceEvent(t, a, voiceTimer(retryTimer)).StartTasks)
					return
				}
				voiceEvent(t, a, voiceTimer(retryTimer))
				effects = voiceEvent(t, a, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: a.state.Turn, TaskID: "accepted", Sequence: 1, Chunk: agent.Chunk{Data: []byte("Please keep my speech.")}}})
				require.Contains(t, effects.CancelTimers, capacityTimer)
				require.False(t, a.state.CapacityWindow)
				require.False(t, a.state.CapacityExpired)
				effects = voiceEvent(t, a, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: a.state.Turn, TaskID: "accepted", Sequence: 2, Final: true, Outcome: agent.OK}})
				require.Equal(t, "Please keep my speech.", a.state.Transcript)
				require.Len(t, effects.StartTasks, 1)
				if phase == "transcribing" {
					require.Equal(t, ReplyKind, effects.StartTasks[0].Kind)
				} else {
					require.Equal(t, SpeechKind, effects.StartTasks[0].Kind)
				}
			})
		}
	}
}
