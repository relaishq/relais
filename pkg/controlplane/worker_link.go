package controlplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/workerlink"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
)

// ErrWorkerLinkUnavailable is a retryable command-path failure, never evidence of death.
var ErrWorkerLinkUnavailable = workerlink.ErrUnavailable

// ErrWorkerCommandNotSent proves a worker command was rejected before transmission.
var ErrWorkerCommandNotSent = privateapi.ErrNotSent

// LinkHello replaces the initial status dial. Sequence increases per attempt
// within one process instance, so a delayed old stream cannot replace a new one.
type LinkHello struct {
	Instance   string                   `json:"instance"`
	Sequence   uint64                   `json:"sequence"`
	Release    string                   `json:"release"`
	Contract   int                      `json:"contract"`
	DeathCycle uint64                   `json:"death_cycle"`
	Status     mediaworker.WorkerStatus `json:"status"`
}
type LinkWelcome struct {
	Token      string `json:"token"`
	DeathCycle uint64 `json:"death_cycle"`
}
type LinkAddress struct {
	Address netip.AddrPort `json:"address"`
}

type linkEntry struct {
	mu             sync.Mutex
	peer           *workerlink.Peer
	instance       string
	activeInstance string
	sequence       uint64
	remote         *RemoteWorker
}

// WorkerLinks owns only command availability. It never calls detect, Transfer
// or heartbeat. Names are fixed before the private listener starts.
type WorkerLinks struct {
	plane   *Plane
	relay   *RemoteRelay
	entries map[string]*linkEntry
}

func (p *Plane) ConfigureDialOut(names []string, r *RemoteRelay) *WorkerLinks {
	links := &WorkerLinks{plane: p, relay: r, entries: make(map[string]*linkEntry)}
	for _, name := range names {
		entry := &linkEntry{}
		entry.remote = &RemoteWorker{URL: "http://dial-out", Client: &http.Client{Timeout: privateapi.Timeout, Transport: &linkTransport{links: links, name: name, entry: entry}}}
		links.entries[name] = entry
	}
	p.links = links
	return links
}

func (l *WorkerLinks) Handler(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /private/workers/{name}/link", l.serve)
	mux.HandleFunc("GET /private/workers/{name}/status", func(w http.ResponseWriter, r *http.Request) {
		e := l.entries[r.PathValue("name")]
		if e == nil {
			http.NotFound(w, r)
			return
		}
		status, err := e.remote.Status(r.Context())
		if err != nil {
			privateapi.Error(w, err, RemoteErrors)
			return
		}
		privateapi.Write(w, status)
	})
	mux.HandleFunc("GET /private/workers/{name}/metrics", func(w http.ResponseWriter, r *http.Request) {
		e := l.entries[r.PathValue("name")]
		if e == nil {
			http.NotFound(w, r)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, e.remote.URL+"/metrics", nil)
		if err != nil {
			privateapi.Error(w, err, RemoteErrors)
			return
		}
		reply, err := e.remote.Client.Do(req)
		if err != nil {
			privateapi.Error(w, err, RemoteErrors)
			return
		}
		defer func() { _ = reply.Body.Close() }()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.WriteHeader(reply.StatusCode)
		_, _ = io.Copy(w, reply.Body)
	})
	mux.Handle("/", next)
	return mux
}
func (l *WorkerLinks) setAvailable(name string, entry *linkEntry, peer *workerlink.Peer, up bool) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.peer != peer {
		return
	}
	p := l.plane
	p.mu.Lock()
	defer p.mu.Unlock()
	if w := p.workers[name]; w != nil {
		o := &w.observation
		if up {
			if o.linkSeen {
				o.linkReconnects++
			}
			o.linkSeen = true
			if !o.linkDownSince.IsZero() {
				o.linkDownTotal += time.Since(o.linkDownSince)
				o.linkDownSince = time.Time{}
			}
		} else if o.linkDownSince.IsZero() {
			o.linkDownSince = time.Now()
		}
		if o.linkDown == up {
			o.linkDown = !up
			p.loadVersion++
			p.observationChangedLocked()
		}
	}

}
func (l *WorkerLinks) serve(rw http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	entry := l.entries[name]
	if entry == nil {
		http.Error(rw, "worker not configured for dial-out", http.StatusForbidden)
		return
	}
	if req.ProtoMajor != 2 {
		http.Error(rw, "HTTP/2 required", http.StatusHTTPVersionNotSupported)
		return
	}
	if !l.plane.controlAuthority() {
		http.Error(rw, "control not ready", http.StatusServiceUnavailable)
		return
	}
	rc := http.NewResponseController(rw)
	_ = rc.SetReadDeadline(time.Now().Add(privateapi.Timeout))
	defer func() { _ = rc.SetReadDeadline(time.Time{}) }()
	var hello LinkHello
	if err := workerlink.ReadValue(req.Body, "hello", &hello); err != nil {
		return
	}
	if hello.Instance == "" || hello.Sequence == 0 || hello.Contract != workerlink.ContractVersion || !hello.Status.InventoryKnown || hello.Status.Truncated {
		http.Error(rw, "invalid link hello", http.StatusConflict)
		return
	}
	p := l.plane
	entry.mu.Lock()
	p.mu.Lock()
	old := p.workers[name]
	canReplace := old != nil && old.observation.canReplace(old.reserved)
	cycle := uint64(0)
	if old != nil {
		cycle = old.observation.deathCycle
	}
	p.mu.Unlock()
	if entry.instance != "" && (entry.instance != hello.Instance && old != nil && !canReplace || entry.instance == hello.Instance && hello.Sequence <= entry.sequence) {
		entry.mu.Unlock()
		http.Error(rw, "stale worker instance or link", http.StatusConflict)
		return
	}
	if hello.DeathCycle > cycle && old != nil && entry.activeInstance != "" {
		entry.mu.Unlock()
		http.Error(rw, "stale control death cycle", http.StatusConflict)
		return
	}
	instanceChanged := entry.activeInstance != "" && entry.activeInstance != hello.Instance
	entry.instance, entry.sequence = hello.Instance, hello.Sequence
	entry.mu.Unlock()
	token := randomToken()
	if err := l.relay.ExpectWorker(req.Context(), token); err != nil {
		http.Error(rw, "relay unavailable", http.StatusServiceUnavailable)
		return
	}
	rw.Header().Set("Content-Type", "application/octet-stream")
	rw.WriteHeader(http.StatusOK)
	if err := workerlink.WriteValue(rw, "welcome", LinkWelcome{Token: token, DeathCycle: cycle}); err != nil {
		return
	}
	var address LinkAddress
	if err := workerlink.ReadValue(req.Body, "address", &address); err != nil {
		return
	}
	if !address.Address.IsValid() {
		return
	}
	entry.mu.Lock()
	if entry.instance != hello.Instance || entry.sequence != hello.Sequence {
		entry.mu.Unlock()
		return
	}
	p.mu.Lock()
	old = p.workers[name]
	duplicate := p.owner(address.Address)
	canReplace = old != nil && old.observation.canReplace(old.reserved)
	p.mu.Unlock()
	if duplicate != nil && duplicate != old || old != nil && (old.addr != address.Address || instanceChanged) && !canReplace || !p.controlAuthority() {
		entry.mu.Unlock()
		return
	}
	if err := l.relay.AddBoundWorker(req.Context(), address.Address, token); err != nil {
		entry.mu.Unlock()
		return
	}
	if old == nil {
		if err := p.Register(name, address.Address, entry.remote); err != nil {
			entry.mu.Unlock()
			return
		}
	} else if old.addr != address.Address || old.worker != entry.remote || instanceChanged {
		// Changing address/instance is the existing explicit replacement path.
		if !canReplace || p.Replace(name, address.Address, entry.remote) != nil {
			entry.mu.Unlock()
			return
		}
	}
	if err := workerlink.WriteValue(rw, "ready", struct{}{}); err != nil {
		entry.mu.Unlock()
		return
	}
	peer := workerlink.New(req.Body, rw, func() { _ = rc.SetWriteDeadline(time.Now()); _ = req.Body.Close() }, nil)
	previous := entry.peer
	entry.peer = peer
	entry.activeInstance = hello.Instance
	entry.mu.Unlock()
	if previous != nil {
		previous.Close()
	}
	p.mu.Lock()
	w := p.workers[name]
	w.observation.draining = w.observation.draining || hello.Status.Draining
	w.observation.relayRegistered()
	p.mu.Unlock()
	l.setAvailable(name, entry, peer, true)
	defer l.setAvailable(name, entry, peer, false)
	_ = rc.SetReadDeadline(time.Time{})
	_ = peer.Run(req.Context())
}

// Drop closes only the command stream. Separate heartbeats and calls continue.
func (l *WorkerLinks) Drop(name string) {
	if e := l.entries[name]; e != nil {
		e.mu.Lock()
		peer := e.peer
		e.mu.Unlock()
		if peer != nil {
			peer.Close()
		}
	}
}

type linkTransport struct {
	links *WorkerLinks
	name  string
	entry *linkEntry
}

func (t *linkTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.entry.mu.Lock()
	peer := t.entry.peer
	t.entry.mu.Unlock()
	if peer == nil {
		return nil, errors.Join(ErrCreateNotSent, workerlink.ErrNotSent, workerlink.ErrUnavailable)
	}
	p := t.links.plane
	p.mu.Lock()
	w := p.workers[t.name]
	blocked := w == nil || w.observation.dead || w.observation.recovering || w.observation.linkDown
	p.mu.Unlock()
	if blocked && req.URL.Path != "/status" {
		return nil, errors.Join(ErrCreateNotSent, workerlink.ErrNotSent, workerlink.ErrUnavailable)
	}
	if strings.HasPrefix(req.URL.Path, "/sessions/") {
		id := strings.Split(strings.TrimPrefix(req.URL.Path, "/sessions/"), "/")[0]
		lease, err := p.store.Get(req.Context(), id)
		if err != nil {
			return nil, errors.Join(workerlink.ErrNotSent, err)
		}
		if lease.Worker != w.addr {
			return nil, errors.Join(workerlink.ErrNotSent, mediaworker.ErrStaleTenure)
		}
		req = req.Clone(req.Context())
		req.Header.Set("X-Relais-Epoch", strconv.FormatUint(lease.Epoch, 10))
	}
	resp, err := peer.RoundTrip(req)
	if errors.Is(err, workerlink.ErrNotSent) {
		err = errors.Join(ErrCreateNotSent, err)
	}
	return resp, err
}

func (r *RemoteRelay) ExpectWorker(ctx context.Context, token string) error {
	return privateapi.Do(ctx, remoteClient(r.Client), r.URL, http.MethodPost, "/workers/expect", relay.WorkerRequest{Token: token}, nil, relay.RemoteErrors)
}
func (r *RemoteRelay) AddBoundWorker(ctx context.Context, address netip.AddrPort, token string) error {
	return privateapi.Do(ctx, remoteClient(r.Client), r.URL, http.MethodPost, "/workers", relay.WorkerRequest{Address: address, Token: token}, nil, relay.RemoteErrors)
}

// WaitWorkerLink bounds planned source operations before any relay hold.
func (p *Plane) waitWorkerLink(ctx context.Context, address netip.AddrPort) error {
	ctx, cancel := context.WithTimeout(ctx, privateapi.Timeout)
	defer cancel()
	for {
		p.mu.Lock()
		w := p.owner(address)
		down := w != nil && w.observation.linkDown
		changed := p.workerChanged
		p.mu.Unlock()
		if !down {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(workerlink.ErrUnavailable, ctx.Err())
		case <-changed:
		}
	}
}

// WorkerLinkStatus is private command-path diagnostics, separate from liveness.
type WorkerLinkStatus struct {
	Connected   bool    `json:"connected"`
	Reconnects  uint64  `json:"reconnects"`
	DownSeconds float64 `json:"down_seconds"`
}

func (p *Plane) linkStatusLocked(w *registration) *WorkerLinkStatus {
	if p.links == nil || p.links.entries[w.name] == nil {
		return nil
	}
	o := &w.observation
	duration := o.linkDownTotal
	if !o.linkDownSince.IsZero() {
		duration += time.Since(o.linkDownSince)
	}
	return &WorkerLinkStatus{Connected: !o.linkDown, Reconnects: o.linkReconnects, DownSeconds: duration.Seconds()}
}

func (p *Plane) workerUsesLink(name string, worker Worker) bool {
	if p.links == nil {
		return false
	}
	entry := p.links.entries[name]
	return entry != nil && entry.remote == worker
}
