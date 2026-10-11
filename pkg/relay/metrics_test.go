package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func relayMetricsBody(r *Relay) string {
	reply := httptest.NewRecorder()
	r.PrivateHandler().ServeHTTP(reply, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return reply.Body.String()
}

func TestRelayMetricsExposeHeldPacketsDropsAndExpiry(t *testing.T) {
	sys := startTestRelay(t, Config{MaxHeldPackets: 1, HoldTimeout: 500 * time.Millisecond})
	a := sys.worker(t, sessionA)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	beginHold(t, sys, a, sessionA)
	for range 2 {
		caller.send(t, media, sys.relay.PublicAddr())
	}
	require.Eventually(t, func() bool { return sys.relay.Stats().HoldDrops == 1 }, receiveTimeout, time.Millisecond)
	body := relayMetricsBody(sys.relay)
	require.Contains(t, body, "relais_relay_held_packets 1\n")
	require.Contains(t, body, "relais_relay_hold_drops_total 1\n")
	require.Eventually(t, func() bool { return sys.relay.Stats().HoldTimeouts == 1 }, receiveTimeout, time.Millisecond)
	body = relayMetricsBody(sys.relay)
	require.Contains(t, body, "relais_relay_held_packets 0\n")
	require.Contains(t, body, "relais_relay_hold_expiries_total 1\n")
	require.Contains(t, body, "relais_relay_hold_drops_total 1\n")
}
