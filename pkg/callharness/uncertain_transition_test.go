package callharness_test

import (
	"context"
	"errors"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// The underlying Redis executes Transfer. Discard its committed reply and
// fail settlement plus the first consumer read: the public Store boundary
// models the store-level network hook without exposing a production test API.
type uncertainTransitionStore struct {
	sessionstore.Store
	mu         sync.Mutex
	armed      bool
	failRead   bool
	candidate  *sessionstore.Lease
	failSettle int
}

func (s *uncertainTransitionStore) Transfer(ctx context.Context, from sessionstore.Lease, to netip.AddrPort, ttl time.Duration) (sessionstore.Lease, error) {
	lease, err := s.Store.Transfer(ctx, from, to, ttl)
	if err != nil {
		return lease, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.armed {
		return lease, nil
	}
	s.armed = false
	s.failRead = true
	s.candidate = &lease
	return sessionstore.Lease{}, &sessionstore.TransientError{Op: "transfer", Err: errors.New("EOF: dropped reply and failed settle"), Candidate: &lease}
}
func (s *uncertainTransitionStore) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	s.mu.Lock()
	deadline, bounded := ctx.Deadline()
	fail := s.failRead && bounded && time.Until(deadline) < 6*time.Second
	if fail {
		s.failRead = false
	}
	s.mu.Unlock()
	if fail {
		return sessionstore.Lease{}, &sessionstore.TransientError{Op: "get", Err: errors.New("EOF: failed reconcile")}
	}
	return s.Store.Get(ctx, id)
}
func (s *uncertainTransitionStore) Settle(ctx context.Context, c sessionstore.Lease) (sessionstore.Lease, bool, error) {
	s.mu.Lock()
	fail := s.failSettle > 0
	if fail {
		s.failSettle--
	}
	s.mu.Unlock()
	if fail {
		return sessionstore.Lease{}, false, &sessionstore.TransientError{Op: "settle", Err: errors.New("EOF: failed consumer settlement")}
	}
	return s.Store.(sessionstore.TransitionResolver).Settle(ctx, c)
}
func TestRelayUncertainTransferSurvives(t *testing.T) {
	for _, kind := range []string{"move", "takeover", "deferred-move", "deferred-takeover"} {
		t.Run(kind, func(t *testing.T) {
			forSessionStores(t, func(t *testing.T, store sessionstore.Store) {
				if store == nil {
					t.Skip("network uncertainty uses the real Redis store")
				}
				fault := &uncertainTransitionStore{Store: store}
				if kind == "deferred-move" || kind == "deferred-takeover" {
					fault.failSettle = 1
				}
				h := startHarness(t, callharness.Options{Relay: true, Workers: 2, SessionStore: fault})
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				call, err := h.Dial(ctx, callharness.CallOptions{Video: true})
				require.NoError(t, err)
				sent := make(chan error, 1)
				go func() { sent <- call.SendMedia(ctx, 4*time.Second) }()
				time.Sleep(time.Second)
				fault.mu.Lock()
				fault.armed = true
				fault.mu.Unlock()
				if kind == "move" || kind == "deferred-move" {
					moveErr := call.Handover(callharness.HandoverOptions{To: 1})
					if kind == "deferred-move" {
						require.Error(t, moveErr)
						require.Eventually(t, func() bool {
							s, e := h.Status(ctx)
							return e == nil && len(s.Calls) == 1 && s.Calls[0].LastMoveKind == "move"
						}, time.Second, time.Millisecond)
					} else {
						require.NoError(t, moveErr)
					}
				} else {
					require.NoError(t, h.Kill(0))
					waitTakeovers(t, ctx, h, 1)
				}
				require.NoError(t, <-sent)
				status, err := h.Status(ctx)
				require.NoError(t, err)
				require.Len(t, status.Calls, 1)
				require.Equal(t, "1", status.Calls[0].Owner)
				if kind == "move" || kind == "deferred-move" {
					require.Equal(t, "move", status.Calls[0].LastMoveKind)
					require.Zero(t, status.Calls[0].TakeoverCount)
					require.Empty(t, status.Takeovers)
				}
				fault.mu.Lock()
				candidate := fault.candidate
				fault.mu.Unlock()
				require.NotNil(t, candidate)
				require.Equal(t, candidate.Epoch, status.Calls[0].Epoch)
				report, err := call.Hangup(ctx)
				require.NoError(t, err)
				assertCleanCall(t, report)
				assertVideoDecodes(t, report)
			})
		})
	}
}
