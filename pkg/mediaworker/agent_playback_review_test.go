package mediaworker

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pion/srtp/v3"
	"net"
	"strings"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

// Probe: a streaming TTS that emits one Opus packet per Emit. Each result is a
// separate span in the persisted window. Does an ordinary reply overflow the
// 2 KiB host metadata budget and end the call?
func reviewStreamingSession(t *testing.T, packet []byte, n int) (*session, *v2UnitAgent, agent.TaskID) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	id := deriveTaskID(s.id, "unit/v1", "started/v2-unit", "tts")
	spec := agent.TaskSpec{Step: "tts", Kind: "tts/v1", Turn: 1, Class: agent.ReadOnly, Resume: agent.Retry}
	intent := taskIntent{ID: id, Step: spec.Step, Kind: spec.Kind, Turn: 1, Class: agent.ReadOnly, Resume: agent.Retry, Phase: taskStarted, Digest: taskDigest(spec)}
	task := &localTask{intent: intent, terminalBytes: taskTerminalBytes(intent), launched: true}
	s.agent.v2.tasks = []*localTask{task}
	for seq := 1; seq <= n; seq++ {
		r := agent.TaskResult{TaskID: id, Sequence: uint64(seq), Turn: 1, Outcome: agent.OK, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{packet}}}}
		task.results = append(task.results, r)
		task.mailboxBytes += taskResultBytes(r)
		task.intent.Published = uint64(seq)
		require.NoError(t, s.registerAudioLocked(r))
	}
	s.publishTaskIntents()
	s.publishAgent()
	return s, a, id
}

func TestPlaybackReviewWindowSpanBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		packet []byte
	}{{"20ms", []byte{0xf8, 0xff, 0xfe}}, {"10ms", []byte{0xf0, 0xff, 0xfe}}, {"2.5ms", []byte{0x80, 0xff}}} {
		t.Run(tc.name, func(t *testing.T) {
			s, a, id := reviewStreamingSession(t, tc.packet, 200)
			for seq := 1; seq <= 200; seq++ {
				r := s.agent.v2.tasks[0].results[0]
				require.EqualValues(t, seq, r.Sequence)
				a.effects = agent.Effects{Audio: []agent.Audio{{Turn: 1, Source: agent.AudioRef{TaskID: id, Sequence: uint64(seq)}}}}
				err := s.handleV2Event(agent.Event{ID: agent.EventID(fmt.Sprint("task/", seq)), Kind: agent.TaskMessage, Result: &r})
				raw, _ := json.Marshal(s.state.Agent.V2)
				require.NoError(t, err, "streaming window must stay bounded")
				if err != nil {
					t.Logf("%s: callback for result %d FAILED: %v (envelope %d bytes, window spans %d)", tc.name, seq, err, len(raw), len(s.state.Agent.V2.Playback.Window))
					return
				}
			}
			raw, _ := json.Marshal(s.state.Agent.V2)
			t.Logf("%s: all 200 accepted; envelope %d bytes, spans %d", tc.name, len(raw), len(s.state.Agent.V2.Playback.Window))
		})
	}
}

// Probe: 20 ms packets, one per Emit, plus two other live tasks (STT, LLM).
func TestPlaybackReviewWindowSpanBudgetWithOtherTasks(t *testing.T) {
	s, a, id := reviewStreamingSession(t, []byte{0xf8, 0xff, 0xfe}, 200)
	for _, step := range []string{"stt", "llm"} {
		spec := agent.TaskSpec{Step: step, Kind: "tts/v1", Turn: 1, Class: agent.ReadOnly, Resume: agent.Retry}
		other := taskIntent{ID: deriveTaskID(s.id, "unit/v1", "started/v2-unit", step), Step: step, Kind: spec.Kind, Turn: 1, Class: agent.ReadOnly, Resume: agent.Retry, Phase: taskStarted, Digest: taskDigest(spec)}
		s.agent.v2.tasks = append(s.agent.v2.tasks, &localTask{intent: other, terminalBytes: taskTerminalBytes(other), launched: true})
	}
	s.publishTaskIntents()
	s.publishAgent()
	for seq := 1; seq <= 200; seq++ {
		r := s.agent.v2.tasks[0].results[0]
		a.effects = agent.Effects{Audio: []agent.Audio{{Turn: 1, Source: agent.AudioRef{TaskID: id, Sequence: uint64(seq)}}}}
		err := s.handleV2Event(agent.Event{ID: agent.EventID(fmt.Sprint("task/", seq)), Kind: agent.TaskMessage, Result: &r})
		require.NoError(t, err, "live tasks cannot block an ordinary audio result")
		if err != nil {
			raw, _ := json.Marshal(s.state.Agent.V2)
			t.Logf("callback for result %d FAILED: %v (envelope %d bytes, spans %d)", seq, err, len(raw), len(s.state.Agent.V2.Playback.Window))
			return
		}
	}
	t.Logf("all accepted")
}

// Probe: an app that arms one per-turn timer ("no-reply/<turn>") and cancels it
// when the user answers. Fired/cancelled entries are never removed.
func TestPlaybackReviewTimerEntriesAccumulate(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	for turn := 1; turn <= 200; turn++ {
		id := fmt.Sprintf("no-reply/%d", turn)
		a.effects = agent.Effects{SetTimers: []agent.TimerSpec{{ID: id, After: 8 * 1e9}}}
		err := s.handleV2Event(agent.Event{ID: agent.EventID(fmt.Sprint("arm", turn)), Kind: agent.Started})
		require.NoError(t, err, "new timer IDs must not accumulate")
		if err != nil {
			raw, _ := json.Marshal(s.state.Agent.V2)
			t.Logf("turn %d: arming FAILED: %v (timers %d, envelope %d bytes)", turn, err, len(s.agent.latest.V2.Timers), len(raw))
			return
		}
		a.effects = agent.Effects{CancelTimers: []string{id}}
		err = s.handleV2Event(agent.Event{ID: agent.EventID(fmt.Sprint("answer", turn)), Kind: agent.Started})
		require.NoError(t, err)
		if err != nil {
			t.Logf("turn %d: cancel FAILED: %v", turn, err)
			return
		}
	}
	t.Logf("500 turns ok; timers %d", len(s.agent.latest.V2.Timers))
}

// Probe: cost of pruneTasks under the media lock with a long buffered reply.
func TestPlaybackReviewPruneCost(t *testing.T) {
	s, _, id := reviewStreamingSession(t, []byte{0xf8, 0xff, 0xfe}, 1)
	task := s.agent.v2.tasks[0]
	task.results = nil
	for seq := 1; seq <= 760; seq++ {
		r := agent.TaskResult{TaskID: id, Sequence: uint64(seq), Turn: 1, Outcome: agent.OK, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{{0xf8}, {0xf8}, {0xf8}, {0xf8}, {0xf8}}}}}
		s.agent.v2.playback.payloads[audioKey{id, uint64(seq)}] = playbackPayload{r.Chunk.Audio, make([]time.Duration, 5)}
		task.audio = append(task.audio, r)
		for o := range 5 {
			s.agent.v2.playback.queue = append(s.agent.v2.playback.queue, agent.Audio{Turn: 1, Source: agent.AudioRef{TaskID: id, Sequence: uint64(seq), PacketOffset: uint32(o)}})
		}
	}
	task.intent.Cursor = 760
	task.intent.Published = 760
	s.publishTaskIntents()
	s.publishAgent()
	s.durableAgent = cloneAgentState(s.state.Agent)
	start := time.Now()
	for range 10 {
		s.pruneTasks()
	}
	elapsed := time.Since(start) / 10
	t.Logf("pruneTasks with 760 live results / 3800 queued refs: %v per call", elapsed)
	require.Less(t, elapsed, 8*time.Millisecond, "prune must not scan every reference for every retained result")
}

// Probe: barge-in that clears audio and starts the next turn's STT while the
// window is full of one-packet-per-Emit spans.
func TestPlaybackReviewBargeInBlockedByWindow(t *testing.T) {
	s, a, id := reviewStreamingSession(t, []byte{0xf8, 0xff, 0xfe}, 40)
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"stt/v1": func(context.Context, *agent.TaskRun) error { return nil }}
	for seq := 1; seq <= 15; seq++ {
		r := s.agent.v2.tasks[0].results[0]
		a.effects = agent.Effects{Audio: []agent.Audio{{Turn: 1, Source: agent.AudioRef{TaskID: id, Sequence: uint64(seq)}}}}
		require.NoError(t, s.handleV2Event(agent.Event{ID: agent.EventID(fmt.Sprint("task/", seq)), Kind: agent.TaskMessage, Result: &r}))
	}
	t.Logf("queued %d packets, window spans %d", len(s.agent.v2.playback.queue), len(s.state.Agent.V2.Playback.Window))
	turn := agent.Turn(2)
	a.effects = agent.Effects{ClearAudioBefore: &turn, CancelBefore: &turn, StartTasks: []agent.TaskSpec{{Step: "stt", Kind: "stt/v1", Turn: 2, Class: agent.ReadOnly, Resume: agent.Retry, Request: []byte("x")}}}
	err := s.handleV2Event(agent.Event{ID: "audio/1/100", Kind: agent.Started})
	require.NoError(t, err, "clear/cancel/start must use the cleared window")
	require.Empty(t, s.agent.v2.playback.queue)
	t.Logf("barge-in callback: err=%v; queue after: %d", err, len(s.agent.v2.playback.queue))
}

func TestPlaybackReviewBargeInBlockedByWindowLiveTasks(t *testing.T) {
	s, a, id := reviewStreamingSession(t, []byte{0xf8, 0xff, 0xfe}, 40)
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"stt/v1": func(context.Context, *agent.TaskRun) error { return nil }}
	spec := agent.TaskSpec{Step: "llm", Kind: "stt/v1", Turn: 1, Class: agent.ReadOnly, Resume: agent.Retry}
	other := taskIntent{ID: deriveTaskID(s.id, "unit/v1", "started/v2-unit", "llm"), Step: "llm", Kind: spec.Kind, Turn: 1, Class: agent.ReadOnly, Resume: agent.Retry, Phase: taskStarted, Digest: taskDigest(spec)}
	s.agent.v2.tasks = append(s.agent.v2.tasks, &localTask{intent: other, terminalBytes: taskTerminalBytes(other), launched: true})
	other2 := other
	other2.ID = deriveTaskID(s.id, "unit/v1", "started/v2-unit", "llm2")
	other2.Step = "llm2"
	s.agent.v2.tasks = append(s.agent.v2.tasks, &localTask{intent: other2, terminalBytes: taskTerminalBytes(other2), launched: true})
	s.publishTaskIntents()
	s.publishAgent()
	seq := 1
	for ; seq <= 15; seq++ {
		r := s.agent.v2.tasks[0].results[0]
		a.effects = agent.Effects{Audio: []agent.Audio{{Turn: 1, Source: agent.AudioRef{TaskID: id, Sequence: uint64(seq)}}}}
		if err := s.handleV2Event(agent.Event{ID: agent.EventID(fmt.Sprint("task/", seq)), Kind: agent.TaskMessage, Result: &r}); err != nil {
			t.Logf("result %d backpressured: %v", seq, err)
			break
		}
	}
	t.Logf("queued %d packets, window spans %d, live tasks %d", len(s.agent.v2.playback.queue), len(s.state.Agent.V2.Playback.Window), len(s.agent.v2.tasks))
	turn := agent.Turn(2)
	a.effects = agent.Effects{ClearAudioBefore: &turn, CancelBefore: &turn, StartTasks: []agent.TaskSpec{{Step: "stt", Kind: "stt/v1", Turn: 2, Class: agent.ReadOnly, Resume: agent.Retry, Request: []byte("x")}}}
	err := s.handleV2Event(agent.Event{ID: "audio/1/100", Kind: agent.Started})
	require.NoError(t, err, "clear/cancel/start must use the cleared window")
	require.Empty(t, s.agent.v2.playback.queue)
	t.Logf("barge-in callback: err=%v; queue after: %d", err, len(s.agent.v2.playback.queue))
}

func TestPlaybackReviewReferencesBelongToCurrentResult(t *testing.T) {
	s, a, r := audioUnitSession(t, 2)
	appendUnitAudio(t, s, a, r)
	a.effects = agent.Effects{Audio: []agent.Audio{{Turn: 1, Source: agent.AudioRef{TaskID: r.TaskID, Sequence: r.Sequence}}}}
	require.ErrorIs(t, s.handleV2Event(agent.Event{Kind: agent.Started}), agent.ErrIncompatibleAgent)
}
func TestPlaybackReviewIgnoresAudioBelowSameCallbackFloor(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			s, a, r := audioUnitSession(t, 2)
			floor := agent.Turn(2)
			a.effects = agent.Effects{Audio: []agent.Audio{{Turn: 1, Source: agent.AudioRef{TaskID: r.TaskID, Sequence: r.Sequence}}}}
			if cancel {
				a.effects.CancelBefore = &floor
			} else {
				a.effects.ClearAudioBefore = &floor
			}
			require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.TaskMessage, Result: &r}))
			require.Empty(t, s.agent.v2.playback.queue, "a clear/cancel cannot re-append the superseded result")
		})
	}
}
func TestPlaybackReviewCrashKeepsFinishedNotification(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.agent.latest.V2.Playback.Notify = []agent.Turn{7}
	require.NoError(t, s.restorePlayback(nil))
	require.Equal(t, []agent.Turn{7}, s.agent.v2.playback.notify)
	require.NoError(t, s.deliverClockEvent())
	require.Equal(t, agent.PlaybackFinished, a.lastEvent.Kind)
	require.EqualValues(t, 7, a.lastEvent.Playback.Turn)
}
func TestPlaybackReviewIdleSleepsUntilWake(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	delay, err := s.paceOne()
	require.NoError(t, err)
	require.GreaterOrEqual(t, delay, time.Hour)
	s.agent.paused = true
	delay, err = s.paceOne()
	require.NoError(t, err)
	require.GreaterOrEqual(t, delay, time.Hour)
}
func TestPlaybackReviewTimerIDsBoundedAndUniqueAcrossDeletion(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	for _, id := range []string{string([]byte{255}), string(make([]byte, 129))} {
		a.effects = agent.Effects{SetTimers: []agent.TimerSpec{{ID: id}}}
		require.ErrorIs(t, s.handleV2Event(agent.Event{Kind: agent.Started}), agent.ErrIncompatibleAgent)
	}
	a.effects = agent.Effects{SetTimers: []agent.TimerSpec{{ID: "reused"}}}
	require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started}))
	first := s.agent.latest.V2.Timers[0].Generation
	a.effects = agent.Effects{}
	require.NoError(t, s.deliverClockEvent())
	require.Empty(t, s.agent.latest.V2.Timers)
	a.effects = agent.Effects{SetTimers: []agent.TimerSpec{{ID: "other"}, {ID: "reused"}}}
	require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started}))
	require.Greater(t, s.agent.latest.V2.Timers[0].Generation, first)
	require.Greater(t, s.agent.latest.V2.Timers[1].Generation, s.agent.latest.V2.Timers[0].Generation)
}

func TestPlaybackReviewOutputClockDoesNotAccumulateWakeDelay(t *testing.T) {
	s, a, r := audioUnitSession(t, 80)
	appendUnitAudio(t, s, a, r)
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	s.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	sink := listenTestUDP(t)
	s.state.ICE.RemoteAddr = sink.LocalAddr().(*net.UDPAddr).AddrPort()
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	s.agent.v2.playback.nextAt = base
	s.agent.v2.playback.initialized = true
	for range 80 {
		s.storeClock = s.agent.v2.playback.nextAt.Add(2 * time.Millisecond)
		s.storeClockAt = time.Now()
		_, err := s.paceOne()
		require.NoError(t, err)
	}
	require.InDelta(t, float64(1600*time.Millisecond), float64(s.agent.v2.playback.nextAt.Sub(base)), float64(time.Millisecond), "small wake delay must not accumulate into speech drift")
}
func TestPlaybackReviewRestoreAdvancesRTPByStoreAge(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	s.agent.latest.V2.Playback = playbackState{Initialized: true, NextAt: base, NextTS: 1234}
	s.storeClock = base.Add(time.Second)
	s.storeClockAt = time.Now()
	require.NoError(t, s.restorePlayback(nil))
	require.InDelta(t, float64(49234), float64(s.agent.v2.playback.nextTS), 48)
}

func TestPlaybackReviewReferenceStorageReservedBeforeDelivery(t *testing.T) {
	s, a, r := audioUnitSession(t, 5)
	require.NoError(t, s.taskBudget(s.agent.v2.tasks, 0))
	reserved := s.moveReserved
	s.agent.v2.registration.Bounds.MaxMailboxBytes = int(reserved)
	appendUnitAudio(t, s, a, r)
	require.LessOrEqual(t, s.moveReserved, reserved, "delivery transfers the reference reservation; it cannot require new mailbox space")
}

func TestPlaybackReviewNonAudioMetadataOverflowIsTerminal(t *testing.T) {
	for _, withAudio := range []bool{false, true} {
		t.Run(fmt.Sprint("audio=", withAudio), func(t *testing.T) {
			s, a, r := audioUnitSession(t, 1)
			for i := range 40 {
				a.effects.SetTimers = append(a.effects.SetTimers, agent.TimerSpec{
					ID: fmt.Sprintf("timer/%02d/%s", i, strings.Repeat("x", 100)), After: time.Hour,
				})
			}
			if withAudio {
				a.effects.Audio = []agent.Audio{{Turn: r.Turn, Source: agent.AudioRef{TaskID: r.TaskID, Sequence: r.Sequence}}}
			}
			delivered, err := s.deliverTaskResult()
			require.ErrorIs(t, err, agent.ErrCapacity, "the actor must terminate on the first delivery, rather than retry unchanged non-audio overflow")
			require.False(t, delivered)
			require.EqualValues(t, 0, s.agent.v2.tasks[0].intent.Cursor)
			require.Empty(t, s.agent.v2.playback.queue)
			require.Empty(t, s.agent.latest.V2.Timers)
		})
	}
}
