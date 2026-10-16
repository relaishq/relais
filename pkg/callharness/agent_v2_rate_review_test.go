package callharness

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type reviewConstAgent struct{ audio atomic.Int64 }

func (a *reviewConstAgent) Handle(_ context.Context, e agent.Event) (agent.Effects, error) {
	if e.Kind == agent.AudioInput {
		a.audio.Add(1)
	}
	return agent.Effects{}, nil
}
func (*reviewConstAgent) Save(context.Context) (agent.State, error) {
	return agent.State{Version: 1, Bytes: []byte("constant")}, nil
}
func (*reviewConstAgent) Restore(context.Context, agent.State) error { return nil }

type reviewCountStore struct {
	sessionstore.Store
	puts atomic.Int64
}

func (s *reviewCountStore) PutState(ctx context.Context, lease sessionstore.Lease, data []byte) error {
	s.puts.Add(1)
	return s.Store.PutState(ctx, lease, data)
}

func TestV2ReviewConstantStateCheckpointRate(t *testing.T) {
	forAgentStores(t, func(t *testing.T, base sessionstore.Store) {
		var counts [2]int64
		for i, mode := range []string{"v2-const", "legacy-echo"} {
			store := &reviewCountStore{Store: base}
			a := &reviewConstAgent{}
			settings := agentHarnessSettings{echo: true}
			if i == 0 {
				reg := agenthost.Registration{Name: "const/v1", Factory: func(agent.FactoryArgs) agent.Agent { return a }}
				settings = agentHarnessSettings{v2: []agenthost.Registration{reg, reg}}
			}
			sys := startAgentHarness(t, store, settings)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			call, err := sys.h.Dial(ctx, CallOptions{})
			require.NoError(t, err)
			time.Sleep(300 * time.Millisecond)
			before := store.puts.Load()
			require.NoError(t, call.SendMedia(ctx, 2*time.Second))
			counts[i] = store.puts.Load() - before
			if i == 0 {
				require.GreaterOrEqual(t, a.audio.Load(), int64(90))
			}
			_, err = call.Hangup(ctx)
			require.NoError(t, err)
			cancel()
			t.Logf("%s checkpoint writes=%d", mode, counts[i])
		}
		require.InDelta(t, counts[1], counts[0], 6, "constant-state v2 must use the Echo checkpoint cadence")
	})
}
