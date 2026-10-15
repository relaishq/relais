package callharness

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/workerprobe"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/framecache"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// privateLeg is a loopback UDP proxy on one worker's private relay leg. The
// relay and the worker each see only the proxy, so a test can reorder caller
// datagrams relative to the drain marker exactly where #127 loses them.
type privateLeg struct {
	conn          *net.UDPConn
	relay, worker netip.AddrPort
	// intercept sees every relay-to-worker datagram. Returning true means it
	// took a copy and will deliver (or drop) it itself.
	intercept atomic.Pointer[func([]byte) bool]
	done      chan struct{}
}

func newPrivateLeg(t *testing.T) *privateLeg {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	return &privateLeg{conn: conn, done: make(chan struct{})}
}

func (l *privateLeg) addr() netip.AddrPort { return l.conn.LocalAddr().(*net.UDPAddr).AddrPort() }

func (l *privateLeg) run() {
	defer close(l.done)
	buf := make([]byte, 65535)
	for {
		n, from, err := l.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if from != l.relay {
			_, _ = l.conn.WriteToUDPAddrPort(buf[:n], l.relay)
			continue
		}
		if f := l.intercept.Load(); f != nil && (*f)(append([]byte(nil), buf[:n]...)) {
			continue
		}
		_, _ = l.conn.WriteToUDPAddrPort(buf[:n], l.worker)
	}
}

// deliver sends a datagram the interceptor took, late, to the worker.
func (l *privateLeg) deliver(datagram []byte) {
	_, _ = l.conn.WriteToUDPAddrPort(datagram, l.worker)
}

func (l *privateLeg) setIntercept(f func([]byte) bool) {
	if f == nil {
		l.intercept.Store(nil)
		return
	}
	l.intercept.Store(&f)
}

// legAddrs maps each real worker address to its private-leg proxy.
type legAddrs map[netip.AddrPort]netip.AddrPort

func (a legAddrs) proxy(addr netip.AddrPort) netip.AddrPort {
	if p, ok := a[addr]; ok {
		return p
	}
	return addr
}

// legOwners lets the relay route each session to its owner's proxy.
type legOwners struct {
	store sessionstore.Store
	addrs legAddrs
}

func (o legOwners) Owner(ctx context.Context, id string) (netip.AddrPort, error) {
	owner, err := o.store.Owner(ctx, id)
	return o.addrs.proxy(owner), err
}

func (o legOwners) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	lease, err := o.store.Get(ctx, id)
	lease.Worker = o.addrs.proxy(lease.Worker)
	return lease, err
}

// legRelay translates the plane's real worker addresses to proxies on every
// call through the relay's real private HTTP API (RemoteRelay).
type legRelay struct {
	remote *controlplane.RemoteRelay
	addrs  legAddrs
}

func (r legRelay) HoldSession(ctx context.Context, id string, from netip.AddrPort) error {
	return r.remote.HoldSession(ctx, id, r.addrs.proxy(from))
}
func (r legRelay) MoveSession(id string, from, to netip.AddrPort) error {
	return r.remote.MoveSession(id, r.addrs.proxy(from), r.addrs.proxy(to))
}
func (r legRelay) MoveSessionAfterExport(id string, from, to netip.AddrPort, epoch uint64) error {
	return r.remote.MoveSessionAfterExport(id, r.addrs.proxy(from), r.addrs.proxy(to), epoch)
}
func (r legRelay) MoveSessionAtEpoch(id string, from, to netip.AddrPort, epoch uint64) error {
	return r.remote.MoveSessionAtEpoch(id, r.addrs.proxy(from), r.addrs.proxy(to), epoch)
}
func (r legRelay) ReleaseSession(id string, to netip.AddrPort) (int, error) {
	return r.remote.ReleaseSession(id, r.addrs.proxy(to))
}
func (r legRelay) ReleaseAfterExport(id string, to netip.AddrPort, input map[uint32]relay.ConsumedInput) (relay.ReleaseReply, error) {
	return r.remote.ReleaseAfterExport(id, r.addrs.proxy(to), input)
}
func (r legRelay) ForgetSession(id string) { r.remote.ForgetSession(id) }
func (r legRelay) BeginReplay(ctx context.Context, id string, from netip.AddrPort, inbound map[uint32]uint64) (relay.ReplayPlan, error) {
	return r.remote.BeginReplay(ctx, id, r.addrs.proxy(from), inbound)
}
func (r legRelay) ReplaySession(ctx context.Context, id string, to netip.AddrPort) (relay.ReplayResult, error) {
	return r.remote.ReplaySession(ctx, id, r.addrs.proxy(to))
}
func (r legRelay) ForgetSessionTenure(ctx context.Context, lease sessionstore.Lease) error {
	lease.Worker = r.addrs.proxy(lease.Worker)
	return r.remote.ForgetSessionTenure(ctx, lease)
}

type reorderOptions struct {
	workers       int
	buffer        bool
	agents        []mediaworker.AgentConfig
	relayHandler  func(http.Handler) http.Handler
	workerHandler func(int, http.Handler) http.Handler
	store         sessionstore.Store
}

// reorderTopology is a relay, a real control plane and real workers, each
// worker behind its own private-leg proxy.
type reorderTopology struct {
	relay   *relay.Relay
	plane   *controlplane.Plane
	workers []*mediaworker.Worker
	private []*httptest.Server
	legs    []*privateLeg
	h       *Harness
}

func startReorderTopology(t *testing.T, opts reorderOptions) *reorderTopology {
	t.Helper()
	t.Cleanup(workerprobe.Enable())
	store := opts.store
	if store == nil {
		store = sessionstore.NewMemory()
	}
	count := max(opts.workers, 2)
	addrs := legAddrs{}
	sys := &reorderTopology{}
	for range count {
		sys.legs = append(sys.legs, newPrivateLeg(t))
	}
	// The race detector and the proxies' extra hop can slow a loaded worker's
	// read loop well past the production marker bounds; this harness must
	// observe every drain rather than an aborted move.
	cfg := relay.Config{Owners: legOwners{store: store, addrs: addrs}, HoldTimeout: 5 * time.Second, BarrierTimeout: 3 * time.Second, OutputDrainTimeout: 2 * time.Second}
	if opts.buffer {
		cfg.Buffer = &relay.BufferConfig{}
	}
	r, err := relay.New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	sys.relay = r
	handler := r.PrivateHandler()
	if opts.relayHandler != nil {
		handler = opts.relayHandler(handler)
	}
	rs := httptest.NewServer(handler)
	t.Cleanup(rs.Close)
	plane := controlplane.NewWithConfig(legRelay{remote: &controlplane.RemoteRelay{URL: rs.URL}, addrs: addrs}, store, controlplane.Config{FrameCache: framecache.NewMemory(framecache.Limits{})})
	sys.plane = plane
	for i, leg := range sys.legs {
		var agentConfig mediaworker.AgentConfig
		if i < len(opts.agents) {
			agentConfig = opts.agents[i]
		}
		w, err := mediaworker.New(mediaworker.Config{Agent: agentConfig, DisableFrameCache: true, Relay: &mediaworker.RelayConfig{Addr: leg.addr(), PublicAddr: r.PublicAddr(), Owners: store}})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		leg.relay, leg.worker = r.WorkerAddr(), w.LocalAddr()
		addrs[w.LocalAddr()] = leg.addr()
		go leg.run()
		t.Cleanup(func() { _ = leg.conn.Close(); <-leg.done })
		r.AddWorker(leg.addr())
		private := w.PrivateHandler()
		if opts.workerHandler != nil {
			private = opts.workerHandler(i, private)
		}
		ws := httptest.NewServer(private)
		t.Cleanup(ws.Close)
		require.NoError(t, plane.Register(strconv.Itoa(i), w.LocalAddr(), &controlplane.RemoteWorker{URL: ws.URL}))
		w.StartHeartbeats(plane)
		sys.workers = append(sys.workers, w)
		sys.private = append(sys.private, ws)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- plane.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	ps := httptest.NewServer(plane.Handler())
	t.Cleanup(ps.Close)
	h, err := Start(Options{External: &ExternalTopology{SignalingURL: ps.URL + "/calls", RelayAddr: r.PublicAddr()}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	sys.h = h
	return sys
}

// workerMetric reads one counter from a worker's real private /metrics.
func (s *reorderTopology) workerMetric(t *testing.T, worker int, name string) float64 {
	t.Helper()
	values, err := scrapeMetrics(s.private[worker].URL + "/metrics")
	require.NoError(t, err)
	value, ok := values[name]
	require.True(t, ok, "worker %d exports %s", worker, name)
	return value
}

var errLegacyRelay = errors.New("older relay without the ADR 0009 route")

// legacyRelayHandler answers the ADR 0009 route the way an older relay's mux
// does, so the plane must fall back to the existing /release.
func legacyRelayHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/release-after-export") || strings.HasSuffix(req.URL.Path, "/move-after-export") {
			http.Error(w, errLegacyRelay.Error(), http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, req)
	})
}

// scrapeMetrics reads unlabeled Prometheus text samples.
func scrapeMetrics(url string) (map[string]float64, error) {
	resp, err := http.Get(url) //nolint:gosec,noctx // loopback test endpoint
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	values := make(map[string]float64)
	for _, line := range strings.Split(string(data), "\n") {
		name, value, ok := strings.Cut(line, " ")
		if !ok || strings.HasPrefix(line, "#") || strings.Contains(name, "{") {
			continue
		}
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
			values[name] = parsed
		}
	}
	return values, nil
}

// workerInputRejections sums a session's decryption failures and replay
// rejections. Before ADR 0009 both were counted as decryption failures; a
// gate that required zero still requires zero of both, so a duplicate
// delivery keeps failing it.
func workerInputRejections(w *mediaworker.Worker, id string) (uint64, error) {
	failures, err := w.SessionDecryptFailures(id)
	if err != nil {
		return 0, err
	}
	replays, err := w.SessionReplayRejections(id)
	return failures + replays, err
}
