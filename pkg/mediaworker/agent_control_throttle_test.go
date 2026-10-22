package mediaworker

import (
	"context"
	"fmt"
	"testing"

	"github.com/relaishq/relais/internal/agenthost"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

// Audio pressure must not arm the independent receipt retry delays.
func TestControlAudioBackpressureDoesNotThrottle(t *testing.T) {
	for _, source := range []string{"mailbox", "playback"} {
		t.Run(source, func(t *testing.T) {
			bounds := agenthost.Bounds{}
			copies := 2
			if source == "mailbox" {
				bounds.MaxMailboxBytes = 4096
				copies = 64
			} else {
				bounds.MaxPendingBytes = 900
			}
			rig, runs := startAcceptedSpeech(t, bounds, [][]byte{{0xf8, 0xff, 0xfe}})
			for i := uint64(1); i <= 2; i++ {
				_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: fmt.Sprint(i), Sequence: i, Name: "x"}}, eventLive)
				require.NoError(t, err)
			}
			pressure := true
			rig.app.handle = func(_ context.Context, e agent.Event) (agent.Effects, error) {
				effects := agent.Effects{}
				require.Equal(t, agent.TaskMessage, e.Kind)
				require.Equal(t, runs[0].ID(), e.Result.TaskID)
				require.EqualValues(t, 1, e.Result.Sequence)
				if pressure {
					for range copies {
						effects.Audio = append(effects.Audio, agent.Audio{Turn: e.Result.Turn, Source: agent.AudioRef{TaskID: e.Result.TaskID, Sequence: e.Result.Sequence}})
					}
				}
				return effects, nil
			}
			// Emit has already admitted this real result. Submit the actor event
			// directly so the acceptance interface exposes its pressure error.
			result := agent.TaskResult{TaskID: runs[0].ID(), Sequence: 1, Turn: 1, Outcome: agent.OK, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{{0xf8, 0xff, 0xfe}}}}}
			_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.TaskMessage, ID: agent.EventID("task/" + string(result.TaskID) + "/1"), Result: &result}, eventLive)
			require.ErrorIs(t, err, agent.ErrBackpressure)
			status, err := rig.s.worker.agentSessionStatus(rig.s.id)
			require.NoError(t, err)
			require.Zero(t, status.PlaybackPackets)
			require.Zero(t, status.Tasks[0].Cursor)
			count := len(rig.app.events)
			pressure = false
			delivered, err := rig.s.deliverTaskResult()
			require.NoError(t, err, "audio pressure must not arm receipt throttle")
			require.True(t, delivered, "audio pressure must not arm receipt throttle")
			require.Len(t, rig.app.events, count+1)
			status, err = rig.s.worker.agentSessionStatus(rig.s.id)
			require.NoError(t, err)
			require.EqualValues(t, 1, status.Tasks[0].Cursor)
		})
	}
}

func TestControlReceiptThrottleKeepsIndependentDelays(t *testing.T) {
	rig := newAcceptedEventRig(t, agenthost.Bounds{MaxPendingBytes: 900}, map[string]agent.TaskHandler{"wait/v1": func(context.Context, *agent.TaskRun) error { return nil }})
	effects := agent.Effects{}
	rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) { return effects, nil }
	_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started, ID: "started"}, eventLive)
	require.NoError(t, err)
	for i := uint64(1); i <= 2; i++ {
		_, err = (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: fmt.Sprint(i), Sequence: i, Name: "x"}}, eventLive)
		require.NoError(t, err)
	}
	for i := range 10 {
		effects.StartTasks = append(effects.StartTasks, agent.TaskSpec{Step: fmt.Sprint(i), Kind: "wait/v1", Class: agent.ReadOnly})
	}
	first := agent.Event{Kind: agent.Started, ID: "fan/first"}
	second := agent.Event{Kind: agent.Started, ID: "fan/second"}
	_, err = (acceptedEvents{rig.s}).submit(first, eventLive)
	require.ErrorIs(t, err, agent.ErrBackpressure)
	count := len(rig.app.events)
	_, err = (acceptedEvents{rig.s}).submit(second, eventLive)
	require.ErrorIs(t, err, agent.ErrBackpressure)
	require.Len(t, rig.app.events, count+1, "a different event has its own first attempt")
	for _, event := range []agent.Event{first, second} {
		_, err = (acceptedEvents{rig.s}).submit(event, eventLive)
		require.ErrorIs(t, err, agent.ErrBackpressure)
	}
	require.Len(t, rig.app.events, count+1, "both blocked events retain independent delays")
}
