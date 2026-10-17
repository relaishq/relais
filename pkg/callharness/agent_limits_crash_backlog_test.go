package callharness

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Make Lost-before-cleanup observable deterministically, as permitted by the
// control contract. No extra delay is applied to normal worker releases.
type delayedReleaseStore struct {
	sessionstore.Store
	armed atomic.Bool
}

func (s *delayedReleaseStore) Release(ctx context.Context, l sessionstore.Lease) error {
	if s.armed.Load() {
		select {
		case <-time.After(150 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.Release(ctx, l)
}
func TestCrashLosesOversizedBacklog(t *testing.T) {
	if testing.Short() {
		t.Skip("large queued audio takeover on both stores; unit crash-bound checks cover the fast path")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		// Queue 240 KB of valid fixture packets under one result/reference span.
		fixture := pacerFixtures(t, 1)[0]
		packets := make([][]byte, 1500)
		for i := range packets {
			packets[i] = fixture
		}
		// A fixture packet is small. Replicate enough valid packets for >128 KiB.
		for len(packets)*len(fixture) < 240000 {
			packets = append(packets, fixture)
		}
		regs, events, gate, _ := reviewStreamRegs(packets, len(packets), 0)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, snapshotLimits: []mediaworker.SnapshotLimits{{MoveBytesPerSession: 1 << 20}, {MoveBytesPerSession: 128 << 10}}})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		close(gate)
		require.Eventually(t, func() bool {
			data, err := store.GetState(ctx, call.SessionID())
			if err != nil {
				return false
			}
			var snap struct {
				State struct {
					Agent struct {
						V2 struct {
							Playback struct {
								Length int `json:"l"`
							} `json:"pb"`
						}
					}
				}
			}
			return json.Unmarshal(data, &snap) == nil && snap.State.Agent.V2.Playback.Length > 128<<10
		}, 2*time.Second, 5*time.Millisecond)
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		require.Eventually(t, func() bool { return sys.workers[1].SessionCount() == 1 }, 4*time.Second, 5*time.Millisecond, "lost audio must not consume the target's move ceiling")
		got := awaitPacerEvent(t, events[1], agent.Resumed)
		require.Equal(t, agent.OutcomeUnknown, got.Resume.MoveOnly)
		require.NoError(t, call.SendMedia(ctx, 100*time.Millisecond))
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
