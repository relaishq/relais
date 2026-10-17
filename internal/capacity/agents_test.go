package capacity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAgentSamplerRetirementDuringRequest(t *testing.T) {
	requested, release, observed := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requested)
		<-release
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer old.Close()
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"agent_session":{"id":"session","name":"custom/v1","checkpoint_known":true}}`))
		select {
		case observed <- struct{}{}:
		default:
		}
	}))
	defer next.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Monitor{targets: []Target{{Name: "worker-0", URL: old.URL, AgentSessions: []string{"session"}}, {Name: "worker-1", URL: next.URL}}, retired: make([]atomic.Bool, 2), client: old.Client()}
	done := make(chan struct{})
	go func() { defer close(done); m.sampleAgents(ctx) }()
	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("no request")
	}
	m.retired[0].Store(true)
	close(release)
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("replacement not observed")
	}
	cancel()
	<-done
	require.Empty(t, m.data.Agents.Errors)
}
