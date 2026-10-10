package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestExplicitRemoteRejoin(t *testing.T) {
	p, a, _, _ := setup(t)
	p.workers["a"].dead, p.workers["a"].recovered = true, true
	server := httptest.NewServer(p.ProcessHandler())
	defer server.Close()
	beat := func(token string) (HeartbeatReply, error) {
		var reply HeartbeatReply
		err := privateapi.Do(context.Background(), server.Client(), server.URL, http.MethodPost, "/private/workers/a/heartbeat", HeartbeatRequest{Address: a.addr, Token: token}, &reply, nil)
		return reply, err
	}
	challenge, err := beat("")
	require.NoError(t, err)
	require.NotEmpty(t, challenge.Token)
	again, err := beat("")
	require.NoError(t, err)
	require.Equal(t, challenge, again, "ordinary heartbeat must not rejoin")
	wrong, err := beat("wrong")
	require.NoError(t, err)
	require.Equal(t, challenge, wrong)
	p.mu.Lock()
	dead := p.workers["a"].dead
	p.mu.Unlock()
	require.True(t, dead)
	ack, err := beat(challenge.Token)
	require.NoError(t, err)
	require.Empty(t, ack.Token)
	// Lost ACK responses are safe: the exact token is accepted idempotently.
	ack, err = beat(challenge.Token)
	require.NoError(t, err)
	require.Empty(t, ack.Token)
	_, err = beat("wrong")
	require.Error(t, err)
	p.mu.Lock()
	w := p.workers["a"]
	require.False(t, w.dead)
	w.dead, w.recovered, w.rejoinToken, w.acceptedToken = true, true, "", ""
	p.mu.Unlock()
	next, err := beat(challenge.Token)
	require.NoError(t, err)
	require.NotEqual(t, challenge.Token, next.Token)
	remote := &RemoteHeartbeats{URL: server.URL, Name: "a"}
	require.ErrorIs(t, remote.Heartbeat(a.addr), mediaworker.ErrRejoinRequired)
	// Worker.StartHeartbeats calls this second round only after fencing/dropping.
	require.NoError(t, remote.Heartbeat(a.addr))
	require.NoError(t, remote.Heartbeat(a.addr))
	require.Error(t, remote.Heartbeat(netip.MustParseAddrPort("127.0.0.1:99")))
	p.mu.Lock()
	w.dead, w.recovering, w.recovered = true, true, false
	p.mu.Unlock()
	_, err = beat("")
	require.Error(t, err, "incomplete recovery cannot issue a challenge")
}

type gatedListStore struct {
	sessionstore.Store
	started chan struct{}
	release chan struct{}
	worker  netip.AddrPort
}

func (s *gatedListStore) ListByWorker(ctx context.Context, addr netip.AddrPort) ([]sessionstore.Lease, error) {
	if addr == s.worker {
		s.started <- struct{}{}
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Store.ListByWorker(ctx, addr)
}
func TestSlowListingDoesNotBlockHeartbeatAndRechecksPlacement(t *testing.T) {
	for _, operation := range []string{"pick", "drain", "replace"} {
		t.Run(operation, func(t *testing.T) {
			p, a, b, _ := setup(t)
			store := &gatedListStore{Store: p.store, started: make(chan struct{}, 1), release: make(chan struct{}), worker: b.addr}
			p.store = store
			done := make(chan error, 1)
			switch operation {
			case "pick":
				go func() { _, err := p.pick(context.Background(), "b", netip.AddrPort{}); done <- err }()
			case "drain":
				go func() { _, err := p.Drain(context.Background(), "b"); done <- err }()
			case "replace":
				p.workers["b"].dead, p.workers["b"].recovered = true, true
				go func() { done <- p.Replace("b", netip.MustParseAddrPort("127.0.0.1:99"), b) }()
			}
			select {
			case <-store.started:
			case <-time.After(time.Second):
				t.Fatal("listing never started")
			}
			// If mu spans Redis I/O, this heartbeat blocks until the release below.
			heartbeat := make(chan error, 1)
			go func() { heartbeat <- p.Heartbeat(a.addr) }()
			select {
			case err := <-heartbeat:
				require.NoError(t, err)
			case <-time.After(200 * time.Millisecond):
				close(store.release)
				t.Fatal("store listing blocked heartbeat")
			}
			p.mu.Lock()
			if operation == "pick" {
				p.workers["b"].draining = true
			}
			if operation == "replace" {
				p.workers["b"].dead = false
			}
			p.mu.Unlock()
			close(store.release)
			err := <-done
			switch operation {
			case "pick":
				require.ErrorIs(t, err, ErrNoTarget)
				require.Zero(t, b.next)
			case "drain":
				require.NoError(t, err)
			case "replace":
				require.ErrorContains(t, err, "raced")
			}
		})
	}
}
