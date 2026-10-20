package mediaworker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/componentcodec"
	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type movingHistoryAgent struct{ state agent.State }

func (a *movingHistoryAgent) Handle(_ context.Context, event agent.Event) (agent.Effects, error) {
	if event.Kind == agent.Control {
		a.state.Bytes = bytes.Repeat(event.Control.Data, len(a.state.Bytes))
		a.state.History = bytes.Repeat(event.Control.Data, 8192)
	}
	return agent.Effects{}, nil
}
func (a *movingHistoryAgent) Save(context.Context) (agent.State, error) {
	return agent.State{Version: 1, Bytes: bytes.Clone(a.state.Bytes), History: bytes.Clone(a.state.History)}, nil
}
func (a *movingHistoryAgent) Restore(_ context.Context, s agent.State) error {
	a.state = agent.State{Version: s.Version, Bytes: bytes.Clone(s.Bytes), History: bytes.Clone(s.History)}
	return nil
}

type heldComponentStore struct {
	*sessionstore.Memory
	armed                      atomic.Bool
	mu                         sync.Mutex
	entered, release, returned chan struct{}
}

func (s *heldComponentStore) CommitComponents(ctx context.Context, l sessionstore.Lease, c sessionstore.ComponentCommit) (sessionstore.ComponentTuple, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		defer close(s.returned)
		select {
		case <-s.release:
		case <-ctx.Done():
			return sessionstore.ComponentTuple{}, ctx.Err()
		}
	}
	return s.Memory.CommitComponents(ctx, l, c)
}

func TestPlannedMoveAndDrainCarryNearLimitHistoryWithInflightWriter(t *testing.T) {
	for _, work := range []int{3072, 6000} {
		for _, drain := range []bool{false, true} {
			t.Run(fmt.Sprintf("work-%d-drain-%t", work, drain), func(t *testing.T) {
				defer workerprobe.Enable()()
				store := &heldComponentStore{Memory: sessionstore.NewMemory(), entered: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
				defer close(store.release)
				router, err := relay.New(relay.Config{Owners: store})
				require.NoError(t, err)
				defer func() { require.NoError(t, router.Close()) }()
				limits := agent.Limits{}
				if work == 6000 {
					limits.MaxStateBytes = 6000
				}
				factory := agenthost.Factory(agenthost.Registration{Name: "moving-history/v1", Bounds: agenthost.Bounds(limits), Factory: func(agent.FactoryArgs) agent.Agent {
					return &movingHistoryAgent{state: agent.State{Version: 1, Bytes: bytes.Repeat([]byte{'w'}, work), History: bytes.Repeat([]byte{'a'}, 8192)}}
				}})
				cfg := mediaworker.Config{SplitCheckpoints: true, DisableFrameCache: true, Agent: mediaworker.AgentConfig{Factory: factory, CallbackTimeout: time.Second}, Relay: &mediaworker.RelayConfig{Owners: store, Addr: router.WorkerAddr(), PublicAddr: router.PublicAddr()}}
				source, err := mediaworker.New(cfg)
				require.NoError(t, err)
				defer func() { require.NoError(t, source.Close()) }()
				cfg.SplitCheckpoints = false
				target, err := mediaworker.New(cfg)
				require.NoError(t, err)
				defer func() { require.NoError(t, target.Close()) }()
				plane := controlplane.New(router, store)
				control := httptest.NewServer(plane.ProcessHandler())
				defer control.Close()
				router.AddWorker(source.LocalAddr())
				router.AddWorker(target.LocalAddr())
				require.NoError(t, plane.Register("0", source.LocalAddr(), source))
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				harness, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: control.URL + "/calls", RelayAddr: router.PublicAddr()}})
				require.NoError(t, err)
				defer harness.Close()
				call, err := harness.Dial(ctx, callharness.CallOptions{})
				require.NoError(t, err)
				require.NoError(t, plane.Register("1", target.LocalAddr(), target))
				require.Eventually(t, func() bool {
					tuple, e := store.ReadComponents(ctx, call.SessionID())
					if e != nil {
						return false
					}
					var tr componentcodec.TransportState
					if componentcodec.Unmarshal(tuple.Components.Transport, componentcodec.Transport, &tr) != nil {
						return false
					}
					var host struct{ Started bool }
					_ = json.Unmarshal(tr.Host, &host)
					return host.Started
				}, 2*time.Second, time.Millisecond)
				require.NoError(t, workerprobe.Pause(source.LocalAddr(), true))
				store.mu.Lock()
				store.mu.Unlock() //nolint:staticcheck // Synchronize with the prior commit before admitting history.
				// Refill the initial cold transaction's debit. This is not a callback
				// deadline assertion: every callback has a one-second budget.
				time.Sleep(250 * time.Millisecond)
				_, err = plane.AgentControl(ctx, call.SessionID(), agent.ControlMessage{ID: "new-history", Sequence: 1, Name: "remember", Data: []byte{'c'}})
				require.NoError(t, err)
				store.armed.Store(true)
				require.NoError(t, workerprobe.Pause(source.LocalAddr(), false))
				select {
				case <-store.entered:
				case <-ctx.Done():
					t.Fatal("writer did not enter the component store")
				}
				if drain {
					source.BeginDrain()
					require.NoError(t, source.QuiesceTasks(ctx))
					moves, e := plane.Drain(ctx, "0")
					require.NoError(t, e)
					require.Len(t, moves, 1)
				} else {
					_, err = plane.Move(ctx, call.SessionID(), "1")
					require.NoError(t, err)
				}
				select {
				case <-store.returned:
				case <-ctx.Done():
					t.Fatal("exported source writer did not finish")
				}
				tuple, err := store.ReadComponents(ctx, call.SessionID())
				require.NoError(t, err)
				require.Equal(t, target.LocalAddr(), tuple.Lease.Worker)
				require.Equal(t, target.LocalAddr().String(), tuple.Metadata.Owner)
				var working, history []byte
				require.NoError(t, componentcodec.Unmarshal(tuple.Components.Work, componentcodec.Work, &working))
				require.NoError(t, componentcodec.Unmarshal(tuple.Components.History, componentcodec.History, &history))
				require.Equal(t, bytes.Repeat([]byte{'c'}, work), working)
				require.Equal(t, bytes.Repeat([]byte{'c'}, 8192), history)
				require.NoError(t, call.SendMedia(ctx, 100*time.Millisecond))
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		}
	}
}
