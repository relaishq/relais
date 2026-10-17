package callharness

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type limitHarnessAgent struct {
	size      int
	oversized bool
	received  chan agent.TaskResult
	tasks     []agent.TaskSpec
}

func (a *limitHarnessAgent) Handle(_ context.Context, e agent.Event) (agent.Effects, error) {
	switch e.Kind {
	case agent.Started:
		return agent.Effects{StartTasks: a.tasks}, nil
	case agent.AudioInput:
		if a.oversized {
			a.size = 1025
		}
	case agent.TaskMessage:
		a.received <- *e.Result
	}
	return agent.Effects{}, nil
}
func (a *limitHarnessAgent) Save(context.Context) (agent.State, error) {
	return agent.State{Version: 1, Bytes: make([]byte, a.size)}, nil
}
func (a *limitHarnessAgent) Restore(_ context.Context, state agent.State) error {
	a.size = len(state.Bytes)
	return nil
}

func TestV2LimitsStateRefusalContinues(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		received := make(chan agent.TaskResult, 64)
		reg := agenthost.Registration{Name: "limits/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &limitHarnessAgent{oversized: true, received: received} }}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}})
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		require.NoError(t, call.SendMedia(ctx, 120*time.Millisecond))
		select {
		case r := <-received:
			require.Equal(t, agent.Capacity, r.Outcome)
		case <-ctx.Done():
			t.Fatal("missing state refusal")
		}
		require.Equal(t, 1, sys.workers[0].SessionCount())
		_, err = sys.plane.Move(ctx, call.SessionID(), "1")
		require.NoError(t, err)
		require.NoError(t, call.SendMedia(ctx, 120*time.Millisecond))
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

type publicationStore struct {
	sessionstore.Store
	appends atomic.Int64
	last    uint64
}

func (s *publicationStore) PutState(ctx context.Context, l sessionstore.Lease, data []byte) error {
	// PutState is serialized per session. Count changes in the actual stored
	// publication cursor, rather than counting Emit calls or transport writes.
	var snap struct {
		State struct {
			Agent struct {
				V2 struct {
					Tasks []struct {
						Published uint64 `json:"n"`
					}
				}
			}
		}
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	err := s.Store.PutState(ctx, l, data)
	if err == nil && len(snap.State.Agent.V2.Tasks) > 0 {
		n := snap.State.Agent.V2.Tasks[0].Published
		if n > s.last {
			s.appends.Add(1)
			s.last = n
		}
	}
	return err
}
func TestV2LimitsEmitPublicationRate(t *testing.T) {
	if testing.Short() {
		t.Skip("one-second rate gate runs in the extended matrix; fast Emit spacing is covered in mediaworker")
	}
	forAgentStores(t, func(t *testing.T, base sessionstore.Store) {
		store := &publicationStore{Store: base}
		received := make(chan agent.TaskResult, 32)
		first := make(chan time.Duration, 1)
		reg := agenthost.Registration{Name: "emit-limits/v1", Factory: func(agent.FactoryArgs) agent.Agent {
			return &limitHarnessAgent{received: received, tasks: []agent.TaskSpec{{Step: "stream", Kind: "stream/v1", Class: agent.ReadOnly, Timeout: 3 * time.Second}}}
		}, Tasks: map[string]agent.TaskHandler{"stream/v1": func(ctx context.Context, r *agent.TaskRun) error {
			start := time.Now()
			for i := uint64(1); i <= 11; i++ {
				if err := r.Emit(ctx, i, agent.Chunk{Data: []byte("chunk")}); err != nil {
					return err
				}
				if i == 1 {
					first <- time.Since(start)
				}
			}
			return nil
		}}}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}})
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		select {
		case elapsed := <-first:
			require.Less(t, elapsed, 100*time.Millisecond)
		case <-ctx.Done():
			t.Fatal("first chunk delayed")
		}
		var previous time.Time
		chunks := 0
		for {
			select {
			case r := <-received:
				now := time.Now()
				if !previous.IsZero() {
					require.GreaterOrEqual(t, now.Sub(previous), 80*time.Millisecond)
				}
				previous = now
				if r.Final {
					require.Equal(t, agent.OK, r.Outcome)
					goto complete
				}
				chunks++
			case <-ctx.Done():
				t.Fatal("Emit wedged")
			}
		}
	complete:
		require.Equal(t, 11, chunks)
		require.LessOrEqual(t, store.appends.Load(), int64(12))
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
