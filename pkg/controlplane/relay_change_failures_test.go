package controlplane

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type relayProtocolWorker struct {
	*fakeWorker
	abortErr   error
	abortBlock bool
}

func (w *relayProtocolWorker) ConfigureRelays(context.Context, []mediaworker.RelaySlot) error {
	return nil
}
func (w *relayProtocolWorker) RestartSession(context.Context, string, mediaworker.RestartRequest) (string, error) {
	return "answer", nil
}
func (w *relayProtocolWorker) AbortRelayRestart(ctx context.Context, _ string, _ uint64) error {
	if w.abortBlock {
		<-ctx.Done()
		return ctx.Err()
	}
	return w.abortErr
}
func (w *relayProtocolWorker) CreateSessionAtRelay(ctx context.Context, offer, slot string) (string, string, sessionstore.Lease, error) {
	id, answer, err := w.CreateSession(ctx, offer)
	if err != nil {
		return "", "", sessionstore.Lease{}, err
	}
	lease, err := w.store.Get(ctx, id)
	if err == nil {
		err = w.store.(sessionstore.RelayAssignments).CASRelay(ctx, lease, sessionstore.RelayAssignment{}, sessionstore.RelayAssignment{Slot: slot})
	}
	return id, answer, lease, err
}
func relayFailurePlane(t *testing.T, store sessionstore.Store) (*Plane, *relayProtocolWorker, *fakeWorker, string) {
	t.Helper()
	p := New(&fakeRelay{}, store)
	a := &relayProtocolWorker{fakeWorker: &fakeWorker{store: store, addr: netip.MustParseAddrPort("127.0.0.1:17001"), running: map[string]bool{}}}
	b := &relayProtocolWorker{fakeWorker: &fakeWorker{store: store, addr: netip.MustParseAddrPort("127.0.0.1:17002"), running: map[string]bool{}}}
	require.NoError(t, p.Register("a", a.addr, a))
	require.NoError(t, p.Register("b", b.addr, b))
	var slots []RelayBinding
	for i, id := range []string{"r1", "r2"} {
		addr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(18001+i))
		slots = append(slots, RelayBinding{Slot: mediaworker.RelaySlot{ID: id, Leg: addr, Public: addr}, Relay: &fakeRelay{}})
	}
	require.NoError(t, p.ConfigureRelaySet(context.Background(), slots))
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	c, err := p.lookup(id)
	require.NoError(t, err)
	c.stateMu.Lock()
	c.relayCapable = true
	c.stateMu.Unlock()
	return p, a, b.fakeWorker, id
}
func waitRelayPending(t *testing.T, store sessionstore.Store, id string) (sessionstore.Lease, sessionstore.RelayAssignment) {
	t.Helper()
	var lease sessionstore.Lease
	var assignment sessionstore.RelayAssignment
	require.Eventually(t, func() bool {
		var err error
		lease, assignment, err = store.(sessionstore.RelayAssignments).RelayOwner(context.Background(), id)
		return err == nil && assignment.Pending != ""
	}, time.Second, time.Millisecond)
	return lease, assignment
}
func TestRelayOwnerDeathClearsPending(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, _, id := relayFailurePlane(t, store)
		a.abortErr = mediaworker.ErrUnknownSession
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- p.ChangeRelay(ctx, id, "r2") }()
		waitRelayPending(t, store, id)
		cancel()
		require.Error(t, <-done)
		_, assignment, err := store.(sessionstore.RelayAssignments).RelayOwner(context.Background(), id)
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r1", Generation: 1}, assignment, "dead owner must not leave permanent pending state")
	})
}
func TestTakeoverPreemptsRelayChange(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "unreachable"}[blocked], func(t *testing.T) {
			forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
				p, a, b, id := relayFailurePlane(t, store)
				a.abortErr = mediaworker.ErrUnknownSession
				a.abortBlock = blocked
				p.workers["b"].worker = &takeoverWorker{fakeWorker: b}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				lease, err := store.Get(ctx, id)
				require.NoError(t, err)
				require.NoError(t, store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
				done := make(chan error, 1)
				go func() { done <- p.ChangeRelay(ctx, id, "r2") }()
				waitRelayPending(t, store, id)
				// Always settle the old operation, including on the pre-fix red run.
				defer func() {
					cancel()
					select {
					case <-done:
					case <-time.After(12 * time.Second):
						t.Error("relay operation did not stop")
					}
				}()
				source := p.workers["a"]
				p.mu.Lock()
				expireObservedWorker(t, source)
				p.mu.Unlock()
				start := time.Now()
				p.recovery.recover(ctx, source, lease, start)
				require.Less(t, time.Since(start), takeoverBudget)
				current, err := store.Get(context.Background(), id)
				require.NoError(t, err)
				require.Equal(t, b.addr, current.Worker)
				_, assignment, err := store.(sessionstore.RelayAssignments).RelayOwner(context.Background(), id)
				require.NoError(t, err)
				require.Empty(t, assignment.Pending)
				require.True(t, b.runs(id))
			})
		})
	}
}

func TestCreateDuringRelayDrain(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		drained := make(chan error, 1)
		go func() { drained <- p.DrainRelay(context.Background(), "r1") }()
		lease, assignment := waitRelayPending(t, store, id)
		defer func() {
			require.NoError(t, store.(sessionstore.RelayAssignments).CASRelay(context.Background(), lease, assignment, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}))
			require.NoError(t, <-drained)
		}()
		created := make(chan error, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		go func() { _, _, err := p.Create(ctx, "offer", "a"); created <- err }()
		select {
		case err := <-created:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("Create stalled behind relay drain")
		}
	})
}

func TestRelayCancellationKeepsSwitchedAssignment(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, _, id := relayFailurePlane(t, store)
		a.abortErr = mediaworker.ErrRelaySwitched
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- p.ChangeRelay(ctx, id, "r2") }()
		lease, pending := waitRelayPending(t, store, id)
		cancel()
		require.Error(t, <-done)
		_, got, err := store.(sessionstore.RelayAssignments).RelayOwner(context.Background(), id)
		require.NoError(t, err)
		require.Equal(t, pending, got, "an uncommitted switched path stays authorized; cancellation cannot roll it back")
		require.NoError(t, store.(sessionstore.RelayAssignments).CASRelay(context.Background(), lease, pending, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}))
	})
}
