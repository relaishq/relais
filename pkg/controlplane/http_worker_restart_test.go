package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestHTTPWorkerRestartDuringControlDowntimeRequiresTakeover(t *testing.T) {
	for _, mode := range []string{"control-rebuild", "paused-death-check", "status-observation", "pre-identity-record", "rejoin-record-error"} {
		t.Run(mode, func(t *testing.T) { testHTTPWorkerMissedCrash(t, mode) })
	}
}

func testHTTPWorkerMissedCrash(t *testing.T, mode string) {
	ctx := context.Background()
	store := &failingControlJournal{Memory: sessionstore.NewMemory()}
	addr := netip.MustParseAddrPort("127.0.0.1:23531")
	lease, err := store.Claim(ctx, "crashed-call", addr, time.Minute)
	require.NoError(t, err)
	require.NoError(t, store.PutState(ctx, lease, takeoverSnapshot(t, lease.SessionID, 0)))
	returning := &fakeWorker{store: store, addr: addr, next: 100, running: map[string]bool{}}
	var crashed atomic.Bool
	crashed.Store(mode != "paused-death-check" && mode != "status-observation")
	// Control missed the crash. Its saved registration names the old process;
	// the endpoint now serves a new process with no installed sessions.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			status := mediaworker.WorkerStatus{Address: addr, Instance: "before-crash", InventoryKnown: true,
				Sessions: 1, Inventory: []mediaworker.InstalledSession{{ID: lease.SessionID, Owner: addr, Epoch: lease.Epoch}}}
			if crashed.Load() {
				status.Instance, status.Inventory, status.Sessions = "after-crash", nil, 0
			}
			privateapi.Write(w, status)
			return
		}
		unitWorkerHandler(returning).ServeHTTP(w, r)
	}))
	defer worker.Close()
	p := New(&fakeRelay{}, store)
	authorizeLongControl(t, p)
	p.SetRelayReady(true)
	predecessor := "before-crash"
	if mode == "pre-identity-record" {
		predecessor = ""
	}
	require.NoError(t, p.controlRecord(ctx, "worker:a", FixedWorker{Name: "a", URL: worker.URL, Address: addr, Instance: predecessor}))
	target := &takeoverWorker{fakeWorker: &fakeWorker{store: store, addr: netip.MustParseAddrPort("127.0.0.1:23532"), running: map[string]bool{}}}
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			privateapi.Write(w, mediaworker.WorkerStatus{Address: target.addr, InventoryKnown: true})
		case "/resume":
			var req mediaworker.ResumeRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			id, err := target.ResumeSession(req.State, mediaworker.ResumeOptions{Lease: req.Lease, SequenceMargin: req.SequenceMargin, SRTCPIndexMargin: req.SRTCPIndexMargin})
			require.NoError(t, err)
			privateapi.Write(w, mediaworker.ResumeReply{ID: id})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer targetServer.Close()
	fixed := []FixedWorker{{Name: "a", URL: worker.URL}, {Name: "b", URL: targetServer.URL}}
	require.NoError(t, p.rebuild(ctx, fixed))
	// Dispatch synchronously through the real HTTP handler and JSON transport;
	// the test does not put an asynchronous callback under the heartbeat budget.
	hb := &RemoteHeartbeats{URL: "http://127.0.0.1", Name: "a", Instance: "after-crash",
		Client: &http.Client{Transport: handlerTransport{handler: p.ProcessHandler()}}}
	if mode == "paused-death-check" || mode == "status-observation" {
		status, err := p.Status(ctx)
		require.NoError(t, err)
		require.False(t, status.Workers[0].Dead)
		crashed.Store(true)
		if mode == "status-observation" {
			require.NoError(t, p.discoverWorker(ctx, fixed[0], nil))
		} else {
			require.Error(t, hb.Heartbeat(addr), "a new process must not hide the missed crash")
		}
	}
	status, err := p.Status(ctx)
	require.NoError(t, err)
	t.Logf("after rebuild: worker_dead=%t calls=%d takeovers=%d", status.Workers[0].Dead, len(status.Calls), len(status.Takeovers))
	require.Len(t, status.Calls, 1, "the old lease must remain available for crash takeover")
	require.True(t, status.Workers[0].Dead, "empty replacement must not make old leases appear live")
	_, _, err = p.Create(ctx, "offer", "a")
	require.Error(t, err, "placement must stay fenced during takeover")
	saved, err := p.linkRecord(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, predecessor, saved.Instance, "retain predecessor evidence until takeover settles")
	source := p.workers["a"]
	require.True(t, p.recoverWorker(ctx, source, time.Now()))
	p.mu.Lock()
	source.observation.recoveryFinished(true)
	p.mu.Unlock()
	require.True(t, target.runs(lease.SessionID))
	recovered, err := store.Get(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, target.addr, recovered.Worker)
	require.ErrorIs(t, hb.Heartbeat(addr), mediaworker.ErrRejoinRequired)
	require.True(t, p.workers["a"].observation.dead, "the challenge is not an acknowledgement")
	if mode == "rejoin-record-error" {
		store.fail.Store(true)
		require.Error(t, hb.Heartbeat(addr))
		require.True(t, p.workers["a"].observation.dead, "failed identity persistence must not admit the worker")
		store.fail.Store(false)
		saved, err = p.linkRecord(ctx, "a")
		require.NoError(t, err)
		require.Equal(t, predecessor, saved.Instance)
	}
	require.NoError(t, hb.Heartbeat(addr))
	saved, err = p.linkRecord(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, "after-crash", saved.Instance)
	newID, _, err := p.Create(ctx, "offer", "")
	require.NoError(t, err)
	require.True(t, returning.runs(newID), "ordinary placement must use the returning HTTP worker")
	status, err = p.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Takeovers, 1)
	require.False(t, status.Takeovers[0].Lost)
	require.Zero(t, status.LostCount)
}
