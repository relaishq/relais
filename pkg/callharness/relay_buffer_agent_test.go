package callharness

import (
	"context"
	"testing"
	"time"

	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/agent"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type replayTrackingAgent struct {
	*harnessCounterAgent
	inputs chan uint64
}

func (a *replayTrackingAgent) Process(ctx context.Context, in agent.Input) (agent.Output, error) {
	out, err := a.harnessCounterAgent.Process(ctx, in)
	if err == nil {
		a.inputs <- in.Progress.Index
	}
	return out, err
}

func TestAgentRelayBufferReplaysQueuedCheckpointAudio(t *testing.T) {
	relayBufferStores(t, func(t *testing.T, store sessionstore.Store) {
		resumes := make(chan agentResumeObservation, 8)
		inputs := make(chan uint64, 512)
		configs := []mediaworker.AgentConfig{
			{Factory: func(string) agent.Agent {
				return &harnessCounterAgent{resumes: resumes, processDelay: 35 * time.Millisecond}
			}},
			{QueueCapacity: 128, Factory: func(string) agent.Agent {
				return &replayTrackingAgent{harnessCounterAgent: &harnessCounterAgent{resumes: resumes}, inputs: inputs}
			}},
		}
		sys := startBufferHarnessWithAgents(t, store, relay.Config{Owners: store, Buffer: &relay.BufferConfig{}}, configs)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{InitialSequenceNumbers: &RTPSequenceNumbers{Audio: 1000}})
		require.NoError(t, err)
		sent := make(chan error, 1)
		go func() { sent <- call.SendMedia(ctx, 3*time.Second) }()
		time.Sleep(time.Second)
		sys.h.RecordProcessKill("0", time.Now())
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		state, err := store.GetState(ctx, call.SessionID())
		require.NoError(t, err)
		transport, err := mediaworker.SnapshotInboundIndexes(state)
		require.NoError(t, err)
		floors, err := mediaworker.SnapshotReplayIndexes(state)
		require.NoError(t, err)
		var notice agent.ResumeNotice
		select {
		case observation := <-resumes:
			notice = observation.notice
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		require.Equal(t, agent.Takeover, notice.Kind)
		require.True(t, notice.InputMayBeDuplicated)
		require.NotEmpty(t, notice.DuplicateWindows)
		require.Equal(t, floors[notice.Progress.SSRC], notice.Progress.Index)
		high := transport[notice.Progress.SSRC]
		require.Greater(t, high, notice.Progress.Index, "exercise transport input queued but not yet consumed at checkpoint")
		require.NoError(t, <-sent)
		decryptFailures, err := sys.workers[1].SessionDecryptFailures(call.SessionID())
		require.NoError(t, err)
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		require.Zero(t, decryptFailures)
		require.Zero(t, report.DecryptionFailures.Total())
		require.Zero(t, sys.workers[1].AgentStats().InputDrops)
		seen := make(map[uint64]int)
		var previous uint64
	drain:
		for {
			select {
			case index := <-inputs:
				require.Greater(t, index, previous)
				previous = index
				seen[index]++
			default:
				break drain
			}
		}
		for index := notice.Progress.Index + 1; index <= high; index++ {
			require.Equal(t, 1, seen[index], "queued checkpoint input %d must be consumed exactly once on target", index)
		}
		t.Logf("AGENT_REPLAY queued_floor=%d transport_high=%d queued_recovered=%d target_inputs=%d decrypt_failures=0", notice.Progress.Index, high, high-notice.Progress.Index, len(seen))
	})
}
