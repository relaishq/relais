package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func authorizeLongControl(t *testing.T, p *Plane) {
	t.Helper()
	require.NoError(t, p.EnableRestart())
	lease, err := p.restart.store.ClaimControl(context.Background(), randomToken(), 30*time.Second)
	require.NoError(t, err)
	p.restart.guard.Store(&controlGuard{lease: lease, deadline: time.Now().Add(25 * time.Second)})
	p.restart.healthy.Store(true)
	p.restart.ready.Store(true)
}
func TestDialOutIdentitySurvivesControlRestart(t *testing.T) {
	store := sessionstore.NewMemory()
	first := newAdmissionRig(t, nil, store)
	authorizeLongControl(t, first.p)
	stream, err := first.open(t, "A", 1, 0, first.addr)
	require.NoError(t, err)
	stream.run(statusHandler())
	records, err := store.ControlRecords(context.Background(), first.p.restart.guard.Load().lease)
	require.NoError(t, err)
	var saved FixedWorker
	require.NoError(t, json.Unmarshal(records["worker:a"], &saved))
	var raw map[string]any
	require.NoError(t, json.Unmarshal(records["worker:a"], &raw))
	assert.Equal(t, "A", raw["instance"])
	first.p.BeginShutdown()
	require.NoError(t, store.ReleaseControl(context.Background(), first.p.restart.guard.Load().lease))
	next := newAdmissionRig(t, nil, store)
	authorizeLongControl(t, next.p)
	_, err = next.open(t, "B", 1, 0, next.addr)
	require.Error(t, err, "durable identity must also guard admission before bootstrap registers the worker")
	// This is the restart registration path, including unavailable link status.
	fixed := FixedWorker{Name: "a", URL: "dial-out"}
	require.NoError(t, next.p.registerObservedWorker(context.Background(), fixed, &saved, next.links.entries["a"].remote, mediaworker.WorkerStatus{}, ErrWorkerLinkUnavailable))
	_, err = next.open(t, "B", 1, 0, next.addr)
	require.Error(t, err, "a new control must not forget the live worker's identity")
	_, err = next.open(t, "A", 1, 0, next.addr)
	require.Error(t, err, "accepted sequence watermark must survive restart")
	stream, err = next.open(t, "A", 2, 3, next.addr)
	require.NoError(t, err, "same live instance must be admitted after restart, including its known cycle")
	stream.run(statusHandler())
	next.p.mu.Lock()
	cycle := next.p.workers["a"].observation.deathCycle
	next.p.mu.Unlock()
	require.EqualValues(t, 3, cycle)
	_, err = next.open(t, "A", 3, 4, next.addr)
	require.Error(t, err, "future cycle is refused once the new control has accepted a link")
}

func TestPendingRebindRestoresBeforeLinkAdmission(t *testing.T) {
	r := newAdmissionRig(t, nil)
	authorizeLongControl(t, r.p)
	old := netip.MustParseAddrPort("127.0.0.1:17002")
	require.NoError(t, r.p.controlRecord(context.Background(), "worker:a", FixedWorker{Name: "a", URL: "dial-out", Address: r.addr, Instance: "A", Sequence: 1, PreviousAddress: &old}))
	require.NoError(t, r.p.controlRecord(context.Background(), "drain:a", true))
	stream, err := r.open(t, "A", 2, 0, r.addr)
	require.NoError(t, err)
	require.True(t, stream.ready.Activate, "first reconnect must restore an unfinished address activation")
	peer := stream.run(statusHandler())
	client := &http.Client{Transport: peer}
	r.p.restart.ready.Store(false)
	err = privateapi.Do(context.Background(), client, "http://control", "POST", "/leg-ready", LinkAddress{Address: r.addr}, nil, RemoteErrors)
	require.Error(t, err, "activation must wait for rebuild to finish")
	r.p.restart.ready.Store(true)
	require.NoError(t, privateapi.Do(context.Background(), client, "http://control", "POST", "/leg-ready", LinkAddress{Address: r.addr}, nil, RemoteErrors))
	r.p.mu.Lock()
	pending := r.p.workers["a"].previousAddr.IsValid()
	draining := r.p.workers["a"].observation.draining
	r.p.mu.Unlock()
	require.False(t, pending)
	require.False(t, draining)
}

func TestRebindingAdmitsDuringBootstrap(t *testing.T) {
	r := newAdmissionRig(t, nil)
	authorizeLongControl(t, r.p)
	require.NoError(t, r.p.controlRecord(context.Background(), "worker:a", FixedWorker{Name: "a", URL: "dial-out", Address: r.addr, Instance: "A", Sequence: 1}))
	r.p.restart.ready.Store(false)
	next := netip.MustParseAddrPort("127.0.0.1:17003")
	stream, err := r.open(t, "A", 2, 0, next)
	require.NoError(t, err, "changed NAT mapping must not prevent the link needed by rebuild")
	stream.run(statusHandler())
	require.Equal(t, r.addr, stream.ready.Address, "keep heartbeat identity until drain finishes")
	require.False(t, stream.ready.Activate)
	r.p.mu.Lock()
	draining := r.p.workers["a"].observation.draining
	r.p.mu.Unlock()
	require.True(t, draining)
}

func TestPendingRebindKeepsOriginalCleanupAddress(t *testing.T) {
	r := newAdmissionRig(t, nil)
	authorizeLongControl(t, r.p)
	original := netip.MustParseAddrPort("127.0.0.1:17002")
	require.NoError(t, r.p.controlRecord(context.Background(), "worker:a", FixedWorker{Name: "a", URL: "dial-out", Address: r.addr, Instance: "A", Sequence: 1, PreviousAddress: &original}))
	next := netip.MustParseAddrPort("127.0.0.1:17003")
	stream, err := r.open(t, "A", 2, 0, next)
	require.NoError(t, err)
	stream.run(statusHandler())
	require.Equal(t, r.addr, stream.ready.Address, "finish pending activation before starting another address change")
	require.True(t, stream.ready.Activate)
	saved, err := r.p.linkRecord(context.Background(), "a")
	require.NoError(t, err)
	require.Equal(t, &original, saved.PreviousAddress, "original relay registry entry must still be removed")
}
