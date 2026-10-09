package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type fakeWorker struct {
	store         sessionstore.Store
	addr          netip.AddrPort
	running       map[string]bool
	mu            sync.Mutex
	next          int
	exportGate    chan struct{}
	exportCancel  context.CancelFunc
	exported      chan struct{}
	resumeError   bool
	resumeStarted chan struct{}
	resumeGate    chan struct{}
	createStarted chan struct{}
	createGate    chan struct{}
}

func (w *fakeWorker) CreateSession(ctx context.Context, _ string) (string, string, error) {
	if w.createStarted != nil {
		close(w.createStarted)
		select {
		case <-w.createGate:
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.next++
	id := fmt.Sprintf("%s-%d", w.addr, w.next)
	if _, err := w.store.Claim(ctx, id, w.addr, time.Minute); err != nil {
		return "", "", err
	}
	w.running[id] = true
	return id, "answer", nil
}

func (w *fakeWorker) EndSession(id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.running[id] {
		return mediaworker.ErrUnknownSession
	}
	delete(w.running, id)
	lease, err := w.store.Get(context.Background(), id)
	if err != nil {
		return err
	}
	return w.store.Release(context.Background(), lease)
}

func (w *fakeWorker) ExportSession(id string) ([]byte, error) {
	w.mu.Lock()
	if !w.running[id] {
		w.mu.Unlock()
		return nil, mediaworker.ErrUnknownSession
	}
	delete(w.running, id)
	w.mu.Unlock()
	if w.exportCancel != nil {
		w.exportCancel()
	}
	if w.exported != nil {
		w.exported <- struct{}{}
	}
	if w.exportGate != nil {
		<-w.exportGate
	}
	return []byte(id), nil
}

func (w *fakeWorker) ResumeSession(data []byte, opts mediaworker.ResumeOptions) (string, error) {
	if w.resumeStarted != nil {
		close(w.resumeStarted)
		<-w.resumeGate
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.resumeError {
		return "", errors.New("injected resume failure")
	}
	if opts.SequenceMargin != 0 {
		return "", errors.New("nonzero margin")
	}
	if _, err := w.store.Renew(context.Background(), opts.Lease, time.Minute); err != nil {
		return "", err
	}
	w.running[string(data)] = true
	return string(data), nil
}

func (w *fakeWorker) runs(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running[id]
}

type fakeRelay struct {
	fail        bool
	failReverse bool
	mu          sync.Mutex
	moves       [][2]netip.AddrPort
}

func (r *fakeRelay) HoldSession(_ context.Context, _ string, _ netip.AddrPort) error {
	return nil
}

func (r *fakeRelay) ReleaseSession(_ string, _ netip.AddrPort) (int, error) {
	return 0, nil
}

func (r *fakeRelay) MoveSession(_ string, from, to netip.AddrPort) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.moves = append(r.moves, [2]netip.AddrPort{from, to})
	if r.fail || r.failReverse && from.Port() == 2 {
		return errors.New("injected relay failure")
	}
	return nil
}

func setup(t *testing.T) (*Plane, *fakeWorker, *fakeWorker, *fakeRelay) {
	t.Helper()
	m := sessionstore.NewMemory()
	r := &fakeRelay{}
	p := New(r, m)
	a := &fakeWorker{store: m, addr: netip.MustParseAddrPort("127.0.0.1:1"), running: map[string]bool{}}
	b := &fakeWorker{store: m, addr: netip.MustParseAddrPort("127.0.0.1:2"), running: map[string]bool{}}
	require.NoError(t, p.Register("a", a.addr, a))
	require.NoError(t, p.Register("b", b.addr, b))
	return p, a, b, r
}

func TestMoveAndRollback(t *testing.T) {
	for _, failure := range []string{"none", "resume", "relay", "transfer", "rollback", "reverse"} {
		t.Run(failure, func(t *testing.T) {
			p, a, b, r := setup(t)
			ctx := context.Background()
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			if failure == "resume" || failure == "rollback" || failure == "reverse" {
				b.resumeError = true
			}
			if failure == "rollback" {
				a.resumeError = true
			}
			if failure == "reverse" {
				r.failReverse = true
			}
			if failure == "relay" {
				r.fail = true
			}
			if failure == "transfer" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				a.exportCancel = cancel // cancellation after the source is already flushed
			}

			res, err := p.Move(ctx, id, "b")
			switch failure {
			case "none":
				require.NoError(t, err)
				require.False(t, a.runs(id))
				require.True(t, b.runs(id))
				status, err := p.Status(ctx)
				require.NoError(t, err)
				require.Equal(t, "b", status.Calls[0].Owner)
				require.EqualValues(t, 2, status.Calls[0].Epoch)
				require.NotNil(t, status.Calls[0].LastMove)
			case "rollback":
				require.ErrorContains(t, err, "call lost")
				require.False(t, a.runs(id))
				require.False(t, b.runs(id))
				require.Empty(t, p.calls)
			default:
				require.ErrorContains(t, err, "rolled back")
				require.True(t, res.Result.RolledBack)
				require.True(t, a.runs(id))
				require.False(t, b.runs(id))
				lease, err := p.store.Get(context.Background(), id)
				require.NoError(t, err)
				require.Equal(t, a.addr, lease.Worker)
				if failure == "transfer" {
					require.EqualValues(t, 1, lease.Epoch)
				} else {
					require.EqualValues(t, 3, lease.Epoch)
				}
			}
		})
	}
}

func TestOverlappingMoveFailsFastAndHangupWaits(t *testing.T) {
	p, a, b, _ := setup(t)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	a.exportGate = make(chan struct{})
	a.exported = make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := p.Move(ctx, id, "b")
		done <- err
	}()
	<-a.exported
	_, err = p.Move(ctx, id, "b")
	require.ErrorIs(t, err, ErrMoveInProgress)
	_, err = p.Drain(ctx, "a")
	require.ErrorIs(t, err, ErrMoveInProgress)
	hungup := make(chan error, 1)
	go func() { hungup <- p.End(ctx, id) }()
	select {
	case <-hungup:
		t.Fatal("hangup did not wait for move")
	case <-time.After(10 * time.Millisecond):
	}
	close(a.exportGate)
	require.NoError(t, <-done)
	require.NoError(t, <-hungup)
	require.False(t, b.runs(id))
}

func TestSelectionAndDrainWithoutTarget(t *testing.T) {
	p, _, _, _ := setup(t)
	ctx := context.Background()
	_, _, err := p.Create(ctx, "offer", "")
	require.NoError(t, err)
	_, _, err = p.Create(ctx, "offer", "")
	require.NoError(t, err)
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, status.Workers[0].Calls)
	require.Equal(t, 1, status.Workers[1].Calls)
	p.mu.Lock()
	p.workers["b"].draining = true
	p.mu.Unlock()
	_, err = p.Drain(ctx, "a")
	require.ErrorIs(t, err, ErrNoTarget)
	status, err = p.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Calls, 2)
	require.Nil(t, status.Calls[0].LastMove)
	require.Nil(t, status.Calls[1].LastMove)
	_, _, err = p.Create(ctx, "offer", "a")
	require.ErrorIs(t, err, ErrNoTarget)
}

func TestPrunesCallsEndedOutsideControlPlane(t *testing.T) {
	for _, action := range []string{"status", "end"} {
		t.Run(action, func(t *testing.T) {
			p, a, _, _ := setup(t)
			id, _, err := p.Create(context.Background(), "offer", "a")
			require.NoError(t, err)
			require.NoError(t, a.EndSession(id))
			if action == "status" {
				status, err := p.Status(context.Background())
				require.NoError(t, err)
				require.Empty(t, status.Calls)
			} else {
				require.Error(t, p.End(context.Background(), id))
			}
			require.Empty(t, p.calls, "ended calls must not accumulate metadata")
		})
	}
}

func TestDrainWaitsForIncomingCreate(t *testing.T) {
	p, a, b, _ := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a.createStarted, a.createGate = make(chan struct{}), make(chan struct{})
	created := make(chan string, 1)
	createErr := make(chan error, 1)
	go func() {
		id, _, err := p.Create(ctx, "offer", "a")
		created <- id
		createErr <- err
	}()
	<-a.createStarted
	drained := make(chan []MoveResult, 1)
	drainErr := make(chan error, 1)
	go func() {
		results, err := p.Drain(ctx, "a")
		drained <- results
		drainErr <- err
	}()
	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.workers["a"].draining
	}, time.Second, time.Millisecond)
	_, _, err := p.Create(ctx, "offer", "a")
	require.ErrorIs(t, err, ErrNoTarget)
	close(a.createGate)
	require.NoError(t, <-createErr)
	id := <-created
	require.NoError(t, <-drainErr)
	results := <-drained
	require.Len(t, results, 1)
	require.Equal(t, id, results[0].ID)
	require.Empty(t, results[0].Error)
	require.True(t, b.runs(id))
	require.False(t, a.runs(id))
}

func TestEndPrunesMissingSessionWithLiveLease(t *testing.T) {
	p, a, _, _ := setup(t)
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	a.mu.Lock()
	delete(a.running, id)
	a.mu.Unlock()
	require.ErrorIs(t, p.End(context.Background(), id), mediaworker.ErrUnknownSession)
	require.Empty(t, p.calls)
	_, err = p.store.Get(context.Background(), id)
	require.ErrorIs(t, err, sessionstore.ErrNotFound)
}
