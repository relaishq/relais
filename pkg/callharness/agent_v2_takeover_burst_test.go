package callharness

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

const slowRestoreDelay = 40 * time.Millisecond

type slowRestoreAgent struct{}

func (*slowRestoreAgent) Handle(context.Context, agent.Event) (agent.Effects, error) {
	return agent.Effects{}, nil
}
func (*slowRestoreAgent) Save(context.Context) (agent.State, error) {
	return agent.State{Version: 1, Bytes: []byte("s")}, nil
}
func (*slowRestoreAgent) Restore(context.Context, agent.State) error {
	time.Sleep(slowRestoreDelay) // within the 100 ms callback budget
	return nil
}

func TestV2TakeoverBurst(t *testing.T) {
	if testing.Short() {
		t.Skip("extended v2 matrix runs outside the PR short suite")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		reg := agenthost.Registration{Name: "burst/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &slowRestoreAgent{} }}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}})
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for i := 0; i < 40 && sys.workers[0].SessionCount() < 14; i++ {
			_, err := sys.h.Dial(ctx, CallOptions{})
			require.NoError(t, err)
		}
		n := sys.workers[0].SessionCount()
		t.Logf("sessions on worker 0: %d, worker 1: %d", n, sys.workers[1].SessionCount())
		time.Sleep(500 * time.Millisecond)
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		var lost, ok int
		var cause string
		require.Eventually(t, func() bool {
			status, err := sys.plane.Status(ctx)
			if err != nil {
				return false
			}
			lost, ok = 0, 0
			for _, tk := range status.Takeovers {
				if tk.Lost {
					lost++
					cause = tk.Error
				} else {
					ok++
				}
			}
			return lost+ok >= n
		}, 30*time.Second, 50*time.Millisecond)
		require.Zero(t, lost, "14-session takeover burst must not lose calls: %s", cause)
		require.Equal(t, n, ok)
		require.Equal(t, 14, n)
	})
}
