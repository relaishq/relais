package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"testing"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestRelayListFlags(t *testing.T) {
	var entries workers
	fs := flag.NewFlagSet("relays", flag.ContinueOnError)
	fs.Var(&entries, "relay", "relay slots")
	require.NoError(t, fs.Parse([]string{"-relay", "r1=http://127.0.0.1:24700", "-relay", "r2=http://127.0.0.1:24703"}))
	relays, err := parseRelays(entries)
	require.NoError(t, err)
	require.Len(t, relays, 2)
	require.Equal(t, "r1", relays[0].id)
	require.Equal(t, "http://127.0.0.1:24703", relays[1].remote.URL)
	for _, entries := range [][]string{
		nil, {""}, {"r1=http://127.0.0.1:1", "r1=http://127.0.0.1:2"},
		{"r1=http://127.0.0.1:1/", "r2=http://127.0.0.1:1"},
		{"http://127.0.0.1:1", "http://127.0.0.1:2"},
		{"r1=dial-out"}, {"r1=https://127.0.0.1:1"}, {"bad/name=http://127.0.0.1:1"},
	} {
		_, err := parseRelays(entries)
		require.Error(t, err, "%v", entries)
	}
}

func TestRelayFlagsReplaceEnvironmentFallback(t *testing.T) {
	oldFlags, oldArgs := flag.CommandLine, os.Args
	t.Cleanup(func() { flag.CommandLine, os.Args = oldFlags, oldArgs })
	for _, explicit := range []bool{false, true} {
		fallback := "http://127.0.0.1:1"
		if explicit {
			fallback = "invalid=environment"
		}
		t.Setenv("RELAIS_RELAY_URL", fallback)
		flag.CommandLine = flag.NewFlagSet("relay-environment", flag.ContinueOnError)
		os.Args = []string{"relay-environment", "-http", "0.0.0.0:0", "-worker", "w=http://127.0.0.1:2"}
		if explicit {
			os.Args = append(os.Args, "-relay", "r1=http://127.0.0.1:1", "-relay", "r2=http://127.0.0.1:3")
		}
		// Listener rejection proves relay parsing succeeded without opening a
		// store. Explicit flags must replace the invalid environment value.
		require.ErrorContains(t, run(), "private HTTP listener")
	}
}

func TestSingleRelayConfigurationKeepsLegacyBehavior(t *testing.T) {
	for _, entry := range []string{"http://127.0.0.1:1", "r1=http://127.0.0.1:1"} {
		relays, err := parseRelays([]string{entry})
		require.NoError(t, err)
		plane := controlplane.New(relays[0].remote, sessionstore.NewMemory())
		// No network access: even an unavailable single relay keeps the legacy
		// background bootstrap and does not enable relay-loss observations.
		require.NoError(t, configureRelaySet(t.Context(), plane, relays))
		response := httptest.NewRecorder()
		plane.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		require.NotContains(t, response.Body.String(), "relais_control_relay_")
	}
}

func TestRelayListConfiguresNewWorkersWithDiscoveredAddresses(t *testing.T) {
	var entries []string
	var want []mediaworker.RelaySlot
	for i, id := range []string{"r1", "r2"} {
		leg := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(24781+i*2))
		public := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(24780+i*2))
		want = append(want, mediaworker.RelaySlot{ID: id, Leg: leg, Public: public})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/status", r.URL.Path)
			privateapi.Write(w, relay.RelayStatus{Public: public, Private: leg})
		}))
		t.Cleanup(server.Close)
		entries = append(entries, id+"="+server.URL)
	}
	relays, err := parseRelays(entries)
	require.NoError(t, err)
	plane := controlplane.New(relays[0].remote, sessionstore.NewMemory())
	require.NoError(t, configureRelaySet(t.Context(), plane, relays))
	var got mediaworker.RelaySetRequest
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/relays", r.URL.Path)
		require.True(t, privateapi.Read(w, r, &got))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer worker.Close()
	require.NoError(t, plane.Register("w", netip.MustParseAddrPort("127.0.0.1:24711"), &controlplane.RemoteWorker{URL: worker.URL}))
	require.ElementsMatch(t, want, got.Slots)
}

func TestRelayListRejectsUnavailableOrMissingAddresses(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if unavailable {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			privateapi.Write(w, relay.RelayStatus{})
		}))
		relays := []relayEndpoint{{id: "r1", remote: &controlplane.RemoteRelay{URL: server.URL}}, {id: "r2", remote: &controlplane.RemoteRelay{URL: server.URL}}}
		plane := controlplane.New(relays[0].remote, sessionstore.NewMemory())
		require.Error(t, configureRelaySet(context.Background(), plane, relays))
		server.Close()
	}
}
