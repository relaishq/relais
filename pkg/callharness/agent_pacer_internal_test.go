package callharness

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type pacerHarnessAgent struct {
	events chan agent.Event
	barge  bool
	state  struct{ Inputs int }
}

func (a *pacerHarnessAgent) Handle(_ context.Context, e agent.Event) (agent.Effects, error) {
	// Observe metadata only. The app never stores or exports encoded audio.
	observed := e
	if e.Result != nil {
		r := *e.Result
		r.Chunk = agent.Chunk{}
		observed.Result = &r
	}
	a.events <- observed
	switch e.Kind {
	case agent.Started:
		return agent.Effects{StartTasks: []agent.TaskSpec{{Step: "tts", Kind: "tts/v1", Turn: 1, Class: agent.ReadOnly, Resume: agent.Retry, Request: []byte("synthetic TTS request")}}, SetTimers: []agent.TimerSpec{{ID: "once", After: 600 * time.Millisecond}}}, nil
	case agent.TaskMessage:
		if e.Result.Chunk.Audio != nil {
			turn := e.Result.Turn
			effects := agent.Effects{NotifyPlaybackFinished: &turn}
			for i := range e.Result.Chunk.Audio.Packets {
				effects.Audio = append(effects.Audio, agent.Audio{Turn: turn, Source: agent.AudioRef{TaskID: e.Result.TaskID, Sequence: e.Result.Sequence, PacketOffset: uint32(i)}})
			}
			return effects, nil
		}
	case agent.AudioInput:
		a.state.Inputs++
		if a.barge && a.state.Inputs == 1 {
			turn := agent.Turn(2)
			return agent.Effects{ClearAudioBefore: &turn, CancelBefore: &turn}, nil
		}
	}
	return agent.Effects{}, nil
}
func (a *pacerHarnessAgent) Save(context.Context) (agent.State, error) {
	b, err := json.Marshal(a.state)
	return agent.State{Version: 1, Bytes: b}, err
}
func (a *pacerHarnessAgent) Restore(_ context.Context, s agent.State) error {
	return json.Unmarshal(s.Bytes, &a.state)
}
func pacerFixtures(t *testing.T, n int) [][]byte {
	t.Helper()
	b, err := os.ReadFile("testdata/tts-opus-20ms.packets")
	require.NoError(t, err)
	var packets [][]byte
	for len(b) > 0 && len(packets) < n {
		require.GreaterOrEqual(t, len(b), 4)
		size := int(binary.BigEndian.Uint32(b))
		b = b[4:]
		require.GreaterOrEqual(t, len(b), size)
		packets = append(packets, slices.Clone(b[:size]))
		b = b[size:]
	}
	require.Len(t, packets, n)
	return packets
}
func pacerRegistrations(t *testing.T, n int, barge bool, holdFinal bool) ([]agenthost.Registration, []chan agent.Event, chan struct{}, *atomic.Int64) {
	t.Helper()
	packets := pacerFixtures(t, n)
	gate := make(chan struct{})
	late := new(atomic.Int64)
	events := []chan agent.Event{make(chan agent.Event, 256), make(chan agent.Event, 256)}
	regs := make([]agenthost.Registration, 2)
	for i := range regs {
		regs[i] = agenthost.Registration{Name: "pacer/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &pacerHarnessAgent{events: events[i], barge: barge} }, Tasks: map[string]agent.TaskHandler{"tts/v1": func(ctx context.Context, run *agent.TaskRun) error {
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
			if run.After() == 0 {
				if err := run.Emit(ctx, 1, agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: packets}}); err != nil {
					return err
				}
			}
			if holdFinal {
				<-ctx.Done()
				late.Add(1)
				_ = run.Emit(context.Background(), 2, agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: packets[:1]}})
				return ctx.Err()
			}
			return nil
		}}}
	}
	return regs, events, gate, late
}

type pacedPacket struct {
	packet *rtp.Packet
	at     time.Time
}

func receivePacer(call *Call) chan pacedPacket {
	received := make(chan pacedPacket, 256)
	call.pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		call.startReader(func() {
			for {
				p, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				received <- pacedPacket{p, time.Now()}
			}
		})
	})
	return received
}
func nextPaced(t *testing.T, ch <-chan pacedPacket) pacedPacket {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(4 * time.Second):
		t.Fatal("missing paced packet")
		return pacedPacket{}
	}
}
func awaitPacerEvent(t *testing.T, ch <-chan agent.Event, kind agent.EventKind) agent.Event {
	t.Helper()
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case e := <-ch:
			if e.Kind == kind {
				return e
			}
		case <-deadline.C:
			t.Fatalf("missing %s", kind)
			return agent.Event{}
		}
	}
}
func assertPacerSequence(t *testing.T, all []pacedPacket, packets [][]byte, clockGaps ...bool) {
	t.Helper()
	require.Len(t, all, len(packets))
	for i, p := range all {
		require.Equal(t, packets[i], p.packet.Payload, "packet %d", i)
		if i > 0 {
			require.Equal(t, all[i-1].packet.SequenceNumber+1, p.packet.SequenceNumber, "sequence %d", i)
			if len(clockGaps) > 0 && clockGaps[0] {
				actual := time.Duration(uint32(p.packet.Timestamp-all[0].packet.Timestamp)) * time.Second / 48000
				require.InDelta(t, float64(p.at.Sub(all[0].at)), float64(actual), float64(50*time.Millisecond), "timestamp %d follows output time", i)
			} else {
				require.Equal(t, all[i-1].packet.Timestamp+960, p.packet.Timestamp, "timestamp %d", i)
			}
		}
	}
}
func TestV2PacerSilentCallerAndFinished(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		regs, events, gate, _ := pacerRegistrations(t, 20, false, false)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, remoteTarget: true})
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		received := receivePacer(call)
		close(gate)
		var all []pacedPacket
		for range 20 {
			all = append(all, nextPaced(t, received))
		}
		assertPacerSequence(t, all, pacerFixtures(t, 20))
		require.InDelta(t, float64(380*time.Millisecond), float64(all[19].at.Sub(all[0].at)), float64(380*time.Millisecond)/50+float64(5*time.Millisecond), "steady output clock during caller silence")
		finished := awaitPacerEvent(t, events[0], agent.PlaybackFinished)
		require.EqualValues(t, 1, finished.Playback.Turn)
		timer := awaitPacerEvent(t, events[0], agent.TimerFired)
		require.EqualValues(t, 1, timer.Timer.Generation)
		// No caller media was sent. Playback and timers cannot advance input progress.
		_, progress, err := sys.workers[0].SessionAgent(call.SessionID())
		require.NoError(t, err)
		require.Zero(t, progress.Consumed)
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
func TestV2PacerNoCatchupAfterStall(t *testing.T) {
	if testing.Short() {
		t.Skip("stall matrix; fast rebasing proof is in mediaworker")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		regs, _, gate, _ := pacerRegistrations(t, 20, false, false)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs})
		var sent atomic.Int64
		require.NoError(t, workerprobe.SetAfterEcho(sys.workers[0].LocalAddr(), func(ctx context.Context, _ string, _ []byte) {
			if sent.Add(1) == 5 {
				timer := time.NewTimer(150 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
				}
			}
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		received := receivePacer(call)
		close(gate)
		var all []pacedPacket
		for range 20 {
			all = append(all, nextPaced(t, received))
		}
		assertPacerSequence(t, all, pacerFixtures(t, 20), true)
		require.Greater(t, all[5].at.Sub(all[4].at), 120*time.Millisecond)
		require.GreaterOrEqual(t, all[19].at.Sub(all[5].at), 250*time.Millisecond, "overdue deadlines cannot drain as a burst")
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
func TestV2PacerExactMoveAndTimer(t *testing.T) {
	if testing.Short() {
		t.Skip("full reply move matrix; fast silence/finish/timer harness remains")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		regs, events, gate, _ := pacerRegistrations(t, 80, false, false)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, remoteTarget: true})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		received := receivePacer(call)
		close(gate)
		var all []pacedPacket
		for range 8 {
			all = append(all, nextPaced(t, received))
		}
		_, err = sys.plane.Move(ctx, call.SessionID(), "1")
		require.NoError(t, err)
		for len(all) < 80 {
			all = append(all, nextPaced(t, received))
		}
		assertPacerSequence(t, all, pacerFixtures(t, 80), true)
		timer := awaitPacerEvent(t, events[1], agent.TimerFired)
		require.EqualValues(t, 1, timer.Timer.Generation)
		finish := awaitPacerEvent(t, events[1], agent.PlaybackFinished)
		require.EqualValues(t, 1, finish.Playback.Turn)
		select {
		case p := <-received:
			t.Fatalf("repeated audio after finish: %v", p.packet.SequenceNumber)
		case <-time.After(100 * time.Millisecond):
		}
		for _, ch := range events {
			for len(ch) > 0 {
				e := <-ch
				require.NotEqual(t, agent.TimerFired, e.Kind, "timer generation fires only once across move")
			}
		}
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
func TestV2PacerClearCancelDropsLateAudio(t *testing.T) {
	if testing.Short() {
		t.Skip("barge-in matrix; fast clear/cancel proof is in mediaworker")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		regs, events, gate, late := pacerRegistrations(t, 80, true, true)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs})
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		received := receivePacer(call)
		close(gate)
		for range 5 {
			nextPaced(t, received)
		}
		require.NoError(t, call.SendMedia(ctx, 20*time.Millisecond))
		awaitPacerEvent(t, events[0], agent.AudioInput)
		terminal := awaitPacerEvent(t, events[0], agent.TaskMessage)
		require.True(t, terminal.Result.Final)
		require.Equal(t, agent.Canceled, terminal.Result.Outcome)
		require.Eventually(t, func() bool { return late.Load() == 1 }, time.Second, time.Millisecond)
		// Drain at most the one packet already in flight at the accepted clear.
		remaining := 0
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()
	done:
		for {
			select {
			case <-received:
				remaining++
			case <-timer.C:
				break done
			}
		}
		require.LessOrEqual(t, remaining, 1, "clear stops the old turn within one packet interval")
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
func TestV2PacerTakeoverLosesSpeechAndKeepsTimer(t *testing.T) {
	if testing.Short() {
		t.Skip("takeover matrix; fast loss metadata proof is in mediaworker")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		regs, events, gate, _ := pacerRegistrations(t, 80, false, false)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, remoteTarget: true})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		received := receivePacer(call)
		close(gate)
		for range 8 {
			nextPaced(t, received)
		}
		require.Eventually(t, func() bool {
			b, err := store.GetState(ctx, call.SessionID())
			if err != nil {
				return false
			}
			var snap struct {
				State struct {
					Agent struct {
						V2 struct {
							Playback struct {
								Pending int `json:"p"`
							} `json:"pb"`
						}
					}
				}
			}
			return json.Unmarshal(b, &snap) == nil && snap.State.Agent.V2.Playback.Pending > 0
		}, time.Second, time.Millisecond)
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		resumed := awaitPacerEvent(t, events[1], agent.Resumed)
		require.Equal(t, agent.OutcomeUnknown, resumed.Resume.MoveOnly)
		timer := awaitPacerEvent(t, events[1], agent.TimerFired)
		require.EqualValues(t, 1, timer.Timer.Generation)
		for len(received) > 0 {
			<-received
		}
		select {
		case p := <-received:
			t.Fatalf("takeover activated missing audio: %v", p.packet.SequenceNumber)
		case <-time.After(100 * time.Millisecond):
		}
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

func TestV2PacerMoveWhileGenerating(t *testing.T) {
	if testing.Short() {
		t.Skip("generation retry matrix; exact-prefix unit proof remains")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		regs, events, gate, _ := pacerRegistrations(t, 80, false, false)
		packets := pacerFixtures(t, 80)
		regs[0].Tasks["tts/v1"] = func(ctx context.Context, run *agent.TaskRun) error {
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
			if err := run.Emit(ctx, 1, agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: packets[:40]}}); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		}
		regs[1].Tasks["tts/v1"] = func(ctx context.Context, run *agent.TaskRun) error {
			if run.After() != 1 || string(run.Spec().Request) != "synthetic TTS request" {
				return run.Fail(agent.Failed, "missing reproducible request or prefix")
			}
			return run.Emit(ctx, 2, agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: packets[40:]}})
		}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, remoteTarget: true})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		received := receivePacer(call)
		close(gate)
		var all []pacedPacket
		for range 8 {
			all = append(all, nextPaced(t, received))
		}
		_, err = sys.plane.Move(ctx, call.SessionID(), "1")
		require.NoError(t, err)
		for len(all) < 80 {
			all = append(all, nextPaced(t, received))
		}
		assertPacerSequence(t, all, packets, true)
		awaitPacerEvent(t, events[1], agent.PlaybackFinished)
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
