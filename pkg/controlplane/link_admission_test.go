package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/workerlink"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type admissionRig struct {
	p      *Plane
	links  *WorkerLinks
	server *httptest.Server
	client *http.Client
	addr   netip.AddrPort
}
type admissionStream struct {
	ready  LinkReady
	body   io.ReadCloser
	writer *io.PipeWriter
	cancel context.CancelFunc
}

func newAdmissionRig(t *testing.T, relayHandler http.Handler, stores ...sessionstore.Store) *admissionRig {
	t.Helper()
	if relayHandler == nil {
		relayHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	}
	relayServer := httptest.NewServer(relayHandler)
	t.Cleanup(relayServer.Close)
	var store sessionstore.Store = sessionstore.NewMemory()
	if len(stores) > 0 {
		store = stores[0]
	}
	p := New(nil, store)
	links := p.ConfigureDialOut([]string{"a"}, &RemoteRelay{URL: relayServer.URL})
	server := httptest.NewUnstartedServer(links.Handler(p.ProcessHandler()))
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	server.Config.Protocols = protocols
	server.Start()
	t.Cleanup(server.Close)
	tr := workerlink.Transport()
	t.Cleanup(tr.CloseIdleConnections)
	return &admissionRig{p: p, links: links, server: server, client: &http.Client{Transport: tr}, addr: netip.MustParseAddrPort("127.0.0.1:17001")}
}
func (r *admissionRig) open(t *testing.T, instance string, sequence, cycle uint64, addr netip.AddrPort) (*admissionStream, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.server.URL+"/private/workers/a/link", reader)
	type response struct {
		resp *http.Response
		err  error
	}
	done := make(chan response, 1)
	go func() { resp, err := r.client.Do(req); done <- response{resp, err} }()
	timer := time.AfterFunc(3*time.Second, cancel)
	defer timer.Stop()
	if err := workerlink.WriteValue(writer, "hello", LinkHello{Instance: instance, Sequence: sequence, Contract: workerlink.ContractVersion, DeathCycle: cycle, Status: mediaworker.WorkerStatus{InventoryKnown: true}}); err != nil {
		return nil, err
	}
	res := <-done
	if res.err != nil {
		return nil, res.err
	}
	t.Cleanup(func() { _ = res.resp.Body.Close() })
	if res.resp.StatusCode != http.StatusOK {
		cancel()
		return nil, fmt.Errorf("admission status %d", res.resp.StatusCode)
	}
	var welcome LinkWelcome
	if err := workerlink.ReadValue(res.resp.Body, "welcome", &welcome); err != nil {
		return nil, err
	}
	if err := workerlink.WriteValue(writer, "address", LinkAddress{Address: addr}); err != nil {
		return nil, err
	}
	var ready LinkReady
	if err := workerlink.ReadValue(res.resp.Body, "ready", &ready); err != nil {
		cancel()
		return nil, err
	}
	return &admissionStream{ready: ready, body: res.resp.Body, writer: writer, cancel: cancel}, nil
}
func (s *admissionStream) run(handler http.Handler) *workerlink.Peer {
	peer := workerlink.New(s.body, s.writer, s.cancel, handler)
	go func() { _ = peer.Run(context.Background()) }()
	return peer
}
func statusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		privateapi.Write(w, mediaworker.WorkerStatus{InventoryKnown: true})
	})
}
func (r *admissionRig) declareDead() {
	r.p.mu.Lock()
	o := &r.p.workers["a"].observation
	o.detect(time.Now().Add(time.Second), time.Millisecond)
	o.recoveryFinished(true)
	r.p.mu.Unlock()
}

func TestFailedAdmissionDoesNotConsumeIdentity(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint(replacement), func(t *testing.T) {
			r := newAdmissionRig(t, nil)
			a, err := r.open(t, "A", 1, 0, r.addr)
			require.NoError(t, err)
			a.run(statusHandler())
			instance, sequence := "A", uint64(99)
			if replacement {
				r.declareDead()
				instance, sequence = "B", 1
			}
			_, err = r.open(t, instance, sequence, 0, netip.AddrPort{})
			require.Error(t, err)
			if replacement {
				hb := &RemoteHeartbeats{URL: r.server.URL, Name: "a"}
				require.ErrorIs(t, hb.Heartbeat(r.addr), mediaworker.ErrRejoinRequired)
				require.NoError(t, hb.Heartbeat(r.addr))
			}
			stream, err := r.open(t, "A", 2, 0, r.addr)
			require.NoError(t, err, "refused hello consumed live identity/sequence")
			stream.run(statusHandler())
		})
	}
}
func TestAdmissionDoesNotBlockExistingCommands(t *testing.T) {
	var blocked atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	r := newAdmissionRig(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/workers" && blocked.Load() {
			close(entered)
			<-release
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	a, err := r.open(t, "A", 1, 0, r.addr)
	require.NoError(t, err)
	a.run(statusHandler())
	blocked.Store(true)
	done := make(chan error, 1)
	go func() { _, err := r.open(t, "A", 2, 0, r.addr); done <- err }()
	<-entered
	defer func() { close(release); <-done }()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	status := make(chan error, 1)
	go func() { _, err := r.links.entries["a"].remote.Status(ctx); status <- err }()
	select {
	case err := <-status:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("admission mutex ignored command context")
	}
}
func TestBeginShutdownClosesCommandStreams(t *testing.T) {
	r := newAdmissionRig(t, nil)
	stream, err := r.open(t, "A", 1, 0, r.addr)
	require.NoError(t, err)
	peer := stream.run(statusHandler())
	r.p.BeginShutdown()
	select {
	case <-peer.Done():
	case <-time.After(time.Second):
		t.Fatal("shutdown left HTTP/2 stream open")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, r.server.Config.Shutdown(ctx))
}
func TestLinkAdmissionFencesSequenceInstanceAndDeathCycle(t *testing.T) {
	r := newAdmissionRig(t, nil)
	stream, err := r.open(t, "A", 2, 0, r.addr)
	require.NoError(t, err)
	stream.run(statusHandler())
	_, err = r.open(t, "A", 1, 0, r.addr)
	require.Error(t, err, "stale sequence accepted")
	_, err = r.open(t, "B", 1, 0, r.addr)
	require.Error(t, err, "live instance replaced")
	_, err = r.open(t, "A", 3, 1, r.addr)
	require.Error(t, err, "future death cycle accepted")
	r.declareDead()
	replacement := netip.MustParseAddrPort("127.0.0.1:17002")
	stream, err = r.open(t, "B", 1, 0, replacement)
	require.NoError(t, err)
	stream.run(statusHandler())
	require.Equal(t, replacement, r.p.workers["a"].addr)
}
func TestControlOwnerMismatchIsNotSent(t *testing.T) {
	r := newAdmissionRig(t, nil)
	stream, err := r.open(t, "A", 1, 0, r.addr)
	require.NoError(t, err)
	var called atomic.Bool
	stream.run(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Store(true) }))
	lease, err := r.p.store.Claim(context.Background(), "call", netip.MustParseAddrPort("127.0.0.1:17002"), time.Minute)
	require.NoError(t, err)
	_, err = r.links.entries["a"].remote.ExportSession(lease.SessionID)
	require.ErrorIs(t, err, workerlink.ErrNotSent)
	require.False(t, errors.Is(err, privateapi.ErrUncertain))
	require.False(t, called.Load())
}

func TestDeadWorkerReconnectRequiresRejoinAcknowledgement(t *testing.T) {
	r := newAdmissionRig(t, nil)
	stream, err := r.open(t, "A", 1, 0, r.addr)
	require.NoError(t, err)
	stream.run(statusHandler())
	r.declareDead()
	stream, err = r.open(t, "A", 2, 0, r.addr)
	require.NoError(t, err)
	stream.run(statusHandler())
	r.p.mu.Lock()
	require.True(t, r.p.workers["a"].observation.dead)
	require.False(t, r.p.workers["a"].observation.knowledge().eligible)
	r.p.mu.Unlock()
	// A hello is not an ACK. The challenge forces the existing drop-all step
	// before the next heartbeat can make the same process eligible again.
	hb := &RemoteHeartbeats{URL: r.server.URL, Name: "a"}
	require.ErrorIs(t, hb.Heartbeat(r.addr), mediaworker.ErrRejoinRequired)
	r.p.mu.Lock()
	require.True(t, r.p.workers["a"].observation.dead)
	r.p.mu.Unlock()
	require.NoError(t, hb.Heartbeat(r.addr))
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	require.False(t, r.p.workers["a"].observation.dead)
	require.True(t, r.p.workers["a"].observation.knowledge().eligible)
	require.EqualValues(t, 1, r.p.workers["a"].observation.deathCycle)
}

type blackholeConn struct {
	net.Conn
	blocked *atomic.Bool
}

func (c *blackholeConn) Read(p []byte) (int, error) {
	for {
		n, err := c.Conn.Read(p)
		if err != nil || !c.blocked.Load() {
			return n, err
		}
	}
}
func (c *blackholeConn) Write(p []byte) (int, error) {
	if c.blocked.Load() {
		return len(p), nil
	}
	return c.Conn.Write(p)
}
func TestBlackholedLinkDoesNotDeclareWorkerDead(t *testing.T) {
	r := newAdmissionRig(t, nil)
	var blocked atomic.Bool
	transport := r.client.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := new(net.Dialer).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &blackholeConn{Conn: c, blocked: &blocked}, nil
	}
	stream, err := r.open(t, "A", 1, 0, r.addr)
	require.NoError(t, err)
	peer := stream.run(statusHandler())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.p.Run(ctx) }()
	defer func() { cancel(); <-done }()
	blocked.Store(true)
	hb := &RemoteHeartbeats{URL: r.server.URL, Name: "a"}
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for down := false; !down; {
		select {
		case <-peer.Done():
			down = true
		case <-ticker.C:
			require.NoError(t, hb.Heartbeat(r.addr))
		case <-deadline:
			t.Fatal("silent command connection did not expire")
		}
	}
	r.p.mu.Lock()
	require.False(t, r.p.workers["a"].observation.dead)
	require.Zero(t, r.p.workers["a"].observation.deathCycle)
	require.Zero(t, r.p.eventCount)
	r.p.mu.Unlock()
	blocked.Store(false)
	// A closing HTTP/2 connection can reject the first new stream. The real
	// DialOut loop retries admission; it never retries an accepted command.
	sequence := uint64(1)
	require.Eventually(t, func() bool {
		require.NoError(t, hb.Heartbeat(r.addr))
		sequence++
		stream, err = r.open(t, "A", sequence, 0, r.addr)
		return err == nil
	}, 3*time.Second, 40*time.Millisecond)
	stream.run(statusHandler())
	_, err = r.links.entries["a"].remote.Status(context.Background())
	require.NoError(t, err)
}
