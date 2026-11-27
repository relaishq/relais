package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func TestRelayLossCallerOptionsAreExplicit(t *testing.T) {
	o := ChildOptions{Callers: 4, Video: true}
	for i := range 4 {
		old := o.callOptions(i, 15*time.Second, &Meter{})
		require.False(t, old.RelayChange)
		require.False(t, old.RelayLossEvidence)
		require.Empty(t, old.ExternalWorker)
		require.Equal(t, -1, old.Worker)
	}
	o.RelayLoss = true
	for i, worker := range []string{"w1", "w1", "w2", "w2"} {
		got := o.callOptions(i, 15*time.Second, &Meter{})
		require.True(t, got.RelayChange)
		require.True(t, got.RelayLossEvidence)
		require.False(t, got.MediaProgress)
		require.Equal(t, worker, got.ExternalWorker)
		require.Equal(t, 15*time.Second, got.Recording.History)
		require.Equal(t, 30, got.Recording.DecodeEvery)
	}
}

func TestRelayLossWrapperLeavesNormalMessageBytes(t *testing.T) {
	m := Message{Type: "call", PID: 12, Session: "session"}
	var normal, empty, wrapped bytes.Buffer
	NewWriter(&normal).Write(m)
	NewWriter(&empty).writeRelayLoss(m, nil)
	require.Equal(t, normal.String(), empty.String())
	require.Equal(t, "{\"type\":\"call\",\"pid\":12,\"session\":\"session\"}\n", normal.String())
	rows := []callharness.RelayRestartEpisode{{Generation: 1, ArrivalsBeforeApplied: []callharness.RelayArrivalCount{{Kind: "audio", Count: 1}}}}
	NewWriter(&wrapped).writeRelayLoss(m, rows)
	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wrapped.Bytes(), &object))
	var got []callharness.RelayRestartEpisode
	require.NoError(t, json.Unmarshal(object["relayLossEpisodes"], &got))
	require.Equal(t, rows, got)
	delete(object, "relayLossEpisodes")
	plain, err := json.Marshal(object)
	require.NoError(t, err)
	require.JSONEq(t, normal.String(), string(plain))
}

func TestRelayLossRegistrationProbesNeedNoClockTracker(t *testing.T) {
	var samples atomic.Int64
	signal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/clock" {
			t.Errorf("unexpected probe path %s", r.URL.Path)
		}
		samples.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]time.Time{"received_at": time.Now(), "sent_at": time.Now()})
	}))
	defer signal.Close()
	mux := http.NewServeMux()
	registerRelayLossProbes(mux, signal.URL+"/calls", signal.Client())
	for range 2 {
		reply := httptest.NewRecorder()
		mux.ServeHTTP(reply, httptest.NewRequest(http.MethodGet, "/clock-probes", nil))
		require.Equal(t, http.StatusOK, reply.Code)
		var probes []callharness.ClockProbe
		require.NoError(t, json.Unmarshal(reply.Body.Bytes(), &probes))
		require.Len(t, probes, 10)
	}
	require.EqualValues(t, 10, samples.Load(), "registration burst is bounded and reused, not background clock mapping")
}

func TestRunRelayLossCallerPlacesTwoCallsPerNamedWorker(t *testing.T) {
	var w1, w2, capable atomic.Int64
	signal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("worker") {
		case "w1":
			w1.Add(1)
		case "w2":
			w2.Add(1)
		default:
			t.Errorf("unexpected worker %q", r.URL.RawQuery)
		}
		if r.Header.Get("Relais-Relay-Change") == "true" && r.Header.Get("Relais-Media-Progress") == "" {
			capable.Add(1)
		}
		// Refuse before ICE setup. This tests the real caller/HTTP boundary,
		// without a live relay fault or a separate media timing claim.
		http.Error(w, "test admission refusal", http.StatusServiceUnavailable)
	}))
	defer signal.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := RunChild(ctx, ChildOptions{RelayLoss: true, Callers: 4, Video: true, Duration: time.Second, SampleEvery: time.Second, ParentPID: os.Getppid(), Output: filepath.Join(t.TempDir(), "caller.jsonl"), Topology: callharness.ExternalTopology{SignalingURL: signal.URL + "/calls", RelayAddr: netip.MustParseAddrPort("127.0.0.1:24780")}})
	require.ErrorContains(t, err, "4 of 4 dials failed")
	require.EqualValues(t, 2, w1.Load())
	require.EqualValues(t, 2, w2.Load())
	require.EqualValues(t, 4, capable.Load())
}

func TestRunRelayLossCallerRetriesCapacityRefusal(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	workers := map[string]int{}
	signal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		mu.Lock()
		defer mu.Unlock()
		key := r.URL.String() + string(body)
		seen[key]++
		workers[r.URL.Query().Get("worker")]++
		if seen[key] == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"code":"at_capacity","message":"retry the identical message"}`)
			return
		}
		// Stop before media: only the unchanged offer/worker retry is under test.
		http.Error(w, "definite test refusal after capacity settled", http.StatusBadRequest)
	}))
	defer signal.Close()
	err := RunChild(t.Context(), ChildOptions{RelayLoss: true, Callers: 4, Video: true, Duration: time.Second, SampleEvery: time.Second, ParentPID: os.Getppid(), Output: filepath.Join(t.TempDir(), "caller.jsonl"), Topology: callharness.ExternalTopology{SignalingURL: signal.URL + "/calls", RelayAddr: netip.MustParseAddrPort("127.0.0.1:24780")}})
	require.ErrorContains(t, err, "definite test refusal after capacity settled")
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seen, 4)
	for _, attempts := range seen {
		require.Equal(t, 2, attempts)
	}
	require.Equal(t, map[string]int{"w1": 4, "w2": 4}, workers)
}

func TestOrdinaryCallerDoesNotRetryCapacity(t *testing.T) {
	var attempts atomic.Int64
	signal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"code":"at_capacity"}`)
	}))
	defer signal.Close()
	err := RunChild(t.Context(), ChildOptions{Callers: 4, Video: true, Duration: time.Second, SampleEvery: time.Second, ParentPID: os.Getppid(), Output: filepath.Join(t.TempDir(), "caller.jsonl"), Topology: callharness.ExternalTopology{SignalingURL: signal.URL + "/calls", RelayAddr: netip.MustParseAddrPort("127.0.0.1:24780")}})
	require.ErrorContains(t, err, "at_capacity")
	require.EqualValues(t, 4, attempts.Load())
}
