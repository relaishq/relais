package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type relayBootstrapLog struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *relayBootstrapLog) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(data)
}
func (l *relayBootstrapLog) text() string { l.mu.Lock(); defer l.mu.Unlock(); return l.buffer.String() }

func TestRelayPollingLogsTransitionsAndLatchesReadiness(t *testing.T) {
	var output relayBootstrapLog
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	var polls atomic.Int64
	var failNext, blockRegistration atomic.Bool
	var instance atomic.Value
	instance.Store("first")
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			polls.Add(1)
			if failNext.Swap(false) {
				http.Error(w, "temporarily slow relay", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(relay.RelayStatus{Instance: instance.Load().(string)})
		case "/workers":
			if blockRegistration.Load() {
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	plane := controlplane.New(nil, sessionstore.NewMemory())
	plane.BeginBootstrap([]string{"w"})
	require.NoError(t, plane.Register("w", netip.MustParseAddrPort("127.0.0.1:1"), &controlplane.RemoteWorker{URL: server.URL}))
	first, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		registerRestartedRelay(ctx, plane, "", &controlplane.RemoteRelay{URL: server.URL, Client: server.Client()}, first)
	}()
	defer func() { releaseOnce.Do(func() { close(release) }); cancel(); <-done }()
	ready := func() bool {
		status, err := plane.Status(context.Background())
		require.NoError(t, err)
		return status.Ready
	}
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("relay bootstrap did not finish its first attempt")
	}
	require.True(t, ready())
	require.Eventually(t, func() bool { return polls.Load() >= 4 }, 3*time.Second, 5*time.Millisecond)
	assert.Equal(t, 1, strings.Count(output.text(), "result=registered"), "healthy polls are silent")
	failNext.Store(true)
	require.Eventually(t, func() bool { return strings.Contains(output.text(), "result=retry:") }, 3*time.Second, 5*time.Millisecond)
	assert.True(t, ready(), "one failed poll cannot revoke established readiness")
	require.Eventually(t, func() bool { return strings.Count(output.text(), "result=registered") >= 2 }, 3*time.Second, 5*time.Millisecond)
	stablePolls := polls.Load()
	require.Eventually(t, func() bool { return polls.Load() >= stablePolls+3 }, 3*time.Second, 5*time.Millisecond)
	assert.Equal(t, 2, strings.Count(output.text(), "result=registered"), "only recovery logs another success")
	blockRegistration.Store(true)
	instance.Store("second")
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("new relay instance was not registered")
	}
	assert.False(t, ready(), "a new relay instance gates creates until re-registration completes")
	releaseOnce.Do(func() { close(release) })
	require.Eventually(t, ready, 3*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		return strings.Contains(output.text(), "instance=second") && strings.Count(output.text(), "result=registered") == 3
	}, 3*time.Second, 5*time.Millisecond)
}

type heldRelayRebuild struct {
	*sessionstore.Memory
	entered, release chan struct{}
	once             sync.Once
}

func (s *heldRelayRebuild) ControlRecords(ctx context.Context, lease sessionstore.ControlLease) (map[string]json.RawMessage, error) {
	s.once.Do(func() {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
		}
	})
	return s.Memory.ControlRecords(ctx, lease)
}

type relayRetryBarrier struct{ fourths chan struct{} }

func (b relayRetryBarrier) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte("relay bootstrap attempt=4 ")) {
		b.fourths <- struct{}{}
	}
	return len(data), nil
}

func TestBothRelayLoopsAdmitPromptlyAfterControlRebuild(t *testing.T) {
	store := &heldRelayRebuild{Memory: sessionstore.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}
	old, err := store.ClaimControl(t.Context(), "before-restart", time.Second)
	require.NoError(t, err)
	require.NoError(t, store.ReleaseControl(t.Context(), old))
	ctx, cancel := context.WithCancel(t.Context())
	var jobs sync.WaitGroup
	var release sync.Once
	managed := make(chan error, 1)
	t.Cleanup(func() {
		release.Do(func() { close(store.release) })
		cancel()
		jobs.Wait()
	})
	joined := map[string]chan struct{}{"r1": make(chan struct{}), "r2": make(chan struct{})}
	joinedOnce := map[string]*sync.Once{"r1": {}, "r2": {}}
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			privateapi.Write(w, mediaworker.WorkerStatus{Address: netip.MustParseAddrPort("127.0.0.1:23456"), Instance: "worker", InventoryKnown: true})
		case "/relays":
			var req mediaworker.RelaySetRequest
			if !privateapi.Read(w, r, &req) {
				return
			}
			for _, slot := range req.Slots {
				joinedOnce[slot.ID].Do(func() { close(joined[slot.ID]) })
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(worker.Close)
	plane := controlplane.New(nil, store)
	var bindings []controlplane.RelayBinding
	var remotes []*controlplane.RemoteRelay
	for i, id := range []string{"r1", "r2"} {
		lease, err := store.ClaimRelay(t.Context(), id, sessionstore.RelayProcess{Owner: id, PID: 1, Start: "test", Host: "test", Namespace: "test", Executable: "test"}, time.Hour)
		require.NoError(t, err)
		jobs.Go(func() {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := store.RenewRelay(ctx, lease, time.Hour); err != nil && ctx.Err() == nil {
						t.Error(err)
						return
					}
				}
			}
		})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/status" {
				addr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 18001+uint16(i))
				privateapi.Write(w, relay.RelayStatus{Instance: id, Continuity: &relay.ContinuityStatus{Epoch: lease.Epoch}, Private: addr, Public: addr})
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(server.Close)
		remote := &controlplane.RemoteRelay{URL: server.URL, Client: server.Client()}
		remotes = append(remotes, remote)
		bindings = append(bindings, controlplane.RelayBinding{Slot: mediaworker.RelaySlot{ID: id}, Relay: remote})
	}
	require.NoError(t, plane.ConfigureRelaySet(t.Context(), bindings))
	require.NoError(t, plane.EnableRestart())
	go func() { managed <- plane.RunManaged(ctx, []controlplane.FixedWorker{{Name: "w", URL: worker.URL}}) }()
	t.Cleanup(func() { release.Do(func() { close(store.release) }); cancel(); require.NoError(t, <-managed) })
	select {
	case <-store.entered:
	case <-time.After(8 * time.Second):
		t.Fatal("successor did not start rebuilding")
	}
	barrier := relayRetryBarrier{fourths: make(chan struct{}, 2)}
	previous := log.Writer()
	log.SetOutput(barrier)
	defer log.SetOutput(previous)
	for i, remote := range remotes {
		// A shared notification can go to another receiver. Each loop must
		// admit promptly using its own polling even without that wake-up.
		jobs.Go(func() { registerRelayLoop(ctx, plane, bindings[i].Slot.ID, remote, nil, nil) })
	}
	for range 2 {
		select {
		case <-barrier.fourths:
		case <-time.After(8 * time.Second):
			t.Fatal("both relay loops must retry while rebuild is held")
		}
	}
	release.Do(func() { close(store.release) })
	require.Eventually(t, func() bool {
		status, err := plane.Status(t.Context())
		return err == nil && status.Rebuild != nil && status.Rebuild.Phase == "ready"
	}, 8*time.Second, time.Millisecond)
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for _, id := range []string{"r1", "r2"} {
		select {
		case <-joined[id]:
		case <-deadline.C:
			t.Fatal("both relays must enroll the worker promptly after rebuild")
		}
	}
	require.Eventually(t, func() bool {
		status, err := plane.Status(t.Context())
		return err == nil && len(status.Relays) == 2 && !status.Relays[0].Pending && !status.Relays[1].Pending
	}, 8*time.Second, time.Millisecond)
}
