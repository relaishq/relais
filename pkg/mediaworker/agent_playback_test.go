package mediaworker

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/sessionstore"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func audioUnitSession(t *testing.T, count int) (*session, *v2UnitAgent, agent.TaskResult) {
	t.Helper()
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	packets := make([][]byte, count)
	for i := range packets {
		packets[i] = []byte{0xf8, 0xff, 0xfe}
	} // RFC Opus silence, 20 ms
	r := agent.TaskResult{TaskID: "audio", Sequence: 1, Turn: 1, Outcome: agent.OK, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: packets}}}
	intent := taskIntent{ID: r.TaskID, Turn: 1, Class: agent.ReadOnly, Phase: taskStarted, Published: 1}
	s.agent.v2.tasks = []*localTask{{intent: intent, results: []agent.TaskResult{r}, mailboxBytes: taskResultBytes(r), terminalBytes: taskTerminalBytes(intent)}}
	require.NoError(t, s.registerAudioLocked(r))
	s.publishTaskIntents()
	s.publishAgent()
	return s, a, r
}
func appendUnitAudio(t *testing.T, s *session, a *v2UnitAgent, r agent.TaskResult) {
	t.Helper()
	a.effects = agent.Effects{}
	for i := range r.Chunk.Audio.Packets {
		a.effects.Audio = append(a.effects.Audio, agent.Audio{Turn: r.Turn, Source: agent.AudioRef{TaskID: r.TaskID, Sequence: r.Sequence, PacketOffset: uint32(i)}})
	}
	require.NoError(t, s.handleV2Event(agent.Event{ID: "result", Kind: agent.TaskMessage, Result: &r}))
	a.effects = agent.Effects{}
}
func TestPlaybackWindowDurationAndBytes(t *testing.T) {
	for _, count := range []int{15, 16} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, a, r := audioUnitSession(t, count)
			appendUnitAudio(t, s, a, r)
			pb := s.state.Agent.V2.Playback
			require.Equal(t, count, pb.Pending)
			require.Len(t, pb.Window, 1)
			require.EqualValues(t, 15, pb.Window[0].Count)
			require.EqualValues(t, count, len(s.agent.v2.playback.queue))
			raw, err := json.Marshal(s.state.Agent.V2)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "Packets")
			require.Len(t, s.agent.v2.tasks[0].audio, 1, "result consumption must retain packet payload")
		})
	}
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprint("bytes", extra), func(t *testing.T) {
			s, a, r := audioUnitSession(t, 4+extra)
			for i := range r.Chunk.Audio.Packets {
				r.Chunk.Audio.Packets[i] = append([]byte{0xf8}, make([]byte, 1023)...)
			}
			s.agent.v2.playback.payloads = nil
			require.NoError(t, s.registerAudioLocked(r))
			s.agent.v2.tasks[0].results[0] = r
			s.agent.v2.tasks[0].mailboxBytes = taskResultBytes(r)
			appendUnitAudio(t, s, a, r)
			require.EqualValues(t, 4, s.state.Agent.V2.Playback.Window[0].Count)
			require.Equal(t, (4+extra)*1024, s.state.Agent.V2.Playback.Length)
		})
	}
}
func TestOpusPacketDurationAndFraming(t *testing.T) {
	for _, tc := range []struct {
		p []byte
		d time.Duration
	}{{[]byte{0x80}, 2500 * time.Microsecond}, {[]byte{0x88}, 5 * time.Millisecond}, {[]byte{0x90}, 10 * time.Millisecond}, {[]byte{0x98}, 20 * time.Millisecond}, {[]byte{0x10}, 40 * time.Millisecond}, {[]byte{0x18}, 60 * time.Millisecond}, {[]byte{0x1b, 2}, 120 * time.Millisecond}} {
		d, err := opusDuration(tc.p)
		require.NoError(t, err)
		require.Equal(t, tc.d, d)
	}
	for _, p := range [][]byte{nil, {0xfb}, {0xfb, 0}, {0x1b, 3}, {0xf9, 1}, {0xfa, 252}, {0xfb, 0x42, 255}, {0xfb, 0x82, 5, 1}, make([]byte, opusPacketMTU+1)} {
		_, err := opusDuration(p)
		require.ErrorIs(t, err, agent.ErrIncompatibleAgent)
	}
	_, err := validateOpus(&agent.EncodedAudio{Format: "audio/pcm"})
	require.ErrorIs(t, err, agent.ErrIncompatibleAgent)
}
func TestPlaybackPacerRebasesAndKeepsCounters(t *testing.T) {
	s, a, r := audioUnitSession(t, 5)
	appendUnitAudio(t, s, a, r)
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	sink := listenTestUDP(t)
	s.state.ICE.RemoteAddr = sink.LocalAddr().(*net.UDPAddr).AddrPort()
	s.state.Audio = trackState{MID: "0", SSRC: 123, PayloadType: 111, InitialSeq: 65534, InitialTS: 77}
	s.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	receiver := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	now := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	s.storeClock = now
	s.storeClockAt = time.Now()
	receive := func() rtp.Packet {
		buf := make([]byte, receiveMTU)
		require.NoError(t, sink.SetReadDeadline(time.Now().Add(time.Second)))
		n, _, err := sink.ReadFromUDP(buf)
		require.NoError(t, err)
		plain, err := receiver.DecryptRTP(nil, buf[:n], nil)
		require.NoError(t, err)
		var p rtp.Packet
		require.NoError(t, p.Unmarshal(plain))
		return p
	}
	delay, err := s.paceOne()
	require.NoError(t, err)
	require.InDelta(t, float64(20*time.Millisecond), float64(delay), float64(2*time.Millisecond))
	first := receive()
	require.EqualValues(t, 65534, first.SequenceNumber)
	s.storeClock = now.Add(time.Second)
	s.storeClockAt = time.Now() // long output stall
	delay, err = s.paceOne()
	require.NoError(t, err)
	require.InDelta(t, float64(20*time.Millisecond), float64(delay), float64(2*time.Millisecond))
	second := receive()
	require.EqualValues(t, 65535, second.SequenceNumber)
	require.InDelta(t, float64(first.Timestamp+48000), float64(second.Timestamp), 48, "RTP clock includes the one-second stall")
	delay, err = s.paceOne()
	require.NoError(t, err)
	require.Greater(t, delay, 15*time.Millisecond, "no catch-up after a stall")
	require.EqualValues(t, 2, s.state.Audio.Packets)
	s.storeClock = now.Add(2 * time.Second)
	s.storeClockAt = time.Now()
	_, _ = s.paceOne()
	third := receive()
	require.Zero(t, third.SequenceNumber)
	require.EqualValues(t, 65536, s.state.Audio.HighestSentIndex)
	// Empty playback allocates no further SRTP indexes.
	s.agent.v2.playback.queue = nil
	_, _ = s.paceOne()
	require.EqualValues(t, 3, s.state.Audio.Packets)
	var rates CheckpointState
	s.checkpointRates(&rates)
	require.GreaterOrEqual(t, rates.RTPPacketRate, float64(400))
	_, _, _, err = (CheckpointEnvelope{}).CheckpointMargins(550*time.Millisecond, rates, 8192, 128)
	require.NoError(t, err)
}
func TestTimerGenerationResetCancelAndRestore(t *testing.T) {
	s, a, _ := audioUnitSession(t, 0)
	now := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	s.storeClock = now
	s.storeClockAt = time.Now()
	a.effects = agent.Effects{SetTimers: []agent.TimerSpec{{ID: "a/12:", After: time.Second}}}
	require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started}))
	require.EqualValues(t, 1, s.agent.latest.V2.Timers[0].Generation)
	a.effects = agent.Effects{SetTimers: []agent.TimerSpec{{ID: "a/12:", After: 2 * time.Second}}}
	require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started}))
	require.EqualValues(t, 2, s.agent.latest.V2.Timers[0].Generation)
	s.storeClock = now.Add(time.Second)
	s.storeClockAt = time.Now()
	a.effects = agent.Effects{}
	handles := a.handles
	require.NoError(t, s.deliverClockEvent())
	require.Equal(t, handles, a.handles)
	s.storeClock = now.Add(3 * time.Second)
	s.storeClockAt = time.Now()
	require.NoError(t, s.deliverClockEvent())
	require.Equal(t, agent.TimerFired, a.lastEvent.Kind)
	require.EqualValues(t, 2, a.lastEvent.Timer.Generation)
	handles = a.handles
	require.NoError(t, s.deliverClockEvent())
	require.Equal(t, handles, a.handles)
	a.effects = agent.Effects{SetTimers: []agent.TimerSpec{{ID: "a/12:", After: time.Second}}, CancelTimers: []string{"a/12:"}}
	require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started}))
	require.EqualValues(t, 3, s.agent.latest.V2.Timers[0].Generation)
	a.effects = agent.Effects{CancelTimers: []string{"a/12:"}}
	require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started}))
	require.Empty(t, s.agent.latest.V2.Timers)
	a.effects = agent.Effects{SetTimers: []agent.TimerSpec{{ID: "negative", After: -1}}}
	require.ErrorIs(t, s.handleV2Event(agent.Event{Kind: agent.Started}), agent.ErrIncompatibleAgent)
	require.NotEqual(t, clockEventID("timer", "a", 12), clockEventID("timer", "a1", 2))
}
func TestPlaybackExactAndCrashRestore(t *testing.T) {
	for _, exact := range []bool{true, false} {
		t.Run(fmt.Sprint(exact), func(t *testing.T) {
			s, a, r := audioUnitSession(t, 40)
			appendUnitAudio(t, s, a, r)
			s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"tts": func(context.Context, *agent.TaskRun) error { return nil }}
			task := s.agent.v2.tasks[0]
			task.intent.Kind = "tts"
			task.intent.Final = true
			task.intent.Phase = taskTerminal
			task.intent.Terminal = agent.OK
			s.publishTaskIntents()
			s.publishAgent()
			b := &v2UnitAgent{state: agent.State{Version: 1}}
			_, target := v2UnitSession(t, b)
			require.NoError(t, target.initAgent(false, ResumeOptions{}))
			target.agent.latest = cloneAgentState(s.state.Agent)
			target.agent.v2.registration = s.agent.v2.registration
			var move *movePayload
			if exact {
				move = &movePayload{Tasks: s.exportTasks(), Audio: slices.Clone(s.agent.v2.playback.queue)}
			}
			require.NoError(t, target.restoreTasks(move))
			require.NoError(t, target.restorePlayback(move))
			if exact {
				require.Equal(t, s.agent.v2.playback.queue, target.agent.v2.playback.queue)
				require.Len(t, target.agent.v2.tasks[0].audio, 1)
			} else {
				require.Empty(t, target.agent.v2.playback.queue)
				require.Empty(t, target.agent.v2.playback.payloads)
			}
		})
	}
}

func TestAudioStreamingReleasesPayloadAfterAckAndDrain(t *testing.T) {
	s, a, r := audioUnitSession(t, 4)
	appendUnitAudio(t, s, a, r)
	s.durableAgent = cloneAgentState(s.state.Agent)
	before := s.moveReserved
	s.pruneTasks()
	require.Len(t, s.agent.v2.tasks[0].audio, 1, "live references retain audio")
	s.agent.v2.playback.queue = nil
	s.agent.v2.playback.length = 0
	s.pruneTasks()
	require.Empty(t, s.agent.v2.tasks[0].audio, "an unfinished producer must regain space")
	require.Empty(t, s.agent.v2.playback.payloads)
	require.Less(t, s.moveReserved, before)
	require.False(t, s.agent.v2.tasks[0].intent.Final)
}
func TestTurnClearCancelAndLateResults(t *testing.T) {
	s, a, r := audioUnitSession(t, 8)
	appendUnitAudio(t, s, a, r)
	task := s.agent.v2.tasks[0]
	task.attempt = 4
	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task.cancel = cancel
	turn := agent.Turn(2)
	a.effects = agent.Effects{ClearAudioBefore: &turn, CancelBefore: &turn}
	require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started}))
	require.Empty(t, s.agent.v2.playback.queue)
	require.Equal(t, agent.Turn(2), s.state.Agent.V2.CancelFloor)
	require.ErrorIs(t, cancelCtx.Err(), context.Canceled)
	e := taskEmission{id: task.intent.ID, attempt: 4, ctx: context.Background(), sequence: 2, chunk: agent.Chunk{Data: []byte("late")}, reply: make(chan error, 1)}
	s.acceptEmission(e)
	require.ErrorIs(t, <-e.reply, context.Canceled)
	require.Len(t, task.results, 1)
	require.Equal(t, agent.Canceled, task.results[0].Outcome)
	// A lower requested floor cannot reopen the old turn.
	lower := agent.Turn(1)
	a.effects = agent.Effects{CancelBefore: &lower}
	require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started}))
	require.EqualValues(t, 2, s.agent.latest.V2.CancelFloor)
}
func TestCapacityFIFOAndNoAdmittedTaskEviction(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"x/v1": func(context.Context, *agent.TaskRun) error { return nil }}
	for i := range 20 {
		a.effects.StartTasks = append(a.effects.StartTasks, agent.TaskSpec{Step: fmt.Sprint(i), Kind: "x/v1", Class: agent.ReadOnly})
	}
	require.NoError(t, s.handleV2Event(agent.Event{ID: "fanout", Kind: agent.Started}))
	require.NotEmpty(t, s.agent.v2.tasks)
	for i, task := range s.agent.v2.tasks {
		require.Equal(t, deriveTaskID(s.id, "unit/v1", "fanout", fmt.Sprint(i)), task.intent.ID, "first admitted tasks stay admitted")
	}
	for _, task := range s.agent.v2.tasks {
		require.Less(t, task.intent.Step, "9")
	}
	t.Logf("width 20 admits %d and refuses %d", len(s.agent.v2.tasks), len(s.agent.latest.V2.Capacity))
	// A fan-out that needs refusal space must keep older outcomes first.
	b := &v2UnitAgent{state: agent.State{Version: 1}}
	_, fifo := v2UnitSession(t, b)
	require.NoError(t, fifo.initAgent(false, ResumeOptions{}))
	fifo.agent.v2.registration.Tasks = s.agent.v2.registration.Tasks
	fifo.agent.latest.V2.Capacity = []taskRefusal{{ID: "older", Turn: 1}}
	b.effects = a.effects
	require.NoError(t, fifo.handleV2Event(agent.Event{ID: "new", Kind: agent.Started}))
	require.Equal(t, agent.TaskID("older"), fifo.agent.latest.V2.Capacity[0].ID)

}

func TestTaskRefusalFitCannotEvictAdmittedTasks(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"x/v1": func(context.Context, *agent.TaskRun) error { return nil }}
	var tasks []*localTask
	for i := range 7 {
		spec := agent.TaskSpec{Step: fmt.Sprint(i), Kind: "x/v1", Class: agent.ReadOnly}
		var err error
		tasks, _, err = s.prepareTaskEffects(agent.Event{ID: "cause", Kind: agent.Started}, agent.Effects{StartTasks: []agent.TaskSpec{spec}}, tasks, nil)
		require.NoError(t, err)
	}
	require.Len(t, tasks, 6)
	var refusals []taskRefusal
	for i := 6; i < 20; i++ {
		refusals = append(refusals, taskRefusal{ID: deriveTaskID(s.id, "unit/v1", "cause", fmt.Sprint(i))})
	}
	_, _, err := s.fitTaskRefusals(nil, tasks, nil, refusals)
	require.ErrorIs(t, err, errTaskAdmissionBackpressure, "fitting refusals cannot evict admitted tasks")
}

func TestAudioMailboxBackpressureUntilPlaybackAckDrain(t *testing.T) {
	s, a, r := audioUnitSession(t, 4)
	appendUnitAudio(t, s, a, r)
	s.agent.v2.registration.Bounds.MaxMailboxBytes = int(s.moveReserved)
	e := taskEmission{id: r.TaskID, attempt: 0, ctx: context.Background(), sequence: 2, chunk: r.Chunk, reply: make(chan error, 1)}
	s.acceptEmission(e)
	require.ErrorIs(t, <-e.reply, agent.ErrBackpressure)
	require.EqualValues(t, 1, s.agent.v2.tasks[0].intent.Published)
	s.agent.v2.playback.queue = nil
	s.agent.v2.playback.length = 0
	s.durableAgent = cloneAgentState(s.state.Agent)
	s.pruneTasks()
	s.acceptEmission(e)
	require.NoError(t, <-e.reply)
	require.EqualValues(t, 2, s.agent.v2.tasks[0].intent.Published)
}
func TestPlaybackEncryptionFailureKeepsCursor(t *testing.T) {
	s, a, r := audioUnitSession(t, 2)
	appendUnitAudio(t, s, a, r)
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	s.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	s.plainBuf = make([]byte, 1) // force the production marshal failure boundary
	_, err := s.paceOne()
	require.Error(t, err)
	require.Len(t, s.agent.v2.playback.queue, 2)
	require.Zero(t, s.state.Audio.Packets)
	require.True(t, s.agent.v2.playback.nextAt.IsZero())
}

func TestAudioResultKeepsUnfinishedReadOnlyRequest(t *testing.T) {
	s, a, r := audioUnitSession(t, 20)
	spec := agent.TaskSpec{Step: "tts", Kind: "tts", Class: agent.ReadOnly, Resume: agent.Retry, Turn: 1, Request: []byte("reproducible request")}
	task := s.agent.v2.tasks[0]
	task.request = slices.Clone(spec.Request)
	task.intent.Step, task.intent.Kind, task.intent.Resume = spec.Step, spec.Kind, spec.Resume
	task.intent.RequestLength = len(spec.Request)
	task.intent.Digest = taskDigest(spec)
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"tts": func(context.Context, *agent.TaskRun) error { return nil }}
	appendUnitAudio(t, s, a, r)
	require.Equal(t, spec.Request, s.agent.v2.tasks[0].request, "consuming an audio chunk cannot lose the request before an exact move")
	require.NoError(t, s.validateTaskMoves(s.agent.latest.V2.Tasks, &movePayload{Tasks: s.taskMoveViews()}))
}

func TestPlaybackVariablePacketDurations(t *testing.T) {
	s, a, r := audioUnitSession(t, 3)
	r.Chunk.Audio.Packets = [][]byte{{0x80}, {0x18}, {0x1b, 2}} // 2.5, 60 and 120 ms
	s.agent.v2.playback.payloads = nil
	require.NoError(t, s.registerAudioLocked(r))
	s.agent.v2.tasks[0].results[0] = r
	s.agent.v2.tasks[0].mailboxBytes = taskResultBytes(r)
	appendUnitAudio(t, s, a, r)
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	s.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	sink := listenTestUDP(t)
	s.state.ICE.RemoteAddr = sink.LocalAddr().(*net.UDPAddr).AddrPort()
	s.state.Audio = trackState{MID: "0", SSRC: 123, PayloadType: 111, InitialSeq: 100, InitialTS: 1}
	expectedTS := uint32(1)
	for i, d := range []time.Duration{2500 * time.Microsecond, 60 * time.Millisecond, 120 * time.Millisecond} {
		if i == 0 {
			s.storeClock = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		} else {
			s.storeClock = s.agent.v2.playback.nextAt
		}
		s.storeClockAt = time.Now()
		delay, err := s.paceOne()
		require.NoError(t, err)
		require.InDelta(t, float64(d), float64(delay), float64(2*time.Millisecond))
		require.Equal(t, expectedTS, s.state.Audio.LastTimestamp)
		expectedTS += uint32(int64(d) * 48000 / int64(time.Second))
		require.Equal(t, expectedTS, s.agent.v2.playback.nextTS)
	}
}

func TestPlaybackRestoreClockRefusalStaysTypedOverHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { workerAPIError(w, sessionstore.ErrUnsafeCheckpointClock) }))
	defer server.Close()
	err := privateapi.Do(context.Background(), server.Client(), server.URL, http.MethodPost, "/resume", nil, nil, RemoteErrors)
	require.ErrorIs(t, err, sessionstore.ErrUnsafeCheckpointClock)
	require.NotErrorIs(t, err, privateapi.ErrUncertain)
}
