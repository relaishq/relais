package controlplane_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The real export handler sees a closed session only after Move has read its
// live lease and held caller media. No transport error substitutes for export.
type beforeMoveExport struct{ before func(string) }

func (f *beforeMoveExport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/export") {
		f.before(strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/sessions/"), "/export"))
	}
	return http.DefaultTransport.RoundTrip(req)
}

type moveTenureCleanup struct {
	*relay.Relay
	before func(sessionstore.Lease)
	mu     sync.Mutex
	tokens []sessionstore.Lease
}

func (r *moveTenureCleanup) ForgetSessionTenure(ctx context.Context, lease sessionstore.Lease) error {
	if r.before != nil {
		r.before(lease)
	}
	if err := r.Relay.ForgetSessionTenure(ctx, lease); err != nil {
		return err
	}
	r.mu.Lock()
	r.tokens = append(r.tokens, lease)
	r.mu.Unlock()
	return nil
}

func (r *moveTenureCleanup) cleaned() []sessionstore.Lease {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sessionstore.Lease(nil), r.tokens...)
}

func TestMoveUnknownExportClassifiesExpiredAndEnded(t *testing.T) {
	for _, outcome := range []string{"expired", "expired-successor", "ended"} {
		t.Run(outcome, func(t *testing.T) {
			forHTTPRecoveryStores(t, func(t *testing.T, store sessionstore.Store) {
				s := newHTTPSystemWithSettings(t, nil, nil, nil, httpSystemSettings{store: store})
				cleanup := &moveTenureCleanup{Relay: s.relay}
				s.plane.SetRelay(cleanup)
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				call, err := s.harness.Dial(ctx, callharness.CallOptions{Worker: 0})
				require.NoError(t, err)
				require.NoError(t, call.SendMedia(ctx, 250*time.Millisecond))
				old, err := store.Get(ctx, call.SessionID())
				require.NoError(t, err)
				var successor sessionstore.Lease
				if outcome == "expired-successor" {
					// Runs on the plane's reconcile goroutine: assert, never require.
					cleanup.before = func(lost sessionstore.Lease) {
						assert.Equal(t, old.Worker, lost.Worker)
						assert.Equal(t, old.Epoch, lost.Epoch)
						next, claimErr := store.Claim(ctx, old.SessionID, s.workers[1].LocalAddr(), time.Minute)
						if assert.NoError(t, claimErr) {
							successor = next
							assert.NoError(t, store.PutState(ctx, successor, []byte("successor checkpoint")))
						}
					}
				}
				var exported atomic.Bool
				s.remote[0].Client = &http.Client{Transport: &beforeMoveExport{before: func(id string) {
					assert.Equal(t, old.SessionID, id)
					exported.Store(true)
					if outcome != "ended" {
						require.NoError(t, workerprobe.Pause(s.workers[0].LocalAddr(), true))
						_, err := store.Renew(ctx, old, time.Millisecond)
						require.NoError(t, err)
						require.Eventually(t, func() bool {
							_, err := store.Get(ctx, id)
							return errors.Is(err, sessionstore.ErrNotFound) && !errors.Is(err, sessionstore.ErrEnded)
						}, time.Second, time.Millisecond)
					}
					require.NoError(t, s.workers[0].EndSession(id))
				}}, Timeout: 2 * time.Second}
				moved, err := s.plane.Move(ctx, call.SessionID(), "1")
				require.ErrorIs(t, err, mediaworker.ErrUnknownSession)
				if outcome != "ended" {
					require.True(t, moved.Lost, "the move result reports the counted loss")
					require.Equal(t, "ownership_expired", moved.LossCause)
				}
				require.True(t, exported.Load(), "expire/end only after Move reaches export")
				status, err := s.plane.Status(ctx)
				require.NoError(t, err)
				require.Empty(t, status.Calls)
				losses := readMetrics(t, s.plane.ProcessHandler())
				if outcome == "ended" {
					require.Zero(t, status.LostCount)
					require.Empty(t, status.Takeovers)
					require.Zero(t, losses[`relais_control_losses_total{cause="ownership_expired"}`])
					require.Empty(t, cleanup.cleaned())
				} else {
					require.EqualValues(t, 1, status.LostCount)
					require.Len(t, status.Takeovers, 1)
					require.True(t, status.Takeovers[0].Lost)
					require.Equal(t, "ownership_expired", status.Takeovers[0].LossCause)
					require.EqualValues(t, 1, losses[`relais_control_losses_total{cause="ownership_expired"}`])
					require.Eventually(t, func() bool {
						return len(cleanup.cleaned()) > 0
					}, 3*time.Second, 10*time.Millisecond)
					tokens := cleanup.cleaned()
					require.Len(t, tokens, 1)
					require.Equal(t, old.Worker, tokens[0].Worker)
					require.Equal(t, old.Epoch, tokens[0].Epoch)
					if outcome == "expired-successor" {
						current, err := store.Get(ctx, old.SessionID)
						require.NoError(t, err)
						require.Equal(t, successor.Epoch, current.Epoch)
						state, err := store.GetState(ctx, old.SessionID)
						require.NoError(t, err)
						require.Equal(t, "successor checkpoint", string(state))
					} else {
						require.Eventually(t, func() bool { return readMetrics(t, s.relay.PrivateHandler())["relais_relay_active_routes"] == 0 }, time.Second, time.Millisecond)
					}
					status, err = s.plane.Status(ctx)
					require.NoError(t, err)
					require.EqualValues(t, 1, status.LostCount)
					require.Len(t, status.Takeovers, 1)
					require.EqualValues(t, 1, readMetrics(t, s.plane.ProcessHandler())[`relais_control_losses_total{cause="ownership_expired"}`])
				}
				for _, worker := range s.workers {
					metrics := readMetrics(t, worker.PrivateHandler())
					require.Contains(t, metrics, "relais_worker_decryption_failures_total")
					require.Zero(t, metrics["relais_worker_decryption_failures_total"])
					require.Zero(t, metrics["relais_worker_replay_rejections_total"])
					require.Zero(t, metrics["relais_worker_handovers_resumed_total"])
					require.Zero(t, metrics["relais_worker_takeovers_resumed_total"])
				}
				report, err := call.Hangup(ctx)
				require.EqualError(t, err, "callharness: DELETE call: 404 Not Found", "terminal call is already forgotten")
				require.Positive(t, report.Track("audio").Packets)
				require.Zero(t, report.DecryptionFailures.Total())
				// Caller history retains the same single terminal loss after cleanup.
				if outcome != "ended" {
					require.Len(t, report.Moves, 1)
					require.True(t, report.Moves[0].Lost)
					require.Equal(t, "ownership_expired", report.Moves[0].LossCause)
				} else {
					require.Empty(t, report.Moves)
				}
			})
		})
	}
}
