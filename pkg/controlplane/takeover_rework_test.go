package controlplane

import (
	"context"
	"encoding/json"
	"errors"
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
	state.Version, state.State.Version = 5, 5
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
				// Exhaust all safe cancelled attempts. It must wait without silently
				// dropping ownership; expiry below must publish the terminal outcome.
				for range 3 {
					require.False(t, p.recoverWorker(ctx, source, time.Now()))
				}
				require.Equal(t, 2, target.calls, "default remaining budget permits two attempts")
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
