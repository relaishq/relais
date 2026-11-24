package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"

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
	store := sessionstore.NewMemory()
	var entries []string
	var want []mediaworker.RelaySlot
	for i, id := range []string{"r1", "r2"} {
		leg := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(24781+i*2))
		public := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(24780+i*2))
		want = append(want, mediaworker.RelaySlot{ID: id, Leg: leg, Public: public})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/workers" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			privateapi.Write(w, relay.RelayStatus{Public: public, Private: leg, Instance: id, Continuity: &relay.ContinuityStatus{Epoch: 1}})
		}))
		t.Cleanup(server.Close)
		entries = append(entries, id+"="+server.URL)
		_, err := store.ClaimRelay(t.Context(), id, sessionstore.RelayProcess{Owner: id, PID: 1, Start: "test", Host: "test", Namespace: "test", Executable: "test"}, time.Hour)
		require.NoError(t, err)
	}
	relays, err := parseRelays(entries)
	require.NoError(t, err)
	plane := controlplane.New(relays[0].remote, store)
	require.NoError(t, configureRelaySet(t.Context(), plane, relays))
	configured := make(chan mediaworker.RelaySetRequest, 3)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/relays", r.URL.Path)
		var req mediaworker.RelaySetRequest
		require.True(t, privateapi.Read(w, r, &req))
		configured <- req
		w.WriteHeader(http.StatusNoContent)
	}))
	defer worker.Close()
	require.NoError(t, plane.Register("w", netip.MustParseAddrPort("127.0.0.1:24711"), &controlplane.RemoteWorker{URL: worker.URL}))
	require.Empty(t, (<-configured).Slots, "pending slots are not enrolled")
	var got []mediaworker.RelaySlot
	for _, entry := range relays {
		registry := controlplane.RelayWorkerRegistry{Slot: entry.id}
		require.NoError(t, registry.Sync(t.Context(), plane, entry.remote))
		req := <-configured
		require.EqualValues(t, 1, req.Epochs[entry.id])
		got = append(got, req.Slots...)
	}
	require.ElementsMatch(t, want, got)
}

func TestRelayListKeepsUnavailableOrMissingAddressesPending(t *testing.T) {
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
		require.NoError(t, configureRelaySet(context.Background(), plane, relays))
		registry := controlplane.RelayWorkerRegistry{Slot: "r1"}
		require.Error(t, registry.Sync(t.Context(), plane, relays[0].remote))
		server.Close()
	}
}

// A down listed relay no longer prevents the control listener from starting.
func TestRelaySetBootstrapLeavesUnreachableSlotsPending(t *testing.T) {
	var probes atomic.Int32
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		privateapi.Write(w, relay.RelayStatus{
			Private: netip.MustParseAddrPort("127.0.0.1:24781"),
			Public:  netip.MustParseAddrPort("127.0.0.1:24780"),
		})
	}))
	defer healthy.Close()
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()
	relays, err := parseRelays([]string{"r1=" + healthy.URL, "r2=" + unreachable.URL})
	require.NoError(t, err)
	plane := controlplane.New(relays[0].remote, sessionstore.NewMemory())
	require.NoError(t, configureRelaySet(t.Context(), plane, relays))
	require.Zero(t, probes.Load(), "discovery belongs to the background registration loop")
	status, err := plane.Status(t.Context())
	require.NoError(t, err)
	require.Len(t, status.Relays, 2)
	require.True(t, status.Relays[0].Pending)
	require.True(t, status.Relays[1].Pending)
	require.False(t, status.Ready, "all-pending control serves management but refuses creates")
}
