package mediaworker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

// Fan-out receipt pressure check. Static timers cannot release this
// pressure; acknowledged receipts must compact in the accepting transaction.
func TestControlAcknowledgedReceiptsReleaseAdmission(t *testing.T) {
	for _, width := range []int{18, 19} {
		for _, held := range []int{0, 2} {
			t.Run(fmt.Sprintf("width%d/held%d", width, held), func(t *testing.T) {
				a := &v2UnitAgent{state: agent.State{Version: 1}}
				_, s := v2UnitSession(t, a, func(c *AgentConfig) { c.CallbackTimeout = 5 * time.Second })
				require.NoError(t, initControlUnit(s))
				s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"wait/v1": func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return nil }}
				for i := 0; i < 8; i++ {
					a.effects.SetTimers = append(a.effects.SetTimers, agent.TimerSpec{ID: fmt.Sprint(i) + strings.Repeat("t", 40), After: time.Hour})
				}
				require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started, ID: "timers"}))
				a.effects = agent.Effects{}
				for i := 1; i <= held; i++ {
					_, err := s.processControl(agent.ControlMessage{ID: fmt.Sprint(i), Sequence: uint64(i), Name: "x"})
					require.NoError(t, err)
					s.controlAck = uint64(i)
				}
				for i := 0; i < width; i++ {
					a.effects.StartTasks = append(a.effects.StartTasks, agent.TaskSpec{Step: fmt.Sprintf("s%02d", i), Kind: "wait/v1", Class: agent.ReadOnly})
				}
				before := a.handles
				require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started, ID: "fan"}))
				require.Equal(t, before+1, a.handles, "no recursive Handle retry")
				require.Equal(t, width, len(s.agent.v2.tasks)+len(s.agent.latest.V2.Capacity))
				if held > 0 {
					require.Empty(t, s.agent.latest.V2.Controls)
					require.EqualValues(t, held, s.agent.latest.V2.ControlFloor)
					require.EqualValues(t, held, s.agent.latest.V2.ControlHigh)
					_, err := s.processControl(agent.ControlMessage{ID: "1", Sequence: 1, Name: "x"})
					require.ErrorIs(t, err, agent.ErrControlStale)
				}
			})
		}
	}
}

func TestControlSnapshotVersion(t *testing.T) {
	golden, err := os.ReadFile("testdata/v7-787127e.json")
	require.NoError(t, err)
	for _, controls := range []bool{false, true} {
		for _, refusals := range []bool{false, true} {
			name := fmt.Sprintf("control%v-refusals%v", controls, refusals)
			t.Run(name, func(t *testing.T) {
				snap, err := decodeSnapshot(golden)
				require.NoError(t, err)
				snap.Version = 8
				snap.State.Version = 8
				snap.State.Agent.V2 = &v2Envelope{Name: "unit/v1", Contract: v2Contract, Started: true}
				if controls {
					snap.State.Agent.V2.ControlHigh = 1
					snap.State.Agent.V2.Controls = []controlRecord{{ID: controlID("a"), Sequence: 1, Digest: controlID("b")}}
				}
				if refusals {
					snap.State.Agent.V2.Capacity = []taskRefusal{{ID: deriveTaskID("s", "n", "e", "a"), Turn: 3}}
				}
				wire, err := json.Marshal(snap)
				require.NoError(t, err)
				if dir := os.Getenv("CONTROL_SNAPSHOT_FIXTURES"); dir != "" {
					require.NoError(t, os.WriteFile(filepath.Join(dir, name+".json"), wire, 0600))
				}
				var version struct {
					Version int
					State   struct{ Version int }
				}
				require.NoError(t, json.Unmarshal(wire, &version))
				want := 8
				if controls {
					want = 9
				}
				require.Equal(t, want, version.Version)
				require.Equal(t, want, version.State.Version)
				decoded, err := decodeSnapshot(wire)
				require.NoError(t, err)
				require.Equal(t, snap.State.Agent, decoded.State.Agent)
			})
		}
	}
}

// Simulate the actor's 10 ms wakeups while the store acknowledges nothing.
// CONTROL_STALL_DURATION=30s performs the longer stall measurement explicitly.
func TestControlUnacknowledgedAdmissionRetryCadence(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a, func(c *AgentConfig) { c.CallbackTimeout = 5 * time.Second })
	require.NoError(t, initControlUnit(s))
	s.agent.v2.registration.Bounds.MaxPendingBytes = 900
	for i := uint64(1); i <= 2; i++ {
		_, err := s.processControl(agent.ControlMessage{ID: fmt.Sprint(i), Sequence: i, Name: "x"})
		require.NoError(t, err)
	}
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"wait/v1": func(context.Context, *agent.TaskRun) error { return nil }}
	for i := 0; i < 10; i++ {
		a.effects.StartTasks = append(a.effects.StartTasks, agent.TaskSpec{Step: fmt.Sprint(i), Kind: "wait/v1", Class: agent.ReadOnly})
	}
	duration := time.Second
	if text := os.Getenv("CONTROL_STALL_DURATION"); text != "" {
		var err error
		duration, err = time.ParseDuration(text)
		require.NoError(t, err)
	}
	// This fixture has no ICE keepalive; keep consent alive during the long probe.
	s.consent.Reset(duration + time.Minute)
	before := cloneAgentState(s.state.Agent)
	handles, restores := a.handles, a.restores
	start := time.Now()
	for time.Since(start) < duration {
		require.ErrorIs(t, s.handleV2Event(agent.Event{Kind: agent.Started, ID: "fan"}), errTaskAdmissionBackpressure)
		time.Sleep(10 * time.Millisecond)
	}
	attempts := a.handles - handles
	t.Logf("stall=%s Handle=%d Restore=%d", time.Since(start), attempts, a.restores-restores)
	require.LessOrEqual(t, attempts, int(duration/(250*time.Millisecond))+1)
	require.Equal(t, before, s.state.Agent)
	s.controlAck = 2
	require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started, ID: "fan"}), "acknowledgement bypasses retry delay")
}

func TestControlAcknowledgedReceiptsReleaseEffects(t *testing.T) {
	for _, kind := range []agent.EventKind{agent.Started, agent.AudioInput, agent.Control} {
		t.Run(string(kind), func(t *testing.T) {
			a := &v2UnitAgent{state: agent.State{Version: 1}}
			_, s := v2UnitSession(t, a, func(c *AgentConfig) { c.CallbackTimeout = 5 * time.Second })
			require.NoError(t, initControlUnit(s))
			for i := uint64(1); i <= 2; i++ {
				_, err := s.processControl(agent.ControlMessage{ID: fmt.Sprint(i), Sequence: i, Name: "x"})
				require.NoError(t, err)
				s.controlAck = i
			}
			// Model adoption onto a worker with a lower effective operator ceiling.
			s.agent.v2.registration.Bounds.MaxPendingBytes = 550
			a.effects.SetTimers = []agent.TimerSpec{{ID: "timer", After: time.Hour}}
			if kind == agent.Control {
				_, err := s.processControl(agent.ControlMessage{ID: "3", Sequence: 3, Name: "x"})
				require.NoError(t, err)
			} else {
				event := agent.Event{Kind: kind, ID: "effect"}
				if kind == agent.AudioInput {
					event.Input = &legacy.Input{}
				}
				require.NoError(t, s.handleV2Event(event))
			}
			require.Len(t, s.agent.latest.V2.Timers, 1)
			require.Greater(t, s.agent.latest.V2.ControlFloor, uint64(0))
		})
	}
}

func TestControlAcknowledgedReceiptsReleaseEmission(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a, func(c *AgentConfig) { c.CallbackTimeout = 5 * time.Second })
	require.NoError(t, initControlUnit(s))
	for i := uint64(1); i <= 2; i++ {
		_, err := s.processControl(agent.ControlMessage{ID: fmt.Sprint(i), Sequence: i, Name: "x"})
		require.NoError(t, err)
		s.controlAck = i
	}
	s.agent.v2.registration.Bounds.MaxPendingBytes = 550
	intent := taskIntent{ID: "emit", Class: agent.ReadOnly, Phase: taskStarted}
	task := &localTask{intent: intent, attempt: 1, launched: true, terminalBytes: taskTerminalBytes(intent)}
	s.agent.v2.tasks = []*localTask{task}
	e := taskEmission{id: "emit", attempt: 1, ctx: context.Background(), sequence: 1, chunk: agent.Chunk{Data: []byte("chunk")}, reply: make(chan error, 1)}
	s.acceptEmission(e)
	require.NoError(t, <-e.reply)
	require.Empty(t, s.agent.latest.V2.Controls)
	require.EqualValues(t, 2, s.agent.latest.V2.ControlFloor)
	require.Equal(t, s.agent.latest.V2.Controls, s.state.Agent.V2.Controls)
	require.Len(t, task.results, 1)
}

// This fixed wire fixture protects version selection independently of the writer.
func TestControlSnapshotFixture(t *testing.T) {
	wire, err := os.ReadFile("testdata/v9-control.json")
	require.NoError(t, err)
	snap, err := decodeSnapshot(wire)
	require.NoError(t, err)
	require.Equal(t, 9, snap.Version)
	require.Equal(t, 9, snap.State.Version)
	require.EqualValues(t, 1, snap.State.Agent.V2.ControlHigh)
	require.Len(t, snap.State.Agent.V2.Controls, 1)
	encoded, err := json.Marshal(snap)
	require.NoError(t, err)
	require.Equal(t, string(wire), string(encoded))
}
