package callharness

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type blockedRelaySocket struct {
	net.PacketConn
	blocked *atomic.Bool
	address string
}

func (s blockedRelaySocket) WriteTo(p []byte, to net.Addr) (int, error) {
	if s.blocked.Load() && to.String() == s.address {
		return len(p), nil
	}
	return s.PacketConn.WriteTo(p, to)
}
func (s blockedRelaySocket) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, from, err := s.PacketConn.ReadFrom(p)
		if err != nil || !s.blocked.Load() || from.String() != s.address {
			return n, from, err
		}
	}
}

// Control keeps its live relay view. Only the caller's access to that public
// address fails; no relay-change event is sent until the caller requests it.
func TestCallerStallChangesLiveRelay(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		p, w, h, relays, _ := relayChangeTopology(t, store, false, false)
		var blocked atomic.Bool
		h.external.CallerSocket = func() (net.PacketConn, error) {
			socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
			return blockedRelaySocket{socket, &blocked, relays[0].PublicAddr().String()}, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		leases := store.(sessionstore.RelayLeases)
		identity := sessionstore.RelayProcess{Owner: "live-relay", PID: 42, Start: "start", Host: "host", Namespace: "namespace", Executable: "relay"}
		var tokens []sessionstore.RelayLease
		for _, id := range []string{"r1", "r2"} {
			token, e := leases.ClaimRelay(ctx, id, identity, time.Minute)
			require.NoError(t, e)
			tokens = append(tokens, token)
		}
		life, stopLife := context.WithCancel(ctx)
		renewed, detector := make(chan struct{}), make(chan error, 1)
		require.NoError(t, p.Heartbeat(w.LocalAddr()))
		go func() { detector <- p.Run(life) }()
		go func() {
			defer close(renewed)
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-life.Done():
					return
				case <-tick.C:
					_ = p.Heartbeat(w.LocalAddr())
					for i, token := range tokens {
						tokens[i], _ = leases.RenewRelay(life, token, time.Minute)
					}
				}
			}
		}()
		defer func() { stopLife(); <-renewed; require.NoError(t, <-detector) }()
		call, err := h.Dial(ctx, CallOptions{Video: true, RelayChange: true})
		require.NoError(t, err)
		mediaCtx, stop := context.WithCancel(ctx)
		sent := make(chan error, 1)
		go func() { sent <- call.SendMedia(mediaCtx, 12*time.Second) }()
		defer func() { stop(); <-sent }()
		require.Eventually(t, func() bool { return len(call.rec.report().Tracks) == 2 }, 2*time.Second, 10*time.Millisecond)
		fault := time.Now()
		blocked.Store(true)
		require.Eventually(t, func() bool {
			_, a, e := store.(sessionstore.RelayAssignments).RelayOwner(ctx, call.SessionID())
			return e == nil && a.Slot == "r2" && a.Pending == "" && a.Generation >= 2
		}, 5*time.Second, 10*time.Millisecond)
		observed := time.Now()
		require.Eventually(t, func() bool { return decodedRelayRecovery(call, fault, observed) > 0 }, time.Second, 10*time.Millisecond)
		gap := decodedRelayRecovery(call, fault, observed)
		require.LessOrEqual(t, gap, 5*time.Second)
		failures, err := w.SessionDecryptFailures(call.SessionID())
		require.NoError(t, err)
		require.Zero(t, failures)
		report := call.rec.report()
		require.Zero(t, report.DecryptionFailures.Total())
		for _, track := range report.Tracks {
			require.Zero(t, track.DuplicatePackets)
		}
		t.Logf("CALLER_STALL row=4 n=1 decoded_upper_bound=%s auth=0 duplicates=0", gap)
	})
}
