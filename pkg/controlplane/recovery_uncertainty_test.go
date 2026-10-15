package controlplane_test

import (
	"context"
	"crypto/rand"
	"errors"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/relaishq/relais/internal/workerprobe"
	"github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func forHTTPRecoveryStores(t *testing.T, scenario func(*testing.T, sessionstore.Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { scenario(t, sessionstore.NewMemory()) })
	t.Run("redis", func(t *testing.T) {
		addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
		if addr == "" {
			if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
				t.Fatal("dedicated Redis required")
			}
			t.Skip("dedicated Redis not configured")
		}
		require.NoError(t, redisendpoint.Validate(addr))
		store, err := sessionstore.NewRedis(context.Background(), storage.RedisConfig{Addr: addr, Prefix: "recovery-uncertainty:" + rand.Text() + ":"}, make([]byte, 32))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		scenario(t, store)
	})
}

// Lose one committed transfer and its reconciliation read. The real Move
// then retains its final export for recovery through takeoverLocked.
type retainedHTTPMoveStore struct {
	sessionstore.Store
	armed    atomic.Bool
	failRead atomic.Bool
}

func (s *retainedHTTPMoveStore) Transfer(ctx context.Context, from sessionstore.Lease, to netip.AddrPort, ttl time.Duration) (sessionstore.Lease, error) {
	lease, err := s.Store.Transfer(ctx, from, to, ttl)
	if err != nil || !s.armed.CompareAndSwap(true, false) {
		return lease, err
	}
	s.failRead.Store(true)
	return sessionstore.Lease{}, &sessionstore.TransientError{Op: "transfer", Err: errors.New("lost committed transfer reply"), Candidate: &lease}
}

func (s *retainedHTTPMoveStore) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	if deadline, bounded := ctx.Deadline(); bounded && time.Until(deadline) <= 500*time.Millisecond && s.failRead.CompareAndSwap(true, false) {
		return sessionstore.Lease{}, &sessionstore.TransientError{Op: "get", Err: errors.New("reconciliation unavailable")}
	}
	return s.Store.Get(ctx, id)
}

func (*retainedHTTPMoveStore) Settle(context.Context, sessionstore.Lease) (sessionstore.Lease, bool, error) {
	return sessionstore.Lease{}, false, &sessionstore.TransientError{Op: "settle", Err: errors.New("settlement unavailable")}
}

func TestHTTPRecoveryUncertainAgentCauseConfirmsTarget(t *testing.T) {
	for _, planned := range []bool{true, false} {
		kind := "takeover"
		if planned {
			kind = "move"
		}
		t.Run(kind, func(t *testing.T) {
			for _, workers := range []int{2, 3} {
				t.Run("workers="+strconv.Itoa(workers), func(t *testing.T) {
					forHTTPRecoveryStores(t, func(t *testing.T, store sessionstore.Store) {
						fault := &retainedHTTPMoveStore{Store: store}
						target := &lostReply{path: "/resume", failure: agent.ErrRestore}
						s := newHTTPSystemWithSettings(t, nil, target, nil, httpSystemSettings{
							holdTimeout: 200 * time.Millisecond,
							store:       fault,
							agent: mediaworker.AgentConfig{Factory: func(string) agent.Agent {
								return &lostReplyAudioAgent{}
							}},
						}, workers)
						if workers == 3 {
							// B owns the uncertain transfer. Give C equal load so
							// the first recovery selects B, with C still eligible.
							load, err := store.Claim(context.Background(), "load-on-c", s.workers[2].LocalAddr(), time.Minute)
							require.NoError(t, err)
							t.Cleanup(func() { require.NoError(t, store.Release(context.Background(), load)) })
						}
						var targetAudio atomic.Uint64
						audioReady := make(chan struct{})
						var ready sync.Once
						require.NoError(t, workerprobe.SetAfterEcho(s.workers[1].LocalAddr(), func(_ context.Context, _ string, packet []byte) {
							var header rtp.Header
							if _, err := header.Unmarshal(packet); err == nil && header.PayloadType == 111 && targetAudio.Add(1) >= 10 {
								ready.Do(func() { close(audioReady) })
							}
						}))
						var audioBeforeError atomic.Bool
						target.afterCommit = func() {
							select {
							case <-audioReady:
								audioBeforeError.Store(true)
							case <-time.After(time.Second):
							}
						}
						report := flowingCall(t, s, func(ctx context.Context, call *callharness.Call) {
							target.armed.Store(true)
							if planned {
								fault.armed.Store(true)
								_, err := s.plane.Move(ctx, call.SessionID(), "1")
								require.ErrorIs(t, err, sessionstore.ErrTransient)
							} else {
								require.NoError(t, workerprobe.Kill(s.workers[0].LocalAddr()))
							}
							require.Eventually(t, func() bool { return target.dropped.Load() && audioBeforeError.Load() }, 2*time.Second, time.Millisecond,
								"target must emit audio before returning the mixed transport error")
							require.Eventually(t, func() bool {
								status, err := s.plane.Status(ctx)
								return err == nil && (status.LostCount > 0 || len(status.Calls) == 1 && status.Calls[0].LastMoveKind == kind)
							}, 3*time.Second, time.Millisecond)
							require.True(t, audioBeforeError.Load(), "target emitted audio before the mixed transport error")
							if planned {
								require.EqualValues(t, 1, s.relay.Stats().HoldTimeouts)
							}
							status, err := s.plane.Status(ctx)
							require.NoError(t, err)
							require.Zero(t, status.LostCount, "the adopted target must not be declared lost")
							require.Len(t, status.Calls, 1)
							require.Equal(t, "1", status.Calls[0].Owner, "confirm B instead of adopting stale planned state on C")
							resumes := target.requests()
							require.Len(t, resumes, 2, "confirm the adopted B rather than excluding it")
							require.Equal(t, resumes[0].Lease.Epoch, resumes[1].Lease.Epoch)
							require.Equal(t, resumes[0].State, resumes[1].State)
							require.Equal(t, resumes[0].Kind, resumes[1].Kind)
							require.Equal(t, resumes[0].SequenceMargin, resumes[1].SequenceMargin)
							require.Equal(t, resumes[0].SRTCPIndexMargin, resumes[1].SRTCPIndexMargin)
							if planned {
								require.Equal(t, agent.PlannedMove, resumes[0].Kind)
								require.Zero(t, resumes[0].SequenceMargin)
								require.Zero(t, resumes[0].SRTCPIndexMargin)
							} else {
								require.Equal(t, agent.Takeover, resumes[0].Kind)
								require.GreaterOrEqual(t, resumes[0].SequenceMargin, uint16(8192))
								require.GreaterOrEqual(t, resumes[0].SRTCPIndexMargin, uint32(128))
							}
							if workers == 3 {
								require.Zero(t, s.workers[2].SessionCount(), "C must not adopt an export whose indexes B has used")
							}
						})
						require.True(t, target.dropped.Load())
						require.Zero(t, report.DecryptionFailures.Total())
					})
				})
			}
		})
	}
}
