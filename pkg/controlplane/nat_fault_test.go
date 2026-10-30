package controlplane_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type natTransportFunc func(*http.Request) (*http.Response, error)

func (f natTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func natSystem(t *testing.T, store sessionstore.Store, transport http.RoundTripper) (*httpSystem, *udpNAT) {
	var nat *udpNAT
	s := newHTTPSystemWithSettings(t, nil, nil, nil, httpSystemSettings{store: store, dialOut: true, targetTransport: transport, sourceLeg: func(a netip.AddrPort) netip.AddrPort { nat = newUDPNAT(t, a); return nat.WorkerAddr() }})
	return s, nat
}
func rebindReady(s controlplane.Status, address netip.AddrPort) bool {
	for _, w := range s.Workers {
		if w.Name == "0" {
			return w.Address == address && !w.Draining && !w.Dead && w.Link != nil && w.Link.Connected
		}
	}
	return false
}
func requireNATCounters(t *testing.T, s *httpSystem) {
	t.Helper()
	status, err := s.plane.Status(context.Background())
	require.NoError(t, err)
	require.Empty(t, status.Takeovers)
	require.Zero(t, status.LostCount)
	for _, w := range s.workers {
		reply := httptest.NewRecorder()
		w.PrivateHandler().ServeHTTP(reply, httptest.NewRequest("GET", "/metrics", nil))
		require.Contains(t, reply.Body.String(), "relais_worker_decryption_failures_total 0\n")
	}
}
func TestDialOutNATFlapBackKeepsCall(t *testing.T) {
	forHTTPRecoveryStores(t, func(t *testing.T, store sessionstore.Store) {
		var nat *udpNAT
		var once sync.Once
		transport := natTransportFunc(func(req *http.Request) (*http.Response, error) {
			resp, err := http.DefaultTransport.RoundTrip(req)
			if err == nil && req.URL.Path == "/resume" {
				once.Do(func() { err = nat.Rebind(nat.original) })
			}
			return resp, err
		})
		s, n := natSystem(t, store, transport)
		nat = n
		report := flowingCall(t, s, func(ctx context.Context, c *callharness.Call) {
			nat.rebind(t)
			require.Eventually(t, func() bool {
				status, err := s.plane.Status(ctx)
				return err == nil && len(status.Calls) == 1 && status.Calls[0].Owner == "1" && rebindReady(status, nat.original)
			}, 6*time.Second, 20*time.Millisecond)
			_, err := s.plane.Move(ctx, c.SessionID(), "0")
			require.NoError(t, err)
			_, err = s.plane.Move(ctx, c.SessionID(), "1")
			require.NoError(t, err, "original address must receive drain barriers after flap-back")
		})
		require.Zero(t, report.DecryptionFailures.Total())
		requireNATCounters(t, s)
		t.Log("NAT_FLAP_BACK false_takeovers=0 lost_calls=0 caller_decryption_failures=0 worker_decryption_failures=0")
	})
}
func TestDialOutNATDuringOperatorMove(t *testing.T) {
	forHTTPRecoveryStores(t, func(t *testing.T, store sessionstore.Store) {
		entered, release := make(chan struct{}), make(chan struct{})
		var gate, finish sync.Once
		defer finish.Do(func() { close(release) })
		transport := natTransportFunc(func(req *http.Request) (*http.Response, error) {
			resp, err := http.DefaultTransport.RoundTrip(req)
			if err == nil && req.URL.Path == "/resume" {
				gate.Do(func() {
					close(entered)
					select {
					case <-release:
					case <-req.Context().Done():
					}
				})
			}
			return resp, err
		})
		s, nat := natSystem(t, store, transport)
		report := flowingCall(t, s, func(ctx context.Context, c *callharness.Call) {
			moved := make(chan error, 1)
			go func() { _, err := s.plane.Move(ctx, c.SessionID(), "1"); moved <- err }()
			<-entered
			next := nat.rebind(t)
			require.NoError(t, nat.Observe())
			require.Eventually(t, func() bool {
				status, err := s.plane.Status(ctx)
				if err != nil {
					return false
				}
				for _, w := range status.Workers {
					if w.Name == "0" {
						return w.Draining
					}
				}
				return false
			}, 1500*time.Millisecond, 10*time.Millisecond)
			require.Equal(t, nat.original, s.workers[0].LocalAddr(), "active operator move must retain its source identity")
			finish.Do(func() { close(release) })
			require.NoError(t, <-moved)
			require.Eventually(t, func() bool { status, err := s.plane.Status(ctx); return err == nil && rebindReady(status, next) }, 5*time.Second, 20*time.Millisecond)
			_, err := s.plane.Move(ctx, c.SessionID(), "0")
			require.NoError(t, err)
		})
		require.Zero(t, report.DecryptionFailures.Total())
		requireNATCounters(t, s)
		t.Log("NAT_OPERATOR_MOVE false_takeovers=0 lost_calls=0 caller_decryption_failures=0 worker_decryption_failures=0")
	})
}
func TestDialOutNATMovesSeveralCalls(t *testing.T) {
	forHTTPRecoveryStores(t, func(t *testing.T, store sessionstore.Store) {
		s, nat := natSystem(t, store, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		media, stop := context.WithCancel(ctx)
		defer stop()
		var calls []*callharness.Call
		var senders sync.WaitGroup
		defer func() { stop(); senders.Wait() }()
		for range 3 {
			c, err := s.harness.Dial(ctx, callharness.CallOptions{Video: true, Worker: 0})
			require.NoError(t, err)
			calls = append(calls, c)
			senders.Go(func() { _ = c.SendMedia(media, 15*time.Second) })
		}
		time.Sleep(600 * time.Millisecond)
		started := time.Now()
		next := nat.rebind(t)
		require.Eventually(t, func() bool {
			status, err := s.plane.Status(ctx)
			if err != nil || len(status.Calls) != 3 || !rebindReady(status, next) {
				return false
			}
			for _, c := range status.Calls {
				if c.Owner != "1" {
					return false
				}
			}
			return true
		}, 6*time.Second, 20*time.Millisecond)
		elapsed := time.Since(started)
		for _, c := range calls {
			_, err := s.plane.Move(ctx, c.SessionID(), "0")
			require.NoError(t, err)
		}
		time.Sleep(700 * time.Millisecond)
		for _, c := range calls {
			observed := time.Now()
			report, err := c.Hangup(ctx)
			require.NoError(t, err)
			require.True(t, report.ConnectedThroughout())
			require.Zero(t, report.DecryptionFailures.Total())
			require.Zero(t, report.ICERestarts)
			require.Zero(t, report.Renegotiations)
			for _, kind := range []string{"audio", "video"} {
				track := report.Track(kind)
				require.NotNil(t, track)
				require.Less(t, observed.Sub(report.StartedAt.Add(track.LastArrival)), 150*time.Millisecond, "each call must carry fresh media after rebind and return move")
			}
		}
		requireNATCounters(t, s)
		t.Logf("NAT_MULTICALL calls=3 recovery=%s false_takeovers=0 lost_calls=0 caller_decryption_failures=0 worker_decryption_failures=0", elapsed)
	})
}
