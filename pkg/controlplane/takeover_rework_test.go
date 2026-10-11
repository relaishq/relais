package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type recoveryErrorStore struct {
	sessionstore.Store
	step   string
	fail   bool
	cancel context.CancelFunc
	target netip.AddrPort
}

func (s *recoveryErrorStore) ListByWorker(ctx context.Context, addr netip.AddrPort) ([]sessionstore.Lease, error) {
	if s.fail && s.step == "pick" && addr == s.target {
		if s.cancel != nil {
			s.cancel()
			return nil, ctx.Err()
		}
		return nil, errors.New("transient pick store timeout")
	}
	return s.Store.ListByWorker(ctx, addr)
}
func (s *recoveryErrorStore) Transfer(ctx context.Context, lease sessionstore.Lease, to netip.AddrPort, ttl time.Duration) (sessionstore.Lease, error) {
	if s.fail && s.step == "transfer" {
		return sessionstore.Lease{}, errors.New("transient transfer store timeout")
	}
	return s.Store.Transfer(ctx, lease, to, ttl)
}
func (s *recoveryErrorStore) GetState(ctx context.Context, id string) ([]byte, error) {
	if s.fail && s.step == "snapshot" {
		return nil, errors.New("transient snapshot store timeout")
	}
	return s.Store.GetState(ctx, id)
}

func TestTakeoverCancellationDuringPickPreservesLease(t *testing.T) {
	p, _, baseB, _ := setup(t)
	p.workers["b"].worker = &takeoverWorker{fakeWorker: baseB}
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(context.Background(), id)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(context.Background(), lease, takeoverSnapshot(t, id, 0)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backing := &recoveryErrorStore{Store: p.store, step: "pick", fail: true, cancel: cancel, target: baseB.addr}
	p.store = backing
	source := p.workers["a"]
	source.dead = true
	require.False(t, p.recoverWorker(ctx, source, time.Now()))
	current, err := p.store.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, lease, current)
	status, err := p.Status(context.Background())
	require.NoError(t, err)
	require.Len(t, status.Calls, 1)
	require.Empty(t, status.Takeovers)
	require.Zero(t, status.LostCount)
	backing.fail = false
	require.True(t, p.recoverWorker(context.Background(), source, time.Now()))
	status, err = p.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, "b", status.Calls[0].Owner)
	require.Zero(t, status.LostCount)
}

func TestTakeoverTransientErrorsRetainTransferredLeaseForRetry(t *testing.T) {
	for _, step := range []string{"pick", "transfer", "snapshot", "route"} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			p, _, baseB, r := setup(t)
			p.workers["b"].worker = &takeoverWorker{fakeWorker: baseB}
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
			backing := &recoveryErrorStore{Store: p.store, step: step, fail: true, target: baseB.addr}
			p.store = backing
			r.fail = step == "route"
			source := p.workers["a"]
			source.dead = true
			require.False(t, p.recoverWorker(ctx, source, time.Now()))
			current, err := p.store.Get(ctx, id)
			require.NoError(t, err, "transient failure must not release either tenure")
			if step == "snapshot" || step == "route" {
				require.Equal(t, baseB.addr, current.Worker)
			}
			status, err := p.Status(ctx)
			require.NoError(t, err)
			require.Len(t, status.Calls, 1)
			require.Empty(t, status.Takeovers)
			require.Zero(t, status.LostCount)
			require.Len(t, source.pending, 1)
			backing.fail, r.fail = false, false
			require.True(t, p.recoverWorker(ctx, source, time.Now()), "must enumerate unfinished transfers too")
			status, err = p.Status(ctx)
			require.NoError(t, err)
			require.Equal(t, "b", status.Calls[0].Owner)
			require.Len(t, status.Takeovers, 1)
			require.False(t, status.Takeovers[0].Lost)
			require.Empty(t, source.pending)
		})
	}
}

func TestMoveAfterHardKillKeepsSnapshotRecoverable(t *testing.T) {
	ctx := context.Background()
	p, a, baseB, _ := setup(t)
	p.workers["b"].worker = &takeoverWorker{fakeWorker: baseB}
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
	a.mu.Lock()
	delete(a.running, id) // crash loses memory, not store ownership
	a.mu.Unlock()
	_, err = p.Move(ctx, id, "b")
	require.ErrorIs(t, err, mediaworker.ErrUnknownSession)
	_, err = p.lookup(id)
	require.NoError(t, err)
	p.workers["a"].dead = true
	require.True(t, p.recoverWorker(ctx, p.workers["a"], time.Now()))
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, "b", status.Calls[0].Owner)
	require.Zero(t, status.LostCount)
}

// Match the production snapshot envelope so coordination reads the same
// counter-budget fields as ResumeSession. DTLS bytes are dummy at this fake
// protocol boundary; crypto restoration is exercised by mediaworker tests.
type retrySnapshot struct {
	Version int
	State   struct {
		Version int
		ID      string
		ICE     struct {
			LocalUfrag string
			RemoteAddr netip.AddrPort
		}
		SRTP  struct{ Profile uint16 }
		Audio struct {
			MID              string
			Packets          uint64
			AdvanceSinceSend uint32
		}
	}
	DTLSConnection []byte
}

func takeoverSnapshot(t *testing.T, id string, advance uint32) []byte {
	t.Helper()
	var state retrySnapshot
	state.Version, state.State.Version = 6, 6
	state.State.ID, state.State.ICE.LocalUfrag = id, id
	state.State.ICE.RemoteAddr = netip.MustParseAddrPort("127.0.0.1:9000")
	state.State.SRTP.Profile = 7
	state.State.Audio.MID, state.State.Audio.Packets = "0", 1
	state.State.Audio.AdvanceSinceSend = advance
	state.DTLSConnection = []byte{1}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	return data
}

type retryTarget struct {
	*fakeWorker
	fail bool
	seen []uint32
}

func (w *retryTarget) ResumeSession(data []byte, opts mediaworker.ResumeOptions) (string, error) {
	var state retrySnapshot
	if err := json.Unmarshal(data, &state); err != nil {
		return "", err
	}
	state.State.Audio.AdvanceSinceSend += uint32(opts.SequenceMargin)
	w.seen = append(w.seen, state.State.Audio.AdvanceSinceSend)
	// Production ResumeSession persists adjusted counters before adopt, so a
	// target dying at adoption leaves these adjusted bytes for the next retry.
	adjusted, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	if err := w.store.PutState(context.Background(), opts.Lease, adjusted); err != nil {
		return "", err
	}
	if w.fail {
		return "", mediaworker.ErrClosed
	}
	w.mu.Lock()
	w.running[state.State.ID] = true
	w.mu.Unlock()
	return state.State.ID, nil
}

func TestTakeoverRetriesAlternateTargetsWithinMarginBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		margin   uint16
		advance  uint32
		attempts int
	}{
		{"default-two-attempts", 8192, 0, 2},
		{"retained-advance-one-attempt", 8192, 8192, 1},
		{"large-margin-one-attempt", 12000, 0, 1},
		{"small-margin-three-attempts", 4096, 0, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, baseB, _ := setup(t)
			p.config.SequenceMargin = tc.margin
			b := &retryTarget{fakeWorker: baseB, fail: true}
			p.workers["b"].worker = b
			c := &retryTarget{fakeWorker: &fakeWorker{store: p.store, addr: netip.MustParseAddrPort("127.0.0.1:3"), running: map[string]bool{}}, fail: tc.attempts != 2}
			d := &retryTarget{fakeWorker: &fakeWorker{store: p.store, addr: netip.MustParseAddrPort("127.0.0.1:4"), running: map[string]bool{}}}
			require.NoError(t, p.Register("c", c.addr, c))
			require.NoError(t, p.Register("d", d.addr, d))
			ctx := context.Background()
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, tc.advance)))
			p.workers["a"].dead = true
			started := time.Now()
			require.True(t, p.recoverWorker(ctx, p.workers["a"], started))
			require.Less(t, time.Since(started), 1500*time.Millisecond)
			require.Equal(t, []uint32{tc.advance + uint32(tc.margin)}, b.seen)
			require.False(t, b.runs(id))
			status, err := p.Status(ctx)
			require.NoError(t, err)
			if tc.attempts >= 2 {
				require.Equal(t, []uint32{tc.advance + 2*uint32(tc.margin)}, c.seen)
				require.Zero(t, status.LostCount)
				if tc.attempts == 2 {
					require.True(t, c.runs(id))
					require.Equal(t, "c", status.Calls[0].Owner)
					require.Empty(t, d.seen)
				} else {
					require.False(t, c.runs(id))
					require.True(t, d.runs(id))
					require.Equal(t, "d", status.Calls[0].Owner)
					require.Equal(t, []uint32{3 * uint32(tc.margin)}, d.seen)
				}
			} else {
				require.Empty(t, c.seen, "retained advance and caller reserve forbid a second attempt")
				require.Empty(t, d.seen)
				require.Empty(t, status.Calls)
				require.EqualValues(t, 1, status.LostCount)
			}
		})
	}
}

func TestTakeoverRecentEventsBoundedAndCountsRetained(t *testing.T) {
	p, baseA, baseB, _ := setup(t)
	p.workers["a"].worker = &takeoverWorker{fakeWorker: baseA}
	p.workers["b"].worker = &takeoverWorker{fakeWorker: baseB}
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
	for i := range 300 {
		lease, err = p.store.Get(ctx, id)
		require.NoError(t, err)
		source := p.owner(lease.Worker)
		source.dead = true
		p.takeover(ctx, source, lease, time.Unix(int64(i), 0))
		source.dead = false
	}
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Takeovers, 256)
	require.Equal(t, time.Unix(44, 0), status.Takeovers[0].Start)
	require.Equal(t, time.Unix(299, 0), status.Takeovers[255].Start)
	require.EqualValues(t, 300, status.Calls[0].TakeoverCount)
	require.EqualValues(t, 300, status.Calls[0].MoveCount)
	require.Equal(t, "takeover", status.Calls[0].LastMoveKind)
	require.NotNil(t, status.Calls[0].LastMove)
	require.Zero(t, status.LostCount)
}

type cancelledResumeTarget struct {
	*takeoverWorker
	calls int
}

func (w *cancelledResumeTarget) ResumeSession(_ []byte, _ mediaworker.ResumeOptions) (string, error) {
	w.calls++
	return "", context.Canceled
}

func TestPendingTakeoverVanishedLeaseRecordsLossAndForgetsRoute(t *testing.T) {
	for _, step := range []string{"transfer-expiry", "route-expiry", "cancelled-expiry", "snapshot-vanished"} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			p, _, baseB, r := setup(t)
			target := &cancelledResumeTarget{takeoverWorker: &takeoverWorker{fakeWorker: baseB}}
			p.workers["b"].worker = target.takeoverWorker
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
			switch step {
			case "transfer-expiry":
				p.store = &recoveryErrorStore{Store: p.store, step: "transfer", fail: true}
			case "route-expiry":
				r.fail = true
			case "cancelled-expiry":
				p.workers["b"].worker = target
			case "snapshot-vanished":
				p.store = &recoveryErrorStore{Store: p.store, step: "snapshot", fail: true}
			}
			source := p.workers["a"]
			source.dead = true
			require.False(t, p.recoverWorker(ctx, source, time.Now()))
			if step == "cancelled-expiry" {
				// Confirmation retries use the same tenure and payload, without consuming
				// another margin. Expiry below must publish the terminal outcome.
				for range 3 {
					require.False(t, p.recoverWorker(ctx, source, time.Now()))
				}
				require.Equal(t, 4, target.calls, "uncertain adoption retries the same request until settled")
			}
			require.Len(t, source.pending, 1)
			require.Zero(t, p.lostCount)
			require.Empty(t, r.forgotten)
			current, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			if step == "snapshot-vanished" {
				require.NoError(t, p.store.Release(ctx, current))
			} else {
				_, err = p.store.Renew(ctx, current, time.Millisecond)
				require.NoError(t, err)
				time.Sleep(5 * time.Millisecond)
			}
			require.True(t, p.recoverWorker(ctx, source, time.Now()))
			status, err := p.Status(ctx)
			require.NoError(t, err)
			require.Empty(t, source.pending)
			require.Empty(t, status.Calls)
			require.EqualValues(t, 1, status.LostCount)
			require.Len(t, status.Takeovers, 1)
			require.True(t, status.Takeovers[0].Lost)
			require.Contains(t, status.Takeovers[0].Error, "lease vanished")
			require.Equal(t, []string{id}, r.forgotten)
			// Additional ticks must not count or forget the same loss twice.
			require.True(t, p.recoverWorker(ctx, source, time.Now()))
			require.EqualValues(t, 1, p.lostCount)
			require.Equal(t, []string{id}, r.forgotten)
		})
	}
}

type adoptedLostReply struct{ *retryTarget }

func (w *adoptedLostReply) ResumeSession(data []byte, opts mediaworker.ResumeOptions) (string, error) {
	_, err := w.retryTarget.ResumeSession(data, opts)
	if err != nil {
		return "", err
	}
	return "", context.DeadlineExceeded
}
func TestUncertainResumeIneligibleTargetUsesThirdWorker(t *testing.T) {
	for _, reason := range []string{"dead", "draining", "replaced"} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			p, _, baseB, _ := setup(t)
			b := &adoptedLostReply{&retryTarget{fakeWorker: baseB}}
			p.workers["b"].worker = b
			c := &retryTarget{fakeWorker: &fakeWorker{store: p.store, addr: netip.MustParseAddrPort("127.0.0.1:3"), running: map[string]bool{}}}
			require.NoError(t, p.Register("c", c.addr, c))
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
			source := p.workers["a"]
			source.dead = true
			require.False(t, p.recoverWorker(ctx, source, time.Now()))
			require.True(t, b.runs(id), "adoption commits before its reply is lost")
			switch reason {
			case "dead":
				p.workers["b"].dead = true
			case "draining":
				p.workers["b"].draining = true
			case "replaced":
				p.workers["b"] = &registration{name: "b", addr: baseB.addr, worker: b, lastHeartbeat: time.Now()}
			}
			require.True(t, p.recoverWorker(ctx, source, time.Now()))
			status, err := p.Status(ctx)
			require.NoError(t, err)
			require.Zero(t, status.LostCount)
			require.Len(t, status.Calls, 1)
			require.Equal(t, "c", status.Calls[0].Owner)
			require.True(t, c.runs(id))
			require.Equal(t, []uint32{16384}, c.seen, "reload adjusted state and apply another safe margin")
			current, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			require.Greater(t, current.Epoch, lease.Epoch+1)
		})
	}
}

type holdReleaseRelay struct {
	*fakeRelay
	released []netip.AddrPort
}

func (r *holdReleaseRelay) ReleaseSession(_ string, to netip.AddrPort) (int, error) {
	r.released = append(r.released, to)
	return 0, nil
}
func TestUncertainExportLeaseMismatchReleasesHeldPackets(t *testing.T) {
	ctx := context.Background()
	p, _, b, baseRelay := setup(t)
	r := &holdReleaseRelay{fakeRelay: baseRelay}
	p.relay = r
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	old, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	source := p.workers["a"]
	source.pending = map[string]*takeoverState{id: {lease: old, held: true}}
	current, err := p.store.Transfer(ctx, old, b.addr, time.Second)
	require.NoError(t, err)
	p.takeover(ctx, source, old, time.Now())
	require.Empty(t, source.pending)
	require.Equal(t, []netip.AddrPort{current.Worker}, r.released)
	require.Zero(t, p.lostCount)
}

type uncertainExportWorker struct{ *fakeWorker }

func (*uncertainExportWorker) ExportSession(string) ([]byte, error) {
	return nil, context.DeadlineExceeded
}

func TestUncertainExportHangupRecordsEnded(t *testing.T) {
	ctx := context.Background()
	p, a, _, _ := setup(t)
	p.workers["a"].worker = &uncertainExportWorker{fakeWorker: a}
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	p.store = &recoveryErrorStore{Store: p.store, step: "transfer", fail: true}
	_, err = p.Move(ctx, id, "b")
	require.Error(t, err)
	source := p.workers["a"]
	require.Contains(t, source.pending, id)
	require.NoError(t, p.End(ctx, id))
	require.NotContains(t, p.calls, id)
	p.takeover(ctx, source, source.pending[id].lease, time.Now())
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.Empty(t, source.pending)
	require.Len(t, status.Takeovers, 1)
	require.Equal(t, "ended", status.Takeovers[0].Kind)
	require.False(t, status.Takeovers[0].Lost)
	require.Zero(t, status.LostCount)
}

func TestUncertainExportTerminalOutcomeReleasesHold(t *testing.T) {
	for _, ended := range []bool{false, true} {
		t.Run(fmt.Sprintf("ended=%t", ended), func(t *testing.T) {
			ctx := context.Background()
			p, _, _, baseRelay := setup(t)
			r := &holdReleaseRelay{fakeRelay: baseRelay}
			p.relay = r
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			source := p.workers["a"]
			source.pending = map[string]*takeoverState{id: {call: p.calls[id], lease: lease, held: true}}
			if ended {
				require.NoError(t, p.End(ctx, id))
			} else {
				require.NoError(t, p.store.Release(ctx, lease))
			}
			p.takeover(ctx, source, lease, time.Now())
			require.Empty(t, source.pending)
			require.Equal(t, []netip.AddrPort{{}}, r.released, "terminal recovery must release its hold immediately")
			require.Equal(t, []string{id}, baseRelay.forgotten)
		})
	}
}

// Model a confirmed negative settlement: an uncertain candidate can no
// longer commit after the end released the last authoritative lease.
type endedCandidateStore struct{ sessionstore.Store }

func (*endedCandidateStore) Settle(context.Context, sessionstore.Lease) (sessionstore.Lease, bool, error) {
	return sessionstore.Lease{}, false, nil
}

type failedEndStore struct {
	sessionstore.Store
	getErr     error
	releaseErr error
}

func (s *failedEndStore) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	if s.getErr != nil {
		err := s.getErr
		s.getErr = nil
		return sessionstore.Lease{}, err
	}
	return s.Store.Get(ctx, id)
}

func (s *failedEndStore) Release(ctx context.Context, lease sessionstore.Lease) error {
	if s.releaseErr != nil {
		return s.releaseErr
	}
	return s.Store.Release(ctx, lease)
}

type failedEndWorker struct {
	*fakeWorker
	err error
}

func (w *failedEndWorker) EndSession(string) error { return w.err }

func TestFailedEndDoesNotHideMissingTakeoverSnapshot(t *testing.T) {
	for _, failure := range []string{"get", "cancelled", "unknown-owner", "end-session", "release"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			p, a, b, _ := setup(t)
			p.workers["b"].worker = &takeoverWorker{fakeWorker: b}
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			c := p.calls[id]
			source := p.workers["a"]
			endCtx := ctx
			switch failure {
			case "get":
				p.store = &failedEndStore{Store: p.store, getErr: errors.New("transient Get timeout")}
			case "cancelled":
				var cancel context.CancelFunc
				endCtx, cancel = context.WithCancel(ctx)
				cancel()
				p.store = &failedEndStore{Store: p.store, getErr: endCtx.Err()}
			case "unknown-owner":
				delete(p.workers, "a")
			case "end-session":
				source.worker = &failedEndWorker{fakeWorker: a, err: errors.New("EndSession unavailable")}
			case "release":
				source.worker = &failedEndWorker{fakeWorker: a, err: mediaworker.ErrUnknownSession}
				p.store = &failedEndStore{Store: p.store, releaseErr: errors.New("Release unavailable")}
			}
			require.Error(t, p.End(endCtx, id))
			p.workers["a"] = source
			source.worker = a
			source.dead = true
			require.True(t, p.recoverWorker(ctx, source, time.Now()))
			status, err := p.Status(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 1, status.LostCount)
			require.Len(t, status.Takeovers, 1)
			require.Equal(t, "takeover", status.Takeovers[0].Kind)
			require.True(t, status.Takeovers[0].Lost)
			require.Contains(t, status.Takeovers[0].Error, "no takeover snapshot")
			require.False(t, c.hungUp.Load(), "failed End must not mark a hang-up")
		})
	}
}

type gatedTakeoverSnapshot struct {
	sessionstore.Store
	entered chan struct{}
	release chan struct{}
}

func (s *gatedTakeoverSnapshot) GetState(ctx context.Context, id string) ([]byte, error) {
	close(s.entered)
	<-s.release
	return s.Store.GetState(ctx, id)
}

func TestEndWaitingForTakeoverDoesNotHideSnapshotLoss(t *testing.T) {
	ctx := context.Background()
	p, _, b, _ := setup(t)
	p.workers["b"].worker = &takeoverWorker{fakeWorker: b}
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	c := p.calls[id]
	source := p.workers["a"]
	source.dead = true
	gate := &gatedTakeoverSnapshot{Store: p.store, entered: make(chan struct{}), release: make(chan struct{})}
	p.store = gate
	recovered := make(chan bool, 1)
	go func() { recovered <- p.recoverWorker(ctx, source, time.Now()) }()
	<-gate.entered // Recovery holds the call lock and has already read the lease.
	done := make(chan error, 1)
	go func() { done <- p.End(ctx, id) }()
	// Sample while recovery is gated: waiting End cannot publish a hang-up.
	marked := false
	deadline := time.After(30 * time.Millisecond)
wait:
	for {
		marked = marked || c.hungUp.Load()
		select {
		case <-deadline:
			break wait
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(gate.release)
	require.True(t, <-recovered)
	require.ErrorIs(t, <-done, sessionstore.ErrNotFound)
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, status.LostCount)
	require.Len(t, status.Takeovers, 1)
	require.Equal(t, "takeover", status.Takeovers[0].Kind)
	require.True(t, status.Takeovers[0].Lost)
	require.Contains(t, status.Takeovers[0].Error, "no takeover snapshot")
	require.False(t, marked)
	require.False(t, c.hungUp.Load())
}

func TestCompletedHangupDoesNotRelabelOtherTakeoverFailures(t *testing.T) {
	for _, cause := range []error{
		fmt.Errorf("controlplane: no takeover snapshot: %w", sessionstore.ErrNotFound),
		errors.New("independent resume failure"),
	} {
		t.Run(cause.Error(), func(t *testing.T) {
			ctx := context.Background()
			p, _, _, _ := setup(t)
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			c := p.calls[id]
			require.NoError(t, p.End(ctx, id))
			require.True(t, c.hungUp.Load())
			res := MoveResult{Kind: "takeover", ID: id, Start: time.Now()}
			c.mu.Lock()
			p.completeTakeover(p.workers["a"], c, lease, &res, true, cause)
			c.mu.Unlock()
			require.Equal(t, "takeover", res.Kind)
			require.True(t, res.Lost)
			require.Equal(t, cause.Error(), res.Error)
			require.EqualValues(t, 1, p.lostCount)
		})
	}
}

func TestHangupDuringPendingTakeoverRecordsEnded(t *testing.T) {
	for _, step := range []string{"transfer", "route", "cancelled"} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			p, _, baseB, r := setup(t)
			target := &cancelledResumeTarget{takeoverWorker: &takeoverWorker{fakeWorker: baseB}}
			p.workers["b"].worker = target
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
			source := p.workers["a"]
			source.dead = true
			switch step {
			case "transfer":
				p.store = &recoveryErrorStore{Store: p.store, step: "transfer", fail: true}
			case "route":
				r.fail = true
			}
			require.False(t, p.recoverWorker(ctx, source, time.Now()))
			require.Len(t, source.pending, 1)
			// Waiting for coordination must not publish a completed hang-up.
			c := source.pending[id].call
			c.mu.Lock()
			done := make(chan error, 1)
			go func() { done <- p.End(ctx, id) }()
			marked := false
			for range 30 {
				marked = marked || c.hungUp.Load()
				time.Sleep(time.Millisecond)
			}
			c.mu.Unlock()
			err = <-done
			require.False(t, marked)
			require.True(t, c.hungUp.Load())
			if step == "transfer" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, mediaworker.ErrUnknownSession)
			}
			// Status/End can forget live metadata; the pending record retains the hang-up.
			require.NotContains(t, p.calls, id)
			require.True(t, p.recoverWorker(ctx, source, time.Now()))
			status, err := p.Status(ctx)
			require.NoError(t, err)
			require.Empty(t, source.pending)
			require.Empty(t, status.Calls)
			require.Zero(t, status.LostCount)
			require.Len(t, status.Takeovers, 1)
			require.Equal(t, "ended", status.Takeovers[0].Kind)
			require.False(t, status.Takeovers[0].Lost)
			require.Contains(t, status.Takeovers[0].Error, "pending takeover lease vanished")
			require.Equal(t, []string{id}, r.forgotten)
		})
	}
}

func TestHangupDuringRetainedPlannedMove(t *testing.T) {
	p, _, _, r := setup(t)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	hold := &trackedHoldRelay{fakeRelay: r}
	p.relay = hold
	p.store = &retainedMoveStore{Store: p.store}
	_, err = p.Move(ctx, id, "b")
	require.Error(t, err)
	source := p.workers["a"]
	require.True(t, source.pending[id].planned)
	require.NotNil(t, source.pending[id].call)
	require.Empty(t, hold.releases)
	require.ErrorIs(t, p.End(ctx, id), mediaworker.ErrUnknownSession)
	require.NotContains(t, p.calls, id)
	// The store confirms the candidate cannot commit after hangup.
	p.store = &endedCandidateStore{Store: p.store}
	p.retryPlannedMoves(ctx, source, time.Now())
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.Empty(t, source.pending)
	require.Empty(t, status.Calls)
	require.Empty(t, status.Takeovers)
	require.Zero(t, status.LostCount)
	require.Equal(t, []string{id}, r.forgotten)
	require.Equal(t, []netip.AddrPort{{}}, hold.releases, "hangup frees retained relay hold")
}
