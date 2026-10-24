package mediaworker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func bridgeOutputRig(t *testing.T) (*session, *bridgeThroughputAgent, *agent.TaskRun) {
	t.Helper()
	started := make(chan *agent.TaskRun, 4)
	handler := func(ctx context.Context, run *agent.TaskRun) error { started <- run; <-ctx.Done(); return ctx.Err() }
	rig := newAcceptedEventRig(t, agenthost.Bounds{}, nil)
	s := rig.s
	s.componentMode = true
	s.storeClockAt = time.Time{}
	s.state.Agent = agentState{}
	a := &bridgeThroughputAgent{state: agent.State{Version: 1, Bytes: make([]byte, 3072), History: make([]byte, 8192)}, timing: &bridgePacketTiming{ready: map[uint32]time.Time{}, admitted: map[uint32]time.Time{}}}
	a.sessionID = s.id
	binary.BigEndian.PutUint64(a.state.Bytes[8:16], 1)
	s.worker.cfg.Agent.Factory = agenthost.Factory(agenthost.Registration{Name: "bridge", Factory: func(agent.FactoryArgs) agent.Agent { return a }, Tasks: map[string]agent.TaskHandler{"bridge.state/v1": handler, "bridge.audio/v1": handler}})
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started, ID: "start"}, eventLive)
	require.NoError(t, err)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	s.launchTasks()
	t.Cleanup(s.stopTaskAttempts)
	var audio *agent.TaskRun
	for range 2 {
		run := <-started
		if run.Spec().Kind == "bridge.audio/v1" {
			audio = run
		}
	}
	require.NotNil(t, audio)
	return s, a, audio
}

func TestBridgeHistoryCandidateReservesUnsettledWrite(t *testing.T) {
	s, _, _ := bridgeOutputRig(t)
	before := cloneAgentState(s.agent.latest)
	candidate := cloneAgentState(before)
	candidate.History[0] = 1
	candidate.State.Bytes[0] = 1
	s.checkpoint.snapshotBudgetMu.Lock()
	s.checkpoint.snapshotBytesBudget = snapshotBucket{tokens: float64(s.snapshotLimits().BurstBytes), at: s.taskNow()}
	s.checkpoint.snapshotWriteInFlight = true
	s.checkpoint.snapshotBudgetMu.Unlock()
	_, err := s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: before, event: true})
	require.ErrorIs(t, err, errSnapshotAdmissionCapacity)
	require.Contains(t, err.Error(), "history transaction needs")
	t.Logf("same full candidate, in-flight write: %v", err)
	s.checkpoint.snapshotBudgetMu.Lock()
	s.checkpoint.snapshotWriteInFlight = false
	s.checkpoint.snapshotBudgetMu.Unlock()
	_, err = s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: before, event: true})
	require.NoError(t, err, "same encoded state and metadata fit after write settlement")
}

func TestBridgePrototypeOutputRollbackAndExactMove(t *testing.T) {
	s, a, run := bridgeOutputRig(t)
	packet := []byte{0xf8, 0, 0, 0, 1}
	result := agent.TaskResult{TaskID: run.ID(), Turn: 1, Sequence: 1, Outcome: agent.OK, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{packet}}}}
	// Reject Save without changing a public host limit or accepting the payload.
	a.state.Bytes = make([]byte, 3073)
	require.ErrorIs(t, s.acceptPrototypeOutput(prototypeOutputRequest{ctx: s.ctx, result: result}), agent.ErrCapacity)
	require.Empty(t, s.agent.v2.playback.queue)
	require.Empty(t, s.agent.v2.playback.payloads)
	a.state = v2State(s.agent.latest)
	require.NoError(t, s.acceptPrototypeOutput(prototypeOutputRequest{ctx: s.ctx, result: result}))
	require.Len(t, s.agent.v2.playback.queue, 1)
	require.Equal(t, 1, a.streamCallbacks-1)
	require.Zero(t, a.taskCallbacks)
	packet[1] = 99 // caller-owned bytes cannot mutate the accepted image
	first, err := s.saveV2MoveOnly(context.Background())
	require.NoError(t, err)
	second, err := s.saveV2MoveOnly(context.Background())
	require.NoError(t, err)
	require.Equal(t, first, second, "repeatable non-destructive exact export")
	require.Len(t, first.audio, 1)
	var retained int
	for _, task := range first.tasks {
		for _, r := range task.Audio {
			retained++
			require.Equal(t, byte(0), r.Chunk.Audio.Packets[0][1])
		}
	}
	require.Equal(t, 1, retained)
	// Restore the actual frozen task/audio image through the production restore.
	s.agent.v2.tasks = nil
	s.agent.v2.playback = playbackRuntime{}
	move := &movePayload{Tasks: first.tasks, Audio: first.audio}
	require.NoError(t, s.restoreTasks(move))
	require.NoError(t, s.restorePlayback(move))
	require.Len(t, s.agent.v2.playback.queue, 1)
	floor := agent.Turn(2)
	a.turn = 1
	// The ordinary interruption transaction fences subsequent old-turn effects.
	_, err = (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: "interrupt", Sequence: 1, Name: "interrupt"}}, eventLive)
	require.NoError(t, err)
	require.Equal(t, floor, s.agent.latest.V2.CancelFloor)
	require.Empty(t, s.agent.v2.playback.queue)
	result.Sequence = 2
	require.ErrorIs(t, s.acceptPrototypeOutput(prototypeOutputRequest{ctx: s.ctx, result: result}), context.Canceled)
}

func TestBridgeCatchupBatchFitsAcceptedEvent(t *testing.T) {
	for _, bound := range []int{10, 20} {
		t.Run(fmt.Sprint(bound), func(t *testing.T) {
			s, a, run := bridgeOutputRig(t)
			packets := make([][]byte, bound)
			for i := range packets {
				packets[i] = make([]byte, 1200)
				packets[i][0] = 0xf8
			}
			// Actual Opus framing, normal TaskRun.Emit, unchanged 100 ms emission floor.
			done := make(chan error, 1)
			go func() {
				done <- run.Emit(s.ctx, 1, agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: packets}})
			}()
			s.acceptEmission(<-s.agent.v2.emissions)
			require.NoError(t, <-done)
			delivered, err := s.deliverTaskResult()
			require.NoError(t, err)
			require.True(t, delivered)
			require.Len(t, s.agent.v2.playback.queue, bound)
			result := agent.TaskResult{TaskID: run.ID(), Sequence: 1, Turn: 1, Outcome: agent.OK, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: packets}}}
			body, err := json.Marshal(result)
			require.NoError(t, err)
			rawSize, ok := chunkSize(result.Chunk, s.agent.v2.registration.Bounds.MaxMailboxBytes)
			require.True(t, ok)
			t.Logf("%d x 1200 B: chunk=%d JSON=%d accounted mailbox=%d audio refs=%d", bound, rawSize, len(body), taskResultBytes(result), taskAudioRefBytes(result))
			require.Equal(t, 1, a.taskCallbacks)
			require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))

		})
	}
}

// A callback gets an owned copy even though stream payloads are retained for
// exact export before that callback runs.
type bridgeMutatingOutput struct{ *bridgeThroughputAgent }

func (a *bridgeMutatingOutput) prototypeOutput(ctx context.Context, r agent.TaskResult) (agent.Effects, error) {
	r.Chunk.Audio.Packets[0][1] = 77
	return a.bridgeThroughputAgent.prototypeOutput(ctx, r)
}
func TestBridgePrototypeCallbackCannotMutateAcceptedAudio(t *testing.T) {
	s, a, run := bridgeOutputRig(t)
	s.agent.v2.instance = &bridgeMutatingOutput{a}
	r := agent.TaskResult{TaskID: run.ID(), Sequence: 1, Turn: 1, Outcome: agent.OK, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{{0xf8, 0, 0, 0, 1}}}}}
	require.NoError(t, s.acceptPrototypeOutput(prototypeOutputRequest{ctx: s.ctx, result: r}))
	saved, err := s.saveV2MoveOnly(context.Background())
	require.NoError(t, err)
	for _, task := range saved.tasks {
		for _, result := range task.Audio {
			require.Equal(t, byte(0), result.Chunk.Audio.Packets[0][1])
		}
	}
}

type bridgeFailingSave struct{ *bridgeThroughputAgent }

func (a *bridgeFailingSave) Save(context.Context) (agent.State, error) {
	return agent.State{}, agent.ErrVersion
}
func TestBridgePrototypeSaveFailureStopsActor(t *testing.T) {
	s, a, run := bridgeOutputRig(t)
	s.worker.log = s.worker.cfg.LoggerFactory.NewLogger("bridge-prototype-test")
	s.agent.v2.instance = &bridgeFailingSave{a}
	s.agent.v2.prototypeOutput = make(chan prototypeOutputRequest)
	close(s.agent.v2.ready)
	done := make(chan struct{})
	go func() { defer close(done); s.v2AgentLoop() }()
	defer func() { s.cancel(); <-done; s.worker.running.Wait() }()
	r := agent.TaskResult{TaskID: run.ID(), Sequence: 1, Turn: 1, Outcome: agent.OK, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{{0xf8, 0, 0, 0, 1}}}}}
	require.Error(t, s.submitPrototypeOutput(s.ctx, r))
	require.Eventually(t, func() bool { return s.ctx.Err() != nil }, time.Second, time.Millisecond)
}

func TestBridgeTaskSlotPressureQueuesUntilCancellation(t *testing.T) {
	started := make(chan *agent.TaskRun, 6)
	rig := newAcceptedEventRig(t, agenthost.Bounds{}, map[string]agent.TaskHandler{"wait/v1": func(ctx context.Context, run *agent.TaskRun) error { started <- run; <-ctx.Done(); return ctx.Err() }})
	var effects agent.Effects
	for i := range 6 {
		effects.StartTasks = append(effects.StartTasks, agent.TaskSpec{Step: fmt.Sprint(i), Kind: "wait/v1", Class: agent.ReadOnly})
	}
	rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) { return effects, nil }
	_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: "start"}, eventLive)
	require.NoError(t, err)
	require.Len(t, rig.s.agent.v2.tasks, 6)
	rig.s.launchTasks()
	t.Cleanup(rig.s.stopTaskAttempts)
	var first *agent.TaskRun
	for range 4 {
		select {
		case run := <-started:
			if first == nil {
				first = run
			}
		case <-time.After(time.Second):
			t.Fatal("four task slots did not fill")
		}
	}
	select {
	case <-started:
		t.Fatal("fifth task bypassed the slot cap")
	default:
	}
	effects = agent.Effects{CancelTasks: []agent.TaskID{first.ID()}}
	_, err = (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: "release"}, eventLive)
	require.NoError(t, err)
	rig.s.launchTasks()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not release a slot")
	}
}

type bridgeFailingOutput struct{ *bridgeThroughputAgent }

func (a *bridgeFailingOutput) prototypeOutput(context.Context, agent.TaskResult) (agent.Effects, error) {
	return agent.Effects{}, agent.ErrVersion
}
func TestBridgePrototypeCallbackFailureStopsActor(t *testing.T) {
	s, a, run := bridgeOutputRig(t)
	s.worker.log = s.worker.cfg.LoggerFactory.NewLogger("bridge-prototype-test")
	s.agent.v2.instance = &bridgeFailingOutput{a}
	s.agent.v2.prototypeOutput = make(chan prototypeOutputRequest)
	close(s.agent.v2.ready)
	done := make(chan struct{})
	go func() { defer close(done); s.v2AgentLoop() }()
	defer func() { s.cancel(); <-done; s.worker.running.Wait() }()
	r := agent.TaskResult{TaskID: run.ID(), Sequence: 1, Turn: 1, Outcome: agent.OK, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{{0xf8, 0, 0, 0, 1}}}}}
	require.Error(t, s.submitPrototypeOutput(s.ctx, r))
	require.Eventually(t, func() bool { return s.ctx.Err() != nil }, time.Second, time.Millisecond)
}
