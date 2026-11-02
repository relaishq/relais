package controlplane

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/workerlink"
	"github.com/relaishq/relais/pkg/sessionstore"
)

func TestLinkAvailabilityNeverDeclaresDeath(t *testing.T) {
	p := New(nil, sessionstore.NewMemory())
	links := p.ConfigureDialOut([]string{"a"}, nil)
	entry := links.entries["a"]
	addr := netip.MustParseAddrPort("127.0.0.1:17001")
	if err := p.Register("a", addr, entry.remote); err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	peer := workerlink.New(reader, writer, func() { _ = reader.Close() }, nil)
	entry.peer = peer
	links.setAvailable("a", entry, peer, true)
	if !p.workers["a"].observation.knowledge().eligible {
		t.Fatal("connected worker ineligible")
	}
	links.setAvailable("a", entry, peer, false)
	ctx := context.Background()
	if _, err := p.pick(ctx, "a", netip.AddrPort{}); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("disconnected target selected: %v", err)
	}
	// Independent heartbeats continue for the longest specified link outage.
	at := time.Now()
	o := &p.workers["a"].observation
	for tick := range 300 {
		now := at.Add(time.Duration(tick) * 100 * time.Millisecond)
		if _, _, err := o.heartbeat(workerHeartbeat{remote: true}, now); err != nil {
			t.Fatal(err)
		}
		if o.detect(now.Add(50*time.Millisecond), 400*time.Millisecond).recover {
			t.Fatal("link loss declared death")
		}
	}
	if o.deathCycle != 0 {
		t.Fatal("link loss advanced death cycle")
	}
	changed := p.workerChanged
	links.setAvailable("a", entry, peer, true)
	select {
	case <-changed:
	default:
		t.Fatal("reconnect did not wake target wait")
	}
	if !o.knowledge().eligible {
		t.Fatal("reconnected worker ineligible")
	}
	links.setAvailable("a", entry, peer, false)
	if !o.detect(at.Add(31*time.Second), 400*time.Millisecond).recover {
		t.Fatal("link down hid a real heartbeat death")
	}
}
func TestLinkReplacementCannotBeRevokedByOldStream(t *testing.T) {
	p := New(nil, sessionstore.NewMemory())
	links := p.ConfigureDialOut([]string{"a"}, nil)
	e := links.entries["a"]
	if err := p.Register("a", netip.MustParseAddrPort("127.0.0.1:17001"), e.remote); err != nil {
		t.Fatal(err)
	}
	old := workerlink.New(nil, nil, func() {}, nil)
	current := workerlink.New(nil, nil, func() {}, nil)
	e.peer = current
	links.setAvailable("a", e, current, true)
	links.setAvailable("a", e, old, false)
	if !p.workers["a"].observation.knowledge().eligible {
		t.Fatal("old link disabled the replacement")
	}
}
func TestAbsentLinkCreateIsDefiniteNotSent(t *testing.T) {
	p := New(nil, sessionstore.NewMemory())
	links := p.ConfigureDialOut([]string{"a"}, nil)
	_, _, err := links.entries["a"].remote.CreateSession(context.Background(), "offer")
	if !errors.Is(err, ErrCreateNotSent) || !errors.Is(err, workerlink.ErrNotSent) || errors.Is(err, privateapi.ErrUncertain) {
		t.Fatalf("wrong absent-link outcome: %v", err)
	}
}

func TestUnsentExportDoesNotFenceLiveSource(t *testing.T) {
	p, a, _, _ := setup(t)
	id, _, err := p.Create(context.Background(), "offer", "a")
	if err != nil {
		t.Fatal(err)
	}
	before, err := p.store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	p.workers["a"].worker = &exportErrorWorker{fakeWorker: a, err: workerlink.ErrNotSent}
	_, err = p.Move(context.Background(), id, "b")
	if !errors.Is(err, workerlink.ErrNotSent) {
		t.Fatalf("wrong move outcome: %v", err)
	}
	after, err := p.store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if before.Epoch != after.Epoch || before.Worker != after.Worker || !a.runs(id) {
		t.Fatal("unsent export fenced the live source")
	}
	if p.lostCount != 0 || p.eventCount != 0 {
		t.Fatal("unsent export counted as takeover or loss")
	}
}
