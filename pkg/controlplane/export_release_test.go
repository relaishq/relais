package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// exportSnapshot is a minimal, decodable export whose caller input names a
// 64-index receive window (ADR 0009).
func exportSnapshot(id string) []byte {
	return []byte(fmt.Sprintf(`{"Version":7,"State":{"Version":7,"ID":%q,"ICE":{"LocalUfrag":%q,"RemoteAddr":"127.0.0.1:5004"},"SRTP":{"Profile":7,"Inbound":{"42":12},"Received":{"42":5}}},"DTLSConnection":"AQ=="}`, id, id))
}

var exportedInput = map[uint32]relay.ConsumedInput{42: {High: 12, Window: 5, Exact: true}}

// exportingWorker returns real export bytes; resume maps them back to the ID.
type exportingWorker struct {
	*fakeWorker
	uncertainResume bool
}

func (w *exportingWorker) ExportSession(id string) ([]byte, error) {
	if _, err := w.fakeWorker.ExportSession(id); err != nil {
		return nil, err
	}
	return exportSnapshot(id), nil
}

func (w *exportingWorker) ResumeSession(data []byte, opts mediaworker.ResumeOptions) (string, error) {
	var snap struct{ State struct{ ID string } }
	if err := json.Unmarshal(data, &snap); err != nil {
		return "", err
	}
	if w.uncertainResume {
		return "", privateapi.Uncertain(errors.New("lost resume reply"))
	}
	opts.SequenceMargin = 0 // the fake accepts crash margins on rollback
	return w.fakeWorker.ResumeSession([]byte(snap.State.ID), opts)
}

type release struct {
	to       netip.AddrPort
	input    map[uint32]relay.ConsumedInput
	exported bool
}

// exportRelay records which release form the plane used.
type exportRelay struct {
	fakeRelay
	mu       sync.Mutex
	releases []release
}

func (r *exportRelay) ReleaseSession(_ string, to netip.AddrPort) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releases = append(r.releases, release{to: to})
	return 0, nil
}

func (r *exportRelay) ReleaseAfterExport(_ string, to netip.AddrPort, input map[uint32]relay.ConsumedInput) (relay.ReleaseReply, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releases = append(r.releases, release{to: to, input: input, exported: true})
	return relay.ReleaseReply{Packets: 3, Recovered: 2, Complete: true}, nil
}

func TestMoveReleasesAfterExactExport(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fail     func(a, b *exportingWorker, r *exportRelay)
		to       string
		exported bool
	}{
		{name: "moved", to: "b", exported: true},
		{name: "resume failed, rolled back to the same export", fail: func(_, b *exportingWorker, _ *exportRelay) { b.resumeError = true }, to: "a", exported: true},
		{name: "route failed, rolled back to the same export", fail: func(_, _ *exportingWorker, r *exportRelay) { r.fail = true }, to: "a", exported: true},
		// The source resumes from a store checkpoint with crash margins, which
		// may predate the export: the plain release keeps today's behavior.
		{name: "uncertain resume, crash-margin rollback", fail: func(_, b *exportingWorker, _ *exportRelay) { b.uncertainResume = true }, to: "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sessionstore.NewMemory()
			r := &exportRelay{}
			p := New(r, m)
			a := &exportingWorker{fakeWorker: &fakeWorker{store: m, addr: netip.MustParseAddrPort("127.0.0.1:1"), running: map[string]bool{}}}
			b := &exportingWorker{fakeWorker: &fakeWorker{store: m, addr: netip.MustParseAddrPort("127.0.0.1:2"), running: map[string]bool{}}}
			require.NoError(t, p.Register("a", a.addr, a))
			require.NoError(t, p.Register("b", b.addr, b))
			ctx := context.Background()
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			if tc.fail != nil {
				tc.fail(a, b, r)
			}
			if tc.name == "uncertain resume, crash-margin rollback" {
				lease, err := m.Get(ctx, id)
				require.NoError(t, err)
				require.NoError(t, m.PutState(ctx, lease, exportSnapshot(id)))
			}
			res, _ := p.Move(ctx, id, "b")
			r.mu.Lock()
			defer r.mu.Unlock()
			require.Len(t, r.releases, 1)
			want := map[string]netip.AddrPort{"a": a.addr, "b": b.addr}[tc.to]
			require.Equal(t, want, r.releases[0].to)
			require.Equal(t, tc.exported, r.releases[0].exported)
			if tc.exported {
				require.Equal(t, exportedInput, r.releases[0].input)
				require.Equal(t, 2, res.Result.RecoveredPackets)
				require.Equal(t, 3, res.Result.HeldPackets)
			}
		})
	}
}

// A relay without the ADR 0009 route answers 404 or 405 from its mux. Only
// then does RemoteRelay fall back to /release; domain and uncertain errors
// are returned unchanged.
func TestRemoteReleaseAfterExportFallback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		fallback bool
		err      error
	}{
		{name: "older relay: 404", status: http.StatusNotFound, fallback: true},
		{name: "older relay: 405", status: http.StatusMethodNotAllowed, fallback: true},
		{name: "domain error", status: http.StatusConflict, err: relay.ErrHoldExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var released, exported int
			mux := http.NewServeMux()
			mux.HandleFunc("POST /sessions/{id}/release", func(w http.ResponseWriter, _ *http.Request) {
				released++
				privateapi.Write(w, relay.ReleaseReply{Packets: 4})
			})
			mux.HandleFunc("/sessions/{id}/release-after-export", func(w http.ResponseWriter, req *http.Request) {
				exported++
				switch tc.status {
				case http.StatusConflict:
					privateapi.Error(w, relay.ErrHoldExpired, relay.RemoteErrors)
				default:
					http.Error(w, "older relay", tc.status)
				}
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			reply, err := (&RemoteRelay{URL: server.URL}).ReleaseAfterExport("call", netip.MustParseAddrPort("127.0.0.1:2"), exportedInput)
			require.Equal(t, 1, exported)
			if tc.fallback {
				require.NoError(t, err)
				require.Equal(t, 1, released)
				require.Equal(t, 4, reply.Packets)
				return
			}
			require.ErrorIs(t, err, tc.err)
			require.Zero(t, released)
		})
	}
	t.Run("uncertain transport error", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		server.Close()
		_, err := (&RemoteRelay{URL: server.URL}).ReleaseAfterExport("call", netip.MustParseAddrPort("127.0.0.1:2"), exportedInput)
		require.ErrorIs(t, err, privateapi.ErrUncertain)
	})
}

func TestRemoteMoveAfterExportFallback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		fallback bool
	}{
		{name: "older relay: 404", status: http.StatusNotFound, fallback: true},
		{name: "older relay: 405", status: http.StatusMethodNotAllowed, fallback: true},
		{name: "domain error", status: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var moved, exported int
			var request relay.RouteRequest
			mux := http.NewServeMux()
			mux.HandleFunc("POST /sessions/{id}/move", func(w http.ResponseWriter, req *http.Request) {
				moved++
				if privateapi.Read(w, req, &request) {
					w.WriteHeader(http.StatusNoContent)
				}
			})
			mux.HandleFunc("/sessions/{id}/move-after-export", func(w http.ResponseWriter, _ *http.Request) {
				exported++
				if tc.status == http.StatusConflict {
					privateapi.Error(w, relay.ErrHeld, relay.RemoteErrors)
					return
				}
				http.Error(w, "older relay", tc.status)
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			from, to := netip.MustParseAddrPort("127.0.0.1:1"), netip.MustParseAddrPort("127.0.0.1:2")
			// No known source: a route repair, never an empty-source drain.
			require.NoError(t, (&RemoteRelay{URL: server.URL}).MoveSessionAfterExport("call", netip.AddrPort{}, to, 7))
			require.Zero(t, exported)
			require.Equal(t, 1, moved)
			require.Equal(t, relay.RouteRequest{To: to, Epoch: 7, Repair: true}, request)
			moved, request = 0, relay.RouteRequest{}
			err := (&RemoteRelay{URL: server.URL}).MoveSessionAfterExport("call", from, to, 7)
			require.Equal(t, 1, exported)
			if !tc.fallback {
				require.ErrorIs(t, err, relay.ErrHeld)
				require.Zero(t, moved)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, moved)
			require.Equal(t, relay.RouteRequest{From: from, To: to, Epoch: 7}, request)
		})
	}
}

// exportMoveRelay also records post-export route moves.
type exportMoveRelay struct {
	exportRelay
	exportMoves [][2]netip.AddrPort
}

func (r *exportMoveRelay) MoveSessionAfterExport(id string, from, to netip.AddrPort, _ uint64) error {
	r.mu.Lock()
	r.exportMoves = append(r.exportMoves, [2]netip.AddrPort{from, to})
	r.mu.Unlock()
	return r.MoveSession(id, from, to)
}

// The retained uncertain-transfer path resumes from the exact export with
// margin zero, so it drains the source's output and releases after export.
func TestRetainedPlannedMoveReleasesAfterExport(t *testing.T) {
	m := sessionstore.NewMemory()
	r := &exportMoveRelay{}
	p := New(r, m)
	a := &exportingWorker{fakeWorker: &fakeWorker{store: m, addr: netip.MustParseAddrPort("127.0.0.1:1"), running: map[string]bool{}}}
	b := &exportingWorker{fakeWorker: &fakeWorker{store: m, addr: netip.MustParseAddrPort("127.0.0.1:2"), running: map[string]bool{}}}
	require.NoError(t, p.Register("a", a.addr, a))
	require.NoError(t, p.Register("b", b.addr, b))
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	p.store = &retainedMoveStore{Store: p.store}
	_, err = p.Move(ctx, id, "b")
	require.Error(t, err)
	r.mu.Lock()
	require.Empty(t, r.releases, "the hold waits for the retained transfer")
	r.mu.Unlock()
	source := p.workers["a"]
	pending := source.pending[id]
	require.NotNil(t, pending)
	p.takeover(ctx, source, pending.lease, time.Now())
	require.True(t, b.runs(id))
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Equal(t, [][2]netip.AddrPort{{a.addr, b.addr}}, r.exportMoves)
	require.Len(t, r.releases, 1)
	require.True(t, r.releases[0].exported)
	require.Equal(t, b.addr, r.releases[0].to)
	require.Equal(t, exportedInput, r.releases[0].input)
}

// A retained planned move whose route call failed retries with no known
// source. The retry must repair the route through the real relay HTTP API
// rather than send an empty source to move-after-export (implementation
// review P2): the old path returned 400 on every attempt and never adopted.
func TestRetainedPlannedMoveRetriesAfterRouteFailure(t *testing.T) {
	m := sessionstore.NewMemory()
	r, err := relay.New(relay.Config{Owners: m})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	var failRoute atomic.Bool
	failRoute.Store(true)
	var routes []string
	var mu sync.Mutex
	private := r.PrivateHandler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/hold"):
			w.WriteHeader(http.StatusNoContent) // fake workers cannot acknowledge a marker
			return
		case strings.HasSuffix(req.URL.Path, "/move-after-export") || strings.HasSuffix(req.URL.Path, "/move"):
			mu.Lock()
			routes = append(routes, req.URL.Path[strings.LastIndex(req.URL.Path, "/"):])
			mu.Unlock()
			if failRoute.CompareAndSwap(true, false) {
				privateapi.Error(w, relay.ErrHeld, relay.RemoteErrors)
				return
			}
		}
		private.ServeHTTP(w, req)
	}))
	t.Cleanup(server.Close)
	p := New(&RemoteRelay{URL: server.URL}, m)
	a := &fakeWorker{store: m, addr: netip.MustParseAddrPort("127.0.0.1:1"), running: map[string]bool{}}
	b := &fakeWorker{store: m, addr: netip.MustParseAddrPort("127.0.0.1:2"), running: map[string]bool{}}
	r.AddWorker(a.addr)
	r.AddWorker(b.addr)
	require.NoError(t, p.Register("a", a.addr, a))
	require.NoError(t, p.Register("b", b.addr, b))
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	p.store = &retainedMoveStore{Store: p.store}
	_, err = p.Move(ctx, id, "b")
	require.Error(t, err)
	source := p.workers["a"]
	for attempt := 0; attempt < 3 && !b.runs(id); attempt++ {
		pending := source.pending[id]
		require.NotNil(t, pending)
		p.takeover(ctx, source, pending.lease, time.Now())
	}
	require.True(t, b.runs(id), "a retry repairs the route and adopts the target")
	require.Empty(t, source.pending)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"/move-after-export", "/move"}, routes, "the failed drain is retried as a route repair")
}
