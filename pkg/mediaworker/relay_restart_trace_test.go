package mediaworker

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/relaytrace"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestRestartMismatchTraceNamesEveryRefusingCheckWithoutCredentials(t *testing.T) {
	for _, reason := range []string{"identity_parse", "offer_parse", "component_mode", "agent_v2", "identity", "missing_relay", "relay_slot", "pending_credentials", "required_mid"} {
		t.Run(reason, func(t *testing.T) {
			var mu sync.Mutex
			var initial, refused []byte
			var event, call string
			stop := relaytrace.Observe(func(_ time.Time, name, path string, data []byte) {
				mu.Lock()
				defer mu.Unlock()
				if name == "worker-restart-initial" {
					initial = append([]byte(nil), data...)
				}
				if strings.HasPrefix(name, "worker-restart-refused/") {
					event, call = name, path
					refused = append([]byte(nil), data...)
				}
			})
			defer stop()
			ctx := t.Context()
			store := sessionstore.NewMemory()
			first, second := listenTestUDP(t).LocalAddr().(*net.UDPAddr).AddrPort(), listenTestUDP(t).LocalAddr().(*net.UDPAddr).AddrPort()
			w, err := New(Config{Relay: &RelayConfig{Owners: store, Addr: first, PublicAddr: first}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, w.Close()) })
			require.NoError(t, w.ConfigureRelays(ctx, []RelaySlot{{ID: "r1", Leg: first, Public: first}, {ID: "r2", Leg: second, Public: second}}))
			offer := testOffer(testOfferAttrs{session: []string{"a=ice-pwd:session-secret", "a=ICE-PWD:case-secret"}, audio: []string{"a=ssrc:123 cname:caller"}})
			id, _, err := w.CreateSession(ctx, offer)
			require.NoError(t, err)
			lease, from, err := store.RelayOwner(ctx, id)
			require.NoError(t, err)
			assignment := from
			assignment.Pending = "r2"
			require.NoError(t, store.CASRelay(ctx, lease, from, assignment))
			req := RestartRequest{Offer: offer, Generation: 1, Slot: "r2"}
			s := w.session(id)
			s.mu.Lock()
			original, component, pending := s.state, s.componentMode, s.pendingRestart
			switch reason {
			case "identity_parse":
				req.Offer = "invalid SDP a=ice-pwd:malformed-secret"
			case "offer_parse":
				req.Offer = strings.ReplaceAll(offer, "a=setup:actpass", "a=setup:passive")
			case "component_mode":
				s.componentMode = true
			case "agent_v2":
				s.state.Agent.V2 = &v2Envelope{}
			case "identity":
				req.Offer = strings.ReplaceAll(offer, "a=ssrc:123", "a=ssrc:456")
			case "missing_relay":
				s.state.ICE.Relay = nil
			case "relay_slot":
				s.state.ICE.Relay = &RelaySlot{ID: "different", Leg: first, Public: first}
			case "pending_credentials":
				s.pendingRestart = &pendingRestart{from: assignment, ice: iceState{RemoteUfrag: "different", RemotePwd: "pending-secret"}}
			case "required_mid":
				req.Required = []string{"unknown"}
			}
			s.mu.Unlock()
			defer func() {
				s.mu.Lock()
				s.state, s.componentMode, s.pendingRestart = original, component, pending
				s.mu.Unlock()
			}()
			_, err = w.RestartRelaySession(ctx, id, req)
			require.Same(t, ErrRestartMismatch, err, "public refusal remains the same sentinel")
			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, "worker-restart-refused/"+reason, event)
			require.Equal(t, id, call)
			require.NotEmpty(t, initial)
			if reason == "identity" {
				require.Contains(t, string(initial), "a=ssrc:123")
				require.Contains(t, string(refused), "a=ssrc:456")
			} else {
				require.Empty(t, refused, "non-identity errors expose only a bounded reason")
			}
			for _, secret := range []string{"ice-pwd", testCallerPwd, "session-secret", "malformed-secret", "pending-secret", "case-secret"} {
				require.NotContains(t, string(initial), secret)
				require.NotContains(t, string(refused), secret)
			}
		})
	}
}
