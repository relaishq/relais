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

	"github.com/relaishq/relais/pkg/controlplane"
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
		registerRestartedRelay(ctx, plane, &controlplane.RemoteRelay{URL: server.URL, Client: server.Client()}, first)
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
