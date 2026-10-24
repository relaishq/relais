package mediaworker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/sessionstore"

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
	s.publishTaskIntents(s.agent.latest.V2)
	s.publishAgent()
	return s, a, r
}

// Pacer, prune and restore fixture only; it never submits an event.
func installUnitPlayback(t *testing.T, s *session, r agent.TaskResult) {
	t.Helper()
	p := &s.agent.v2.playback
	for i, packet := range r.Chunk.Audio.Packets {
		d, err := opusDuration(packet)
		require.NoError(t, err)
		ref := agent.Audio{Turn: r.Turn, Source: agent.AudioRef{TaskID: r.TaskID, Sequence: r.Sequence, PacketOffset: uint32(i)}}
		p.queue = append(p.queue, ref)
		p.length += len(packet)
		p.duration += d
		p.referenceBytes += len(r.TaskID) + 32
	}
	task := s.agent.v2.tasks[0]
	task.results = nil
	task.audio = []agent.TaskResult{r}
	task.intent.Cursor = r.Sequence
	task.mailboxBytes -= taskAudioRefBytes(r)
	s.agent.latest.V2.Playback = p.snapshot()
	s.publishTaskIntents(s.agent.latest.V2)
	s.publishAgent()
	require.NoError(t, s.taskBudget(s.agent.v2.tasks, 0))
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
	s, _, r := audioUnitSession(t, 5)
	installUnitPlayback(t, s, r)
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	sink := listenTestUDP(t)
	s.state.ICE.RemoteAddr = sink.LocalAddr().(*net.UDPAddr).AddrPort()
	s.state.Audio = trackState{MID: "0", SSRC: 123, PayloadType: 111, InitialSeq: 65534, InitialTS: 77}
	s.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	receiver := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	now := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	// Freeze only paceOne's clock. UDP receive deadlines stay on real time
	// outside the bubble, and scheduler stalls cannot change the paced delay.
	paceAt := func(at time.Time) time.Duration {
		var delay time.Duration
		synctest.Test(t, func(t *testing.T) {
			s.storeClock, s.storeClockAt = at, time.Now()
			var err error
			delay, err = s.paceOne()
			require.NoError(t, err)
		})
		return delay
	}
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
	require.Equal(t, 20*time.Millisecond, paceAt(now))
	first := receive()
	require.EqualValues(t, 65534, first.SequenceNumber)
	require.Equal(t, 20*time.Millisecond, paceAt(now.Add(time.Second))) // long output stall
	second := receive()
	require.EqualValues(t, 65535, second.SequenceNumber)
	require.Equal(t, first.Timestamp+48000, second.Timestamp, "RTP clock includes the one-second stall")
	require.Equal(t, 20*time.Millisecond, paceAt(now.Add(time.Second)), "no catch-up after a stall")
	require.EqualValues(t, 2, s.state.Audio.Packets)
	require.Equal(t, 20*time.Millisecond, paceAt(now.Add(2*time.Second)))
	third := receive()
	require.Zero(t, third.SequenceNumber)
	require.EqualValues(t, 65536, s.state.Audio.HighestSentIndex)
	// Empty playback allocates no further SRTP indexes.
	s.agent.v2.playback.queue = nil
	require.Equal(t, time.Hour, paceAt(now.Add(2*time.Second)))
	require.EqualValues(t, 3, s.state.Audio.Packets)
	var rates CheckpointState
	s.checkpointRates(&rates)
	require.GreaterOrEqual(t, rates.RTPPacketRate, float64(400))
	_, _, _, err := (CheckpointEnvelope{}).CheckpointMargins(550*time.Millisecond, rates, 8192, 128)
	require.NoError(t, err)
}
func TestTimerGenerationResetCancelAndRestore(t *testing.T) {
	rig := newAcceptedEventRig(t, agenthost.Bounds{}, nil)
	effects := agent.Effects{}
	rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) { return effects, nil }
	capture := func() *v2Envelope {
		raw, err := rig.s.checkpoint.capture(checkpointLive, nil)
		require.NoError(t, err)
		saved, err := decodeSnapshot(raw)
		require.NoError(t, err)
		return saved.State.Agent.V2
	}
	submit := func() {
		_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started}, eventLive)
		require.NoError(t, err)
	}
	now := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	advance := func(at time.Time) {
		rig.s.mu.Lock()
		rig.s.storeClock, rig.s.storeClockAt = at, time.Now()
		rig.s.mu.Unlock()
	}
	advance(now)
	effects = agent.Effects{SetTimers: []agent.TimerSpec{{ID: "a/12:", After: time.Second}}}
	submit()
	first := capture()
	require.Len(t, first.Timers, 1)
	require.EqualValues(t, 1, first.Timers[0].Generation)
	effects = agent.Effects{SetTimers: []agent.TimerSpec{{ID: "a/12:", After: 2 * time.Second}}}
	submit()
	reset := capture()
	require.Len(t, reset.Timers, 1)
	require.EqualValues(t, 2, reset.Timers[0].Generation)
	require.True(t, reset.Timers[0].At.After(first.Timers[0].At), "reset must replace the original deadline")
	advance(first.Timers[0].At.Add(time.Second / 2))
	effects = agent.Effects{}
	count := len(rig.app.events)
	require.NoError(t, rig.s.deliverClockEvent())
	require.Len(t, rig.app.events, count, "reset timer must not fire at its original deadline")
	advance(now.Add(3 * time.Second))
	require.NoError(t, rig.s.deliverClockEvent())
	require.Len(t, rig.app.events, count+1)
	fired := rig.app.events[count]
	require.Equal(t, agent.TimerFired, fired.Kind)
	require.Equal(t, "a/12:", fired.Timer.ID)
	require.EqualValues(t, 2, fired.Timer.Generation)
	require.Empty(t, capture().Timers)
	require.NoError(t, rig.s.deliverClockEvent())
	require.Len(t, rig.app.events, count+1, "a timer generation must fire exactly once")
	effects = agent.Effects{SetTimers: []agent.TimerSpec{{ID: "a/12:", After: time.Second}}, CancelTimers: []string{"a/12:"}}
	submit()
	armed := capture()
	require.Len(t, armed.Timers, 1, "cancel and set in one event must re-arm")
	require.EqualValues(t, 3, armed.Timers[0].Generation)
	effects = agent.Effects{CancelTimers: []string{"a/12:"}}
	submit()
	require.Empty(t, capture().Timers)
	effects = agent.Effects{SetTimers: []agent.TimerSpec{{ID: "negative", After: -1}}}
	_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started}, eventLive)
	require.ErrorIs(t, err, agent.ErrIncompatibleAgent)
	t.Run("event-identity", func(t *testing.T) {
		rig := newAcceptedEventRig(t, agenthost.Bounds{}, nil)
		effects := agent.Effects{}
		rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) { return effects, nil }
		var ids []agent.EventID
		for generation := 1; generation <= 12; generation++ {
			id := "a"
			if generation == 2 {
				id = "a1"
			}
			effects = agent.Effects{CancelTimers: []string{"a", "a1"}, SetTimers: []agent.TimerSpec{{ID: id}}}
			_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started}, eventLive)
			require.NoError(t, err)
			if generation == 2 || generation == 12 {
				effects = agent.Effects{}
				require.NoError(t, rig.s.deliverClockEvent())
				event := rig.app.events[len(rig.app.events)-1]
				require.Equal(t, agent.TimerFired, event.Kind)
				require.Equal(t, id, event.Timer.ID)
				require.EqualValues(t, generation, event.Timer.Generation)
				ids = append(ids, event.ID)
			}
		}
		require.NotEqual(t, ids[0], ids[1], "timer ID and generation must have distinct event identities")
	})
}

func TestPlaybackExactAndCrashRestore(t *testing.T) {
	for _, exact := range []bool{true, false} {
		t.Run(fmt.Sprint(exact), func(t *testing.T) {
			s, _, r := audioUnitSession(t, 40)
			installUnitPlayback(t, s, r)
			s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"tts": func(context.Context, *agent.TaskRun) error { return nil }}
			task := s.agent.v2.tasks[0]
			task.intent.Kind = "tts"
			task.intent.Final = true
			task.intent.Phase = taskTerminal
			task.intent.Terminal = agent.OK
			s.publishTaskIntents(s.agent.latest.V2)
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
	s, _, r := audioUnitSession(t, 4)
	installUnitPlayback(t, s, r)
	require.NoError(t, s.checkpoint.restore(checkpointRestore{pair: &s.state.Agent}))
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
func TestAudioMailboxBackpressureUntilPlaybackAckDrain(t *testing.T) {
	s, _, r := audioUnitSession(t, 4)
	installUnitPlayback(t, s, r)
	s.agent.v2.registration.Bounds.MaxMailboxBytes = int(s.moveReserved)
	e := taskEmission{id: r.TaskID, attempt: 0, ctx: context.Background(), sequence: 2, chunk: r.Chunk, reply: make(chan error, 1)}
	s.acceptEmission(e)
	require.ErrorIs(t, <-e.reply, agent.ErrBackpressure)
	require.EqualValues(t, 1, s.agent.v2.tasks[0].intent.Published)
	s.agent.v2.playback.queue = nil
	s.agent.v2.playback.length = 0
	require.NoError(t, s.checkpoint.restore(checkpointRestore{pair: &s.state.Agent}))
	s.pruneTasks()
	s.acceptEmission(e)
	require.NoError(t, <-e.reply)
	require.EqualValues(t, 2, s.agent.v2.tasks[0].intent.Published)
}
func TestPlaybackEncryptionFailureKeepsCursor(t *testing.T) {
	s, _, r := audioUnitSession(t, 2)
	installUnitPlayback(t, s, r)
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

func TestPlaybackVariablePacketDurations(t *testing.T) {
	s, _, r := audioUnitSession(t, 3)
	r.Chunk.Audio.Packets = [][]byte{{0x80}, {0x18}, {0x1b, 2}} // 2.5, 60 and 120 ms
	s.agent.v2.playback.payloads = nil
	require.NoError(t, s.registerAudioLocked(r))
	s.agent.v2.tasks[0].results[0] = r
	s.agent.v2.tasks[0].mailboxBytes = taskResultBytes(r)
	installUnitPlayback(t, s, r)
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
		// Freeze only paceOne's clock, as in TestPlaybackPacerRebasesAndKeepsCounters:
		// scheduler stalls under -race cannot change the computed delay.
		var delay time.Duration
		synctest.Test(t, func(t *testing.T) {
			s.storeClockAt = time.Now()
			var err error
			delay, err = s.paceOne()
			require.NoError(t, err)
		})
		require.Equal(t, d, delay)
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

func TestPlaybackWindowDurationAndBytes(t *testing.T) {
	for _, count := range []int{15, 16} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			packets := make([][]byte, count)
			for i := range packets {
				packets[i] = []byte{0xf8, 0xff, 0xfe}
			}
			rig, _ := startAcceptedSpeech(t, agenthost.Bounds{}, packets)
			delivered, err := rig.s.deliverTaskResult()
			require.NoError(t, err)
			require.True(t, delivered)
			raw, err := rig.s.checkpoint.capture(checkpointLive, nil)
			require.NoError(t, err)
			saved, err := decodeSnapshot(raw)
			require.NoError(t, err)
			pb := saved.State.Agent.V2.Playback
			require.Equal(t, count, pb.Pending)
			require.Len(t, pb.Window, 1)
			require.EqualValues(t, 15, pb.Window[0].Count)
			metadata, err := json.Marshal(saved.State.Agent.V2)
			require.NoError(t, err)
			require.NotContains(t, string(metadata), "Packets")
			move, err := rig.s.saveV2MoveOnly(context.Background())
			require.NoError(t, err)
			require.Len(t, move.audio, count, "exact export retains the entire queue")
			require.Len(t, move.tasks, 1)
			require.Len(t, move.tasks[0].Audio, 1, "result consumption must retain packet payload")
			require.Equal(t, packets, move.tasks[0].Audio[0].Chunk.Audio.Packets)
		})
	}
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprint("bytes", extra), func(t *testing.T) {
			packets := make([][]byte, 4+extra)
			for i := range packets {
				packets[i] = append([]byte{0xf8}, make([]byte, 1023)...)
			}
			rig, _ := startAcceptedSpeech(t, agenthost.Bounds{}, packets)
			delivered, err := rig.s.deliverTaskResult()
			require.NoError(t, err)
			require.True(t, delivered)
			raw, err := rig.s.checkpoint.capture(checkpointLive, nil)
			require.NoError(t, err)
			saved, err := decodeSnapshot(raw)
			require.NoError(t, err)
			pb := saved.State.Agent.V2.Playback
			require.Len(t, pb.Window, 1)
			require.EqualValues(t, 4, pb.Window[0].Count)
			require.Equal(t, (4+extra)*1024, pb.Length)
		})
	}
}

func TestAudioResultKeepsUnfinishedReadOnlyRequest(t *testing.T) {
	started := make(chan *agent.TaskRun, 1)
	request := []byte("reproducible request")
	rig := newAcceptedEventRig(t, agenthost.Bounds{}, map[string]agent.TaskHandler{"tts/v1": func(ctx context.Context, run *agent.TaskRun) error {
		started <- run
		<-ctx.Done()
		return ctx.Err()
	}})
	rig.app.handle = func(_ context.Context, e agent.Event) (agent.Effects, error) {
		if e.Kind == agent.Started {
			return agent.Effects{StartTasks: []agent.TaskSpec{{Step: "tts", Kind: "tts/v1", Class: agent.ReadOnly, Resume: agent.Retry, Turn: 1, Request: request}}}, nil
		}
		effects := agent.Effects{}
		for i := range e.Result.Chunk.Audio.Packets {
			effects.Audio = append(effects.Audio, agent.Audio{Turn: e.Result.Turn, Source: agent.AudioRef{TaskID: e.Result.TaskID, Sequence: e.Result.Sequence, PacketOffset: uint32(i)}})
		}
		return effects, nil
	}
	_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: "tts"}, eventLive)
	require.NoError(t, err)
	rig.s.launchTasks()
	t.Cleanup(rig.s.stopTaskAttempts)
	run := <-started
	require.Equal(t, request, run.Spec().Request)
	packets := make([][]byte, 20)
	for i := range packets {
		packets[i] = []byte{0xf8, 0xff, 0xfe}
	}
	done := make(chan error, 1)
	go func() {
		done <- run.Emit(context.Background(), 1, agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: packets}})
	}()
	rig.s.acceptEmission(<-rig.s.agent.v2.emissions)
	require.NoError(t, <-done)
	delivered, err := rig.s.deliverTaskResult()
	require.NoError(t, err)
	require.True(t, delivered)
	move, err := rig.s.saveV2MoveOnly(context.Background())
	require.NoError(t, err)
	require.Len(t, move.audio, 20)
	require.Len(t, move.tasks, 1)
	require.Equal(t, run.ID(), move.tasks[0].ID)
	require.Equal(t, request, move.tasks[0].Request, "consuming an audio chunk cannot lose the request before an exact move")
	flush := agentFlush{done: make(chan error, 1), deadline: time.Now().Add(15 * time.Second), moveOnly: make(chan agentMoveExport, 1)}
	require.True(t, rig.s.finishAgentFlush(flush))
	require.NoError(t, <-flush.done, "exact export must validate the retained request against its accepted intent")
	require.Equal(t, request, (<-flush.moveOnly).tasks[0].Request)
}
