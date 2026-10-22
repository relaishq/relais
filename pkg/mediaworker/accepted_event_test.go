package mediaworker

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// A real registration with a scripted application. Tests supply events and
// handler output, and observe only published state, receipts, export and media.
type acceptedEventAgent struct {
	state  agent.State
	handle func(context.Context, agent.Event) (agent.Effects, error)
	save   func(context.Context) error
	events []agent.Event
}

func (a *acceptedEventAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	a.events = append(a.events, e)
	if a.handle != nil {
		return a.handle(ctx, e)
	}
	return agent.Effects{}, nil
}
func (a *acceptedEventAgent) Save(ctx context.Context) (agent.State, error) {
	if a.save != nil {
		if err := a.save(ctx); err != nil {
			return agent.State{}, err
		}
	}
	return a.state, nil
}
func (a *acceptedEventAgent) Restore(_ context.Context, state agent.State) error {
	a.state = state
	return nil
}

type acceptedEventRig struct {
	s     *session
	app   *acceptedEventAgent
	wire  *dtlsWireConn
	store *sessionstore.Memory
}

func newAcceptedEventRig(t *testing.T, bounds agenthost.Bounds, handlers map[string]agent.TaskHandler) acceptedEventRig {
	t.Helper()
	store := sessionstore.NewMemory()
	snap := dtlsFixture(t)
	lease, err := store.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(t, err)
	s, wire := dtlsCheckpointSession(t, store, snap, lease, ResumeOptions{})
	app := &acceptedEventAgent{state: agent.State{Version: 1, Bytes: []byte("accepted")}}
	s.worker.cfg.SnapshotInterval = 100 * time.Millisecond
	s.worker.cfg.Agent = AgentConfig{CallbackTimeout: 5 * time.Second, Factory: agenthost.Factory(agenthost.Registration{
		Name: "accepted/v1", Factory: func(agent.FactoryArgs) agent.Agent { return app }, Bounds: bounds, Tasks: handlers,
	})}
	s.state.Agent = agentState{}
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.worker.sessions = map[string]*session{s.id: s}
	// A fixed, conservative output clock lets tests advance the pacer explicitly.
	s.storeClock = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	s.storeClockAt = time.Now()
	return acceptedEventRig{s, app, wire, store}
}

// Exercise the production TaskRun adapter. The test drives the actor's existing
// emission route while the registered handler stays parked on its context.
func startAcceptedSpeech(t *testing.T, bounds agenthost.Bounds, packetGroups ...[][]byte) (acceptedEventRig, []*agent.TaskRun) {
	t.Helper()
	started := make(chan *agent.TaskRun, len(packetGroups))
	rig := newAcceptedEventRig(t, bounds, map[string]agent.TaskHandler{"speech/v1": func(ctx context.Context, run *agent.TaskRun) error {
		started <- run
		<-ctx.Done()
		return ctx.Err()
	}})
	rig.app.handle = func(_ context.Context, e agent.Event) (agent.Effects, error) {
		if e.Kind == agent.Started {
			effects := agent.Effects{}
			for i := range packetGroups {
				effects.StartTasks = append(effects.StartTasks, agent.TaskSpec{Step: fmt.Sprint(i), Kind: "speech/v1", Turn: 1, Class: agent.ReadOnly, Resume: agent.Retry})
			}
			return effects, nil
		}
		if e.Kind == agent.TaskMessage && e.Result.Chunk.Audio != nil {
			effects := agent.Effects{}
			for i := range e.Result.Chunk.Audio.Packets {
				effects.Audio = append(effects.Audio, agent.Audio{Turn: e.Result.Turn, Source: agent.AudioRef{TaskID: e.Result.TaskID, Sequence: e.Result.Sequence, PacketOffset: uint32(i)}})
			}
			return effects, nil
		}
		return agent.Effects{}, nil
	}
	_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: "started/speech"}, eventLive)
	require.NoError(t, err)
	rig.s.launchTasks()
	runs := make([]*agent.TaskRun, len(packetGroups))
	for range packetGroups {
		run := <-started
		var i int
		_, err := fmt.Sscan(run.Spec().Step, &i)
		require.NoError(t, err)
		runs[i] = run
	}
	for i, packets := range packetGroups {
		if packets == nil {
			continue
		}
		done := make(chan error, 1)
		go func() {
			done <- runs[i].Emit(context.Background(), 1, agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: packets}})
		}()
		emission := <-rig.s.agent.v2.emissions
		rig.s.acceptEmission(emission)
		require.NoError(t, <-done)
	}
	t.Cleanup(rig.s.stopTaskAttempts)
	return rig, runs
}

func TestAcceptedEventUnknownHasNoCallbackOrPublication(t *testing.T) {
	rig := newAcceptedEventRig(t, agenthost.Bounds{}, nil)
	before, progress, err := rig.s.worker.SessionAgent(rig.s.id)
	require.NoError(t, err)
	rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
		t.Error("unknown kind called Handle")
		return agent.Effects{}, nil
	}
	rig.app.save = func(context.Context) error { t.Error("unknown kind called Save"); return nil }
	_, err = (acceptedEvents{rig.s}).submit(agent.Event{Kind: "future"}, eventLive)
	require.NoError(t, err)
	after, nextProgress, err := rig.s.worker.SessionAgent(rig.s.id)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, progress, nextProgress)
}

func TestAcceptedEventInvalidEffectsRestoreWholePair(t *testing.T) {
	for _, kind := range []string{"resumed-start", "invalid-task", "missing-audio", "save"} {
		t.Run(kind, func(t *testing.T) {
			rig := newAcceptedEventRig(t, agenthost.Bounds{}, nil)
			before, progress, err := rig.s.worker.SessionAgent(rig.s.id)
			require.NoError(t, err)
			event := agent.Event{Kind: agent.Started, ID: "invalid"}
			rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
				rig.app.state.Bytes = []byte("rejected")
				switch kind {
				case "resumed-start":
					return agent.Effects{StartTasks: []agent.TaskSpec{{Kind: "missing/v1"}}}, nil
				case "invalid-task":
					return agent.Effects{StartTasks: []agent.TaskSpec{{}}}, nil
				case "missing-audio":
					return agent.Effects{Audio: []agent.Audio{{}}}, nil
				}
				return agent.Effects{}, nil
			}
			if kind == "resumed-start" {
				event.Kind = agent.Resumed
			}
			if kind == "save" {
				rig.app.save = func(context.Context) error { return fmt.Errorf("save refused") }
			}
			_, err = (acceptedEvents{rig.s}).submit(event, eventLive)
			require.Error(t, err)
			after, nextProgress, readErr := rig.s.worker.SessionAgent(rig.s.id)
			require.NoError(t, readErr)
			require.Equal(t, before, after, "invalid transaction published app state")
			require.Equal(t, progress, nextProgress)
			require.Equal(t, before.Bytes, rig.app.state.Bytes, "rollback did not restore the application")
			status, readErr := rig.s.worker.agentSessionStatus(rig.s.id)
			require.NoError(t, readErr)
			require.Empty(t, status.Tasks)
			require.Zero(t, status.PlaybackPackets)
		})
	}
}

func TestAcceptedEventPlaybackKeepsConcurrentPacerPrefix(t *testing.T) {
	for _, gateAt := range []string{"handle", "save"} {
		t.Run(gateAt, func(t *testing.T) {
			first := [][]byte{{0xf8, 1}, {0xf8, 2}, {0xf8, 3}}
			next := [][]byte{{0xf8, 4}, {0xf8, 5}}
			rig, runs := startAcceptedSpeech(t, agenthost.Bounds{}, first, next)
			delivered, err := rig.s.deliverTaskResult()
			require.NoError(t, err)
			require.True(t, delivered)
			entered, release := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			original := rig.app.handle
			if gateAt == "handle" {
				rig.app.handle = func(ctx context.Context, e agent.Event) (agent.Effects, error) {
					close(entered)
					<-release
					return original(ctx, e)
				}
			} else {
				rig.app.save = func(context.Context) error { close(entered); <-release; return nil }
			}
			done := make(chan error, 1)
			go func() { _, err := rig.s.deliverTaskResult(); done <- err }()
			<-entered
			// Neither callback nor Save owns the media lock. Send two packets before
			// publication, then finish the accepted transaction and drain the remainder.
			connection, ok := rig.s.dtlsConn.ConnectionState()
			require.True(t, ok)
			config := srtp.Config{Profile: rig.s.state.SRTP.Profile}
			require.NoError(t, config.ExtractSessionKeysFromDTLS(&connection, false))
			receiver := testContext(t, config.Keys.LocalMasterKey, config.Keys.LocalMasterSalt, config.Profile)
			captureClock := func() playbackState {
				raw, err := rig.s.checkpoint.capture(checkpointLive, nil)
				require.NoError(t, err)
				saved, err := decodeSnapshot(raw)
				require.NoError(t, err)
				return saved.State.Agent.V2.Playback
			}
			var clocks []playbackState
			receive := func() rtp.Packet {
				_, err := rig.s.paceOne()
				require.NoError(t, err)
				raw := <-rig.wire.packets
				plain, err := receiver.DecryptRTP(nil, raw, nil)
				require.NoError(t, err)
				var packet rtp.Packet
				require.NoError(t, packet.Unmarshal(plain))
				clocks = append(clocks, captureClock())
				rig.s.mu.Lock()
				rig.s.storeClock = rig.s.storeClock.Add(time.Second)
				rig.s.storeClockAt = time.Now()
				rig.s.mu.Unlock()
				return packet
			}
			packets := []rtp.Packet{receive(), receive()}
			beforeCommit := captureClock()
			close(release)
			require.NoError(t, <-done)
			rig.app.save = nil
			afterCommit := captureClock()
			require.Equal(t, 1, beforeCommit.Pending)
			require.Equal(t, 3, afterCommit.Pending, "commit must retain the remaining queue")
			require.Equal(t, beforeCommit.NextAt, afterCommit.NextAt, "commit restored a stale playback deadline")
			require.Equal(t, beforeCommit.NextTS, afterCommit.NextTS, "commit restored a stale RTP timestamp")
			require.Equal(t, beforeCommit.Initialized, afterCommit.Initialized, "commit restored stale clock initialization")
			status, err := rig.s.worker.agentSessionStatus(rig.s.id)
			require.NoError(t, err)
			require.Equal(t, 3, status.PlaybackPackets, "commit must retain the remaining queue")
			for range 3 {
				packets = append(packets, receive())
			}
			for i, packet := range packets {
				require.Equal(t, []byte{0xf8, byte(i + 1)}, packet.Payload, "playback lost, repeated or reordered a packet")
				require.True(t, clocks[i].Initialized)
				require.Equal(t, packet.Timestamp+960, clocks[i].NextTS, "exported clock must follow the sent 20 ms packet")
				if i == 0 {
					require.Equal(t, rig.s.state.Audio.InitialTS, packet.Timestamp)
				} else {
					require.Equal(t, packets[i-1].SequenceNumber+1, packet.SequenceNumber)
					// A one-second clock advance rebases the pacer. RTP media time
					// must advance with the exported absolute playback schedule.
					elapsed := clocks[i].NextAt.Sub(clocks[i-1].NextAt)
					require.InDelta(t, rtpClockTicks(elapsed), packet.Timestamp-packets[i-1].Timestamp, 1)
				}
			}
			status, err = rig.s.worker.agentSessionStatus(rig.s.id)
			require.NoError(t, err)
			require.Zero(t, status.PlaybackPackets)
			require.Len(t, status.Tasks, 2)
			for i, task := range status.Tasks {
				require.Equal(t, runs[i].ID(), task.ID)
				require.EqualValues(t, 1, task.Cursor)
			}
		})
	}
}

func TestAcceptedEventNonAudioOverflowLeavesPublishedPair(t *testing.T) {
	for _, withAudio := range []bool{false, true} {
		t.Run(fmt.Sprint(withAudio), func(t *testing.T) {
			rig, _ := startAcceptedSpeech(t, agenthost.Bounds{}, [][]byte{{0xf8, 0xff, 0xfe}})
			before, progress, err := rig.s.worker.SessionAgent(rig.s.id)
			require.NoError(t, err)
			original := rig.app.handle
			rig.app.handle = func(ctx context.Context, e agent.Event) (agent.Effects, error) {
				effects, err := original(ctx, e)
				if !withAudio {
					effects.Audio = nil
				}
				for i := range 40 {
					effects.SetTimers = append(effects.SetTimers, agent.TimerSpec{ID: fmt.Sprintf("timer/%02d/%0100d", i, i), After: time.Hour})
				}
				rig.app.state.Bytes = []byte("rejected")
				return effects, err
			}
			delivered, err := rig.s.deliverTaskResult()
			require.ErrorIs(t, err, agent.ErrCapacity, "non-audio metadata overflow must terminate on the first delivery")
			require.False(t, delivered)
			after, nextProgress, err := rig.s.worker.SessionAgent(rig.s.id)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, progress, nextProgress)
			status, err := rig.s.worker.agentSessionStatus(rig.s.id)
			require.NoError(t, err)
			require.Zero(t, status.Tasks[0].Cursor)
			require.Zero(t, status.PlaybackPackets)
			raw, captureErr := rig.s.checkpoint.capture(checkpointLive, nil)
			require.NoError(t, captureErr)
			captured, captureErr := decodeSnapshot(raw)
			require.NoError(t, captureErr)
			require.Empty(t, captured.State.Agent.V2.Timers)
		})
	}
}

func TestAcceptedEventTaskIdentityAndConflictAreAtomic(t *testing.T) {
	rig := newAcceptedEventRig(t, agenthost.Bounds{}, map[string]agent.TaskHandler{"work/v1": func(context.Context, *agent.TaskRun) error { return nil }})
	spec := agent.TaskSpec{Step: "one", Kind: "work/v1", Class: agent.ReadOnly, Request: []byte("original")}
	rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
		return agent.Effects{StartTasks: []agent.TaskSpec{spec}}, nil
	}
	event := agent.Event{Kind: agent.Started, ID: "cause"}
	_, err := (acceptedEvents{rig.s}).submit(event, eventLive)
	require.NoError(t, err)
	before, err := rig.s.worker.agentSessionStatus(rig.s.id)
	require.NoError(t, err)
	_, err = (acceptedEvents{rig.s}).submit(event, eventLive)
	require.NoError(t, err)
	after, err := rig.s.worker.agentSessionStatus(rig.s.id)
	require.NoError(t, err)
	require.Equal(t, before.Tasks, after.Tasks)
	spec.Request = []byte("conflict")
	_, err = (acceptedEvents{rig.s}).submit(event, eventLive)
	require.ErrorIs(t, err, agent.ErrIncompatibleAgent)
	after, err = rig.s.worker.agentSessionStatus(rig.s.id)
	require.NoError(t, err)
	require.Equal(t, before.Tasks, after.Tasks)
	payload, err := rig.s.saveV2MoveOnly(context.Background())
	require.NoError(t, err)
	require.Len(t, payload.tasks, 1)
	require.Equal(t, []byte("original"), payload.tasks[0].Request)
}

// Keep the two public state views exercised with non-input events as well.
func TestAcceptedEventTimerDoesNotConsumeInput(t *testing.T) {
	rig := newAcceptedEventRig(t, agenthost.Bounds{}, nil)
	rig.app.handle = func(_ context.Context, e agent.Event) (agent.Effects, error) {
		if e.Kind == agent.Started {
			return agent.Effects{SetTimers: []agent.TimerSpec{{ID: "once"}}}, nil
		}
		rig.app.state.Bytes = []byte("timer committed")
		return agent.Effects{}, nil
	}
	input := legacy.Input{Progress: legacy.Progress{SSRC: 7, Index: 8}}
	_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.AudioInput, ID: "audio/7/8", Input: &input}, eventLive)
	require.NoError(t, err)
	_, before, err := rig.s.worker.SessionAgent(rig.s.id)
	require.NoError(t, err)
	_, err = (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: "start"}, eventLive)
	require.NoError(t, err)
	require.NoError(t, rig.s.deliverClockEvent())
	state, after, err := rig.s.worker.SessionAgent(rig.s.id)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, []byte("timer committed"), state.Bytes)
	require.Equal(t, agent.TimerFired, rig.app.events[len(rig.app.events)-1].Kind)
}

func TestAcceptedEventDeliveryUsesReservedReferenceSpace(t *testing.T) {
	packets := [][]byte{{0xf8, 1}, {0xf8, 2}, {0xf8, 3}, {0xf8, 4}, {0xf8, 5}}
	probe, _ := startAcceptedSpeech(t, agenthost.Bounds{}, packets)
	status, err := probe.s.worker.agentSessionStatus(probe.s.id)
	require.NoError(t, err)
	rig, _ := startAcceptedSpeech(t, agenthost.Bounds{MaxMailboxBytes: int(status.PendingBytes)}, packets)
	delivered, err := rig.s.deliverTaskResult()
	require.NoError(t, err, "delivery must use the reference headroom already admitted by Emit")
	require.True(t, delivered)
	status, err = rig.s.worker.agentSessionStatus(rig.s.id)
	require.NoError(t, err)
	require.Equal(t, len(packets), status.PlaybackPackets)
	payload, err := rig.s.saveV2MoveOnly(context.Background())
	require.NoError(t, err)
	require.Len(t, payload.audio, len(packets))
	require.Equal(t, packets, payload.tasks[0].Audio[0].Chunk.Audio.Packets)
}

func TestAcceptedEventRefusalDoesNotEvictAcceptedTasks(t *testing.T) {
	rig := newAcceptedEventRig(t, agenthost.Bounds{}, map[string]agent.TaskHandler{"work/v1": func(context.Context, *agent.TaskRun) error { return nil }})
	width := 6
	rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
		effects := agent.Effects{}
		for i := range width {
			effects.StartTasks = append(effects.StartTasks, agent.TaskSpec{Step: fmt.Sprint(i), Kind: "work/v1", Class: agent.ReadOnly})
		}
		return effects, nil
	}
	_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: "older"}, eventLive)
	require.NoError(t, err)
	before, err := rig.s.worker.agentSessionStatus(rig.s.id)
	require.NoError(t, err)
	require.NotEmpty(t, before.Tasks)
	for i, task := range before.Tasks {
		require.Equal(t, fmt.Sprint(i), task.Step, "admission must preserve the first tasks")
	}
	width = 14
	_, err = (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: "fresh"}, eventLive)
	require.ErrorIs(t, err, agent.ErrBackpressure)
	after, err := rig.s.worker.agentSessionStatus(rig.s.id)
	require.NoError(t, err)
	require.Equal(t, before.Tasks, after.Tasks, "fitting fresh refusals must not evict accepted work")
}

func TestAcceptedEventCancellationFencesHandlerAndOldTurn(t *testing.T) {
	rig, runs := startAcceptedSpeech(t, agenthost.Bounds{}, [][]byte{{0xf8, 1}, {0xf8, 2}})
	delivered, err := rig.s.deliverTaskResult()
	require.NoError(t, err)
	require.True(t, delivered)
	floor := agent.Turn(2)
	rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
		return agent.Effects{CancelBefore: &floor, ClearAudioBefore: &floor}, nil
	}
	_, err = (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: "barge"}, eventLive)
	require.NoError(t, err)
	status, err := rig.s.worker.agentSessionStatus(rig.s.id)
	require.NoError(t, err)
	require.Zero(t, status.PlaybackPackets)
	require.Equal(t, agent.Canceled, status.Tasks[0].Outcome)
	delivered, err = rig.s.deliverTaskResult()
	require.NoError(t, err)
	require.True(t, delivered)
	terminal := rig.app.events[len(rig.app.events)-1].Result
	require.Equal(t, agent.Canceled, terminal.Outcome)
	delivered, err = rig.s.deliverTaskResult()
	require.NoError(t, err)
	require.False(t, delivered, "cancellation emitted more than one terminal")
	err = runs[0].Emit(context.Background(), 2, agent.Chunk{Data: []byte("late")})
	require.ErrorIs(t, err, context.Canceled)
	floor = 1
	_, err = (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: "older-floor"}, eventLive)
	require.NoError(t, err)
	payload, err := rig.s.saveV2MoveOnly(context.Background())
	require.NoError(t, err)
	require.NoError(t, rig.s.checkpoint.persist(context.Background(), checkpointRequired))
	raw, err := rig.store.GetState(context.Background(), rig.s.id)
	require.NoError(t, err)
	snap, err := decodeSnapshot(raw)
	require.NoError(t, err)
	require.EqualValues(t, 2, snap.State.Agent.V2.CancelFloor)
	require.Empty(t, payload.audio)
}

func TestAcceptedEventAudioReferencesUseCurrentResultAndFloor(t *testing.T) {
	for _, kind := range []string{"prior-result", "cancel-floor", "clear-floor"} {
		t.Run(kind, func(t *testing.T) {
			rig, runs := startAcceptedSpeech(t, agenthost.Bounds{}, [][]byte{{0xf8, 1}})
			result := agent.Audio{Turn: 1, Source: agent.AudioRef{TaskID: runs[0].ID(), Sequence: 1}}
			original := rig.app.handle
			if kind == "prior-result" {
				delivered, err := rig.s.deliverTaskResult()
				require.NoError(t, err)
				require.True(t, delivered)
				rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
					return agent.Effects{Audio: []agent.Audio{result}}, nil
				}
				_, err = (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: "prior"}, eventLive)
				require.ErrorIs(t, err, agent.ErrIncompatibleAgent)
			} else {
				floor := agent.Turn(2)
				rig.app.handle = func(ctx context.Context, e agent.Event) (agent.Effects, error) {
					effects, err := original(ctx, e)
					if kind == "cancel-floor" {
						effects.CancelBefore = &floor
					} else {
						effects.ClearAudioBefore = &floor
					}
					return effects, err
				}
				delivered, err := rig.s.deliverTaskResult()
				require.NoError(t, err)
				require.True(t, delivered)
				status, err := rig.s.worker.agentSessionStatus(rig.s.id)
				require.NoError(t, err)
				require.Zero(t, status.PlaybackPackets, "a same-event floor cannot append superseded audio")
			}
		})
	}
}

func TestAcceptedEventTimerLifecycle(t *testing.T) {
	rig := newAcceptedEventRig(t, agenthost.Bounds{}, nil)
	effects := agent.Effects{}
	rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) { return effects, nil }
	for i := range 200 {
		id := fmt.Sprintf("turn/%d", i)
		effects = agent.Effects{SetTimers: []agent.TimerSpec{{ID: id}}}
		_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: agent.EventID("arm/" + id)}, eventLive)
		require.NoError(t, err)
		effects = agent.Effects{CancelTimers: []string{id}}
		_, err = (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: agent.EventID("cancel/" + id)}, eventLive)
		require.NoError(t, err)
		effects = agent.Effects{}
		count := len(rig.app.events)
		require.NoError(t, rig.s.deliverClockEvent())
		require.Len(t, rig.app.events, count, "cancelled timer must not fire")
	}
	for _, id := range []string{string([]byte{255}), string(make([]byte, 129))} {
		effects = agent.Effects{SetTimers: []agent.TimerSpec{{ID: id}}}
		_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: "invalid"}, eventLive)
		require.ErrorIs(t, err, agent.ErrIncompatibleAgent)
	}
	var generations []uint64
	for round := range 2 {
		effects = agent.Effects{SetTimers: []agent.TimerSpec{{ID: "reused"}}}
		if round == 1 {
			effects.SetTimers = append(effects.SetTimers, agent.TimerSpec{ID: "other"})
		}
		_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: "rearm"}, eventLive)
		require.NoError(t, err)
		effects = agent.Effects{}
		require.NoError(t, rig.s.deliverClockEvent())
		fired := rig.app.events[len(rig.app.events)-1]
		require.Equal(t, agent.TimerFired, fired.Kind)
		generations = append(generations, fired.Timer.Generation)
	}
	require.Greater(t, generations[1], generations[0], "deletion cannot reuse a timer generation")
	require.NoError(t, rig.s.deliverClockEvent())
	fired := rig.app.events[len(rig.app.events)-1]
	require.Equal(t, "other", fired.Timer.ID)
	require.Greater(t, fired.Timer.Generation, generations[1])
	count := len(rig.app.events)
	require.NoError(t, rig.s.deliverClockEvent())
	require.Len(t, rig.app.events, count, "a committed generation fires once")
}

func TestAcceptedEventControlCopyRefusalAndReceipt(t *testing.T) {
	for _, failure := range []string{"error", "deadline"} {
		t.Run(failure, func(t *testing.T) {
			rig := newAcceptedEventRig(t, agenthost.Bounds{}, nil)
			_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: "started"}, eventLive)
			require.NoError(t, err)
			rig.app.handle = func(ctx context.Context, e agent.Event) (agent.Effects, error) {
				rig.app.state.Bytes = []byte("rejected")
				if failure == "deadline" {
					<-ctx.Done()
					// Only Handle's deliberate expiry is short. Restore must not depend
					// on a small success deadline while the race detector is running.
					// The callback settles before the host reads this budget for Restore.
					rig.s.worker.cfg.Agent.CallbackTimeout = 5 * time.Second
					return agent.Effects{}, ctx.Err()
				}
				return agent.Effects{}, fmt.Errorf("rejected Control")
			}
			if failure == "deadline" {
				rig.s.worker.cfg.Agent.CallbackTimeout = 20 * time.Millisecond
			}
			m := agent.ControlMessage{ID: "press", Sequence: 1, Name: "remember", Data: []byte("owned")}
			_, err = (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Control, Control: &m}, eventLive)
			require.ErrorIs(t, err, agent.ErrControlRejected)
			state, progress, err := rig.s.worker.SessionAgent(rig.s.id)
			require.NoError(t, err)
			require.Equal(t, []byte("accepted"), state.Bytes)
			rig.s.worker.cfg.Agent.CallbackTimeout = 5 * time.Second
			require.Zero(t, progress.Consumed)
			rig.app.handle = func(_ context.Context, e agent.Event) (agent.Effects, error) {
				e.Control.Data[0] = 'X'
				e.Control.ID = "changed"
				e.Control.Sequence = 9
				rig.app.state.Bytes = []byte("committed")
				return agent.Effects{}, nil
			}
			receipt, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Control, Control: &m}, eventLive)
			require.NoError(t, err)
			require.Equal(t, []byte("owned"), m.Data, "callback aliases the caller's Control bytes")
			count := len(rig.app.events)
			retry, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Control, Control: &m}, eventLive)
			require.NoError(t, err)
			require.Equal(t, receipt, retry)
			require.Len(t, rig.app.events, count, "identical retry must not invoke the application")
			require.NoError(t, rig.s.checkpoint.persist(context.Background(), checkpointRequired))
			retry, err = (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Control, Control: &m}, eventLive)
			require.NoError(t, err)
			require.True(t, retry.Checkpointed)
			state, progress, err = rig.s.worker.SessionAgent(rig.s.id)
			require.NoError(t, err)
			require.Equal(t, []byte("committed"), state.Bytes)
			require.Zero(t, progress.Consumed)
		})
	}
}
func TestAcceptedEventReceiptPressureAllowsIndependentInput(t *testing.T) {
	rig := newAcceptedEventRig(t, agenthost.Bounds{MaxPendingBytes: 900}, map[string]agent.TaskHandler{"wait/v1": func(context.Context, *agent.TaskRun) error { return nil }})
	effects := agent.Effects{}
	rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) { return effects, nil }
	_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: "started"}, eventLive)
	require.NoError(t, err)
	for i := uint64(1); i <= 2; i++ {
		_, err = (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: fmt.Sprint(i), Sequence: i, Name: "press"}}, eventLive)
		require.NoError(t, err)
	}
	fanout := agent.Effects{}
	for i := range 10 {
		fanout.StartTasks = append(fanout.StartTasks, agent.TaskSpec{Step: fmt.Sprint(i), Kind: "wait/v1", Class: agent.ReadOnly})
	}
	effects = fanout
	event := agent.Event{Kind: agent.Started, ID: "fan"}
	_, err = (acceptedEvents{rig.s}).submit(event, eventLive)
	require.ErrorIs(t, err, agent.ErrBackpressure)
	count := len(rig.app.events)
	_, err = (acceptedEvents{rig.s}).submit(event, eventLive)
	require.ErrorIs(t, err, agent.ErrBackpressure)
	require.Len(t, rig.app.events, count, "receipt-blocked event was not throttled")
	effects = agent.Effects{}
	input := legacy.Input{Progress: legacy.Progress{SSRC: 1, Index: 1}}
	_, err = (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.AudioInput, ID: "audio/1/1", Input: &input}, eventLive)
	require.NoError(t, err)
	effects = fanout
	_, err = (acceptedEvents{rig.s}).submit(event, eventLive)
	require.ErrorIs(t, err, agent.ErrBackpressure)
	require.Len(t, rig.app.events, count+1, "unrelated input must not reset the receipt delay")
	require.NoError(t, rig.s.checkpoint.persist(context.Background(), checkpointRequired))
	_, err = (acceptedEvents{rig.s}).submit(event, eventLive)
	require.NoError(t, err, "checkpoint acknowledgement releases receipt pressure")
	require.Len(t, rig.app.events, count+2)
}

func TestAcceptedEventStreamingWindowAndBargeIn(t *testing.T) {
	if testing.Short() {
		t.Skip("200 real rate-limited emissions per reply")
	}
	for _, packet := range [][]byte{{0xf8, 0xff, 0xfe}, {0xf0, 0xff, 0xfe}, {0x80, 0xff}} {
		for _, others := range []int{0, 2} {
			t.Run(fmt.Sprintf("%x/other=%d", packet[0], others), func(t *testing.T) {
				groups := [][][]byte{{packet}}
				for range others {
					groups = append(groups, nil)
				}
				rig, runs := startAcceptedSpeech(t, agenthost.Bounds{}, groups...)
				for seq := uint64(1); seq <= 200; seq++ {
					delivered, err := rig.s.deliverTaskResult()
					require.NoError(t, err, "an ordinary streamed reply must stay admissible")
					require.True(t, delivered)
					if seq < 200 {
						done := make(chan error, 1)
						go func() {
							done <- runs[0].Emit(context.Background(), seq+1, agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{packet}}})
						}()
						for {
							select {
							case emission := <-rig.s.agent.v2.emissions:
								rig.s.acceptEmission(emission)
							case err := <-done:
								require.NoError(t, err)
								goto emitted
							}
						}
					}
				emitted:
				}
				require.NoError(t, rig.s.checkpoint.persist(context.Background(), checkpointRequired))
				raw, err := rig.store.GetState(context.Background(), rig.s.id)
				require.NoError(t, err)
				saved, err := decodeSnapshot(raw)
				require.NoError(t, err)
				require.Len(t, saved.State.Agent.V2.Playback.Window, 1, "checkpoint window must not grow with Emit spans")
				payload, err := rig.s.saveV2MoveOnly(context.Background())
				require.NoError(t, err)
				require.Len(t, payload.audio, 200, "exact export must retain the whole reply")
				floor := agent.Turn(2)
				rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
					return agent.Effects{ClearAudioBefore: &floor, CancelBefore: &floor, StartTasks: []agent.TaskSpec{{Step: "next", Kind: "speech/v1", Turn: 2, Class: agent.ReadOnly, Resume: agent.Retry, Request: []byte("x")}}}, nil
				}
				_, err = (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: "next-turn"}, eventLive)
				require.NoError(t, err, "barge-in admission must use the cleared playback projection")
				status, err := rig.s.worker.agentSessionStatus(rig.s.id)
				require.NoError(t, err)
				require.Zero(t, status.PlaybackPackets)
				require.Equal(t, "next", status.Tasks[len(status.Tasks)-1].Step)
			})
		}
	}
}

func TestAcceptedEventRefusalsRemainFIFO(t *testing.T) {
	rig := newAcceptedEventRig(t, agenthost.Bounds{MaxMailboxBytes: 4096}, map[string]agent.TaskHandler{"work/v1": func(context.Context, *agent.TaskRun) error { return nil }})
	batches := []agent.Event{{Kind: agent.Started, ID: "first"}, {Kind: agent.Started, ID: "second"}}
	rig.app.handle = func(_ context.Context, e agent.Event) (agent.Effects, error) {
		effects := agent.Effects{}
		if e.Kind == agent.Started {
			if e.ID == batches[0].ID {
				effects.StartTasks = append(effects.StartTasks, agent.TaskSpec{Step: "admitted", Kind: "work/v1", Class: agent.ReadOnly})
			}
			for i := range 4 {
				effects.StartTasks = append(effects.StartTasks, agent.TaskSpec{Step: fmt.Sprint(i), Kind: "work/v1", Turn: agent.Turn(i + 1), Class: agent.ReadOnly, Request: make([]byte, 4097)})
			}
		}
		return effects, nil
	}
	var expected []agent.TaskResult
	for _, batch := range batches {
		_, err := (acceptedEvents{rig.s}).submit(batch, eventLive)
		require.NoError(t, err)
		status, err := rig.s.worker.agentSessionStatus(rig.s.id)
		require.NoError(t, err)
		require.Len(t, status.Tasks, 1, "later fan-out must not evict the admitted task")
		require.Equal(t, deriveTaskID(rig.s.id, "accepted/v1", batches[0].ID, "admitted"), status.Tasks[0].ID)
		for i := range 4 {
			id := deriveTaskID(rig.s.id, "accepted/v1", batch.ID, fmt.Sprint(i))
			if id != status.Tasks[0].ID {
				expected = append(expected, agent.TaskResult{TaskID: id, Sequence: 1, Turn: agent.Turn(i + 1), Outcome: agent.Capacity, Final: true, Detail: "task admission capacity"})
			}
		}
	}
	require.Len(t, expected, 8)
	// Both submissions finish before any refusal drains. Exact identities catch
	// moving a fresh batch ahead of older refusals even if each batch stays FIFO.
	for _, want := range expected {
		count := len(rig.app.events)
		_, err := rig.s.deliverTaskResult()
		require.NoError(t, err)
		require.Len(t, rig.app.events, count+1)
		got := rig.app.events[len(rig.app.events)-1]
		require.Equal(t, agent.TaskMessage, got.Kind)
		require.Equal(t, &want, got.Result, "Capacity identities, order and completeness must survive both batches")
	}
	count := len(rig.app.events)
	delivered, err := rig.s.deliverTaskResult()
	require.NoError(t, err)
	require.False(t, delivered, "all refusals must drain exactly once")
	require.Len(t, rig.app.events, count)
}

func TestAcceptedEventParkedResultCannotCrossAttempt(t *testing.T) {
	rig, runs := startAcceptedSpeech(t, agenthost.Bounds{}, nil)
	done := make(chan error, 1)
	go func() { done <- runs[0].Emit(context.Background(), 1, agent.Chunk{Data: []byte("parked")}) }()
	parked := <-rig.s.agent.v2.emissions
	rig.s.stopTaskAttempts() // A move interrupts the attempt, not the task.
	require.ErrorIs(t, <-done, context.Canceled)
	rig.s.launchTasks()
	// The parked result must never become the new attempt's input, even though
	// its caller context is still live.
	rig.s.acceptEmission(parked)
	delivered, err := rig.s.deliverTaskResult()
	require.NoError(t, err)
	require.False(t, delivered, "a parked result crossed its attempt fence")
}
