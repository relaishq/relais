package mediaworker

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/agent"
	agentv2 "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type delayedComponentStore struct {
	*sessionstore.Memory
	mu sync.Mutex
	// lostAfterCommit: commit, then report a transient error.
	lostAfterCommit bool
	// delay: stash the next commit (not applied) and report transient.
	delay        bool
	stashed      *sessionstore.ComponentCommit
	stashLease   sessionstore.Lease
	landOnNext   bool
	readFailures int
	lastWritten  int
	clockFailure bool
	fixedNow     time.Time
}

func (s *delayedComponentStore) CommitComponents(ctx context.Context, l sessionstore.Lease, c sessionstore.ComponentCommit) (sessionstore.ComponentTuple, error) {
	s.mu.Lock()
	lost, delay, land := s.lostAfterCommit, s.delay, s.landOnNext
	s.lostAfterCommit, s.delay = false, false
	if land {
		s.landOnNext = false
	}
	stashed, stashLease := s.stashed, s.stashLease
	if delay {
		cc := c
		s.stashed, s.stashLease = &cc, l
	}
	s.mu.Unlock()
	if delay {
		return sessionstore.ComponentTuple{}, &sessionstore.TransientError{Op: "delayed component", Err: context.DeadlineExceeded}
	}
	if land && stashed != nil {
		// The earlier (already settled as "not committed") command lands late.
		_, err := s.Memory.CommitComponents(ctx, stashLease, *stashed)
		if err != nil {
			panic(err)
		}
	}
	result, err := s.Memory.CommitComponents(ctx, l, c)
	s.mu.Lock()
	s.lastWritten = result.WrittenBytes
	s.mu.Unlock()
	if err == nil && lost {
		return sessionstore.ComponentTuple{}, &sessionstore.TransientError{Op: "delayed component lost reply", Err: context.DeadlineExceeded}
	}
	return result, err
}

func (s *delayedComponentStore) Clock(ctx context.Context, id string) (time.Time, error) {
	if s.clockFailure {
		return time.Time{}, errCheckpointClockProbe
	}
	if !s.fixedNow.IsZero() {
		return s.fixedNow, nil
	}
	return s.Memory.Clock(ctx, id)
}

func (s *delayedComponentStore) ReadComponents(ctx context.Context, id string) (sessionstore.ComponentTuple, error) {
	s.mu.Lock()
	fail := s.readFailures > 0
	if fail {
		s.readFailures--
	}
	s.mu.Unlock()
	if fail {
		return sessionstore.ComponentTuple{}, &sessionstore.TransientError{Op: "delayed component read", Err: context.DeadlineExceeded}
	}
	return s.Memory.ReadComponents(ctx, id)
}

func TestSplitLateCommitRebasesWithoutAcknowledgingDTLS(t *testing.T) {
	ctx := context.Background()
	store := &delayedComponentStore{Memory: sessionstore.NewMemory()}
	snap := dtlsFixture(t)
	lease, err := store.Claim(ctx, snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(t, err)
	s, wire := dtlsCheckpointSession(t, store, snap, lease, ResumeOptions{})
	s.componentMode = true
	require.NoError(t, s.checkpoint.persist(ctx, checkpointRequired))
	seen := map[[8]byte]bool{}
	send := func(n int) {
		for range n {
			_, err := s.dtlsConn.Write([]byte("advance"))
			require.NoError(t, err)
			pair := dtlsNonceInput(t, dtlsPacket(t, wire))
			require.False(t, seen[pair])
			seen[pair] = true
		}
	}
	send(10)
	store.delay = true
	s.mu.Lock()
	setComponentTestHistory(s, []byte("history P2"))
	s.mu.Unlock()
	require.ErrorIs(t, s.checkpoint.persist(ctx, checkpointRequired), sessionstore.ErrTransient)
	require.EqualValues(t, 1, s.state.Checkpoint.Successes)
	require.Zero(t, s.state.Checkpoint.Failures, "unknown outcome is not yet a failure")
	store.landOnNext = true
	s.mu.Lock()
	setComponentTestHistory(s, []byte("history P3"))
	s.mu.Unlock()
	require.ErrorIs(t, s.checkpoint.persist(ctx, checkpointRequired), sessionstore.ErrStateSuperseded)
	published, err := store.ReadComponents(ctx, s.id)
	require.NoError(t, err)
	store.clockFailure = true
	require.ErrorIs(t, s.checkpoint.persist(ctx, checkpointRequired), errCheckpointClockProbe)
	store.clockFailure = false
	require.EqualValues(t, 1, s.state.Checkpoint.Successes)
	require.EqualValues(t, 3, s.state.Checkpoint.Failures, "two rejected pending captures and the controlled Clock failure")
	send(54)
	_, err = s.dtlsConn.Write([]byte("guarded"))
	require.NoError(t, err)
	require.Empty(t, wire.packets)
	require.Positive(t, s.worker.metrics.dtlsSequenceDrops.Load())
	for range 3 {
		require.NoError(t, s.checkpoint.persist(ctx, checkpointRequired))
	}
	latest, err := store.ReadComponents(ctx, s.id)
	require.NoError(t, err)
	require.NotEqual(t, published.Metadata.References[2], latest.Metadata.References[2])
	var transport transportComponent
	require.NoError(t, unmarshalComponent(latest.Components.Transport, componentTransport, &transport))
	// Crash after the new acknowledgment: its successor starts beyond every
	// packet this process was permitted to send, including the late landing.
	s.fenced.Store(true)
	s.close()
	lease, err = store.Transfer(ctx, lease, netip.MustParseAddrPort("127.0.0.1:10009"), time.Hour)
	require.NoError(t, err)
	raw, _, err := ReadRecoverySnapshot(ctx, store, s.id)
	require.NoError(t, err)
	restored, err := decodeSnapshot(raw)
	require.NoError(t, err)
	target, output := dtlsCheckpointSession(t, store, *restored, lease, ResumeOptions{Kind: agent.Takeover})
	require.NoError(t, target.checkpoint.restore(checkpointRestore{snapshot: restored}))
	require.NoError(t, target.checkpoint.persist(ctx, checkpointRequired))
	for range 64 {
		_, err := target.dtlsConn.Write([]byte("restored"))
		require.NoError(t, err)
		pair := dtlsNonceInput(t, dtlsPacket(t, output))
		require.False(t, seen[pair])
		seen[pair] = true
	}
	_, err = target.dtlsConn.Write([]byte("blocked"))
	require.NoError(t, err)
	require.Empty(t, output.packets)
}

func TestSplitUnknownWriteCountsOnceAndDebitsExactly(t *testing.T) {
	ctx := context.Background()
	store := &delayedComponentStore{Memory: sessionstore.NewMemory(), fixedNow: time.Now()}
	s := componentWriterSession(t, store, true)
	require.NoError(t, s.checkpoint.persist(ctx, checkpointRequired))
	first, err := store.ReadComponents(ctx, s.id)
	require.NoError(t, err)
	firstSnapshot, _, err := assembleSessionComponents(sessionComponents(first.Components), first.Metadata.StateVersion)
	require.NoError(t, err)
	before, err := firstSnapshot.State.Agent.V2.Quota.balance()
	require.NoError(t, err)
	store.lostAfterCommit = true
	s.mu.Lock()
	setComponentTestHistory(s, []byte("history requiring a bank write"))
	s.mu.Unlock()
	debitBefore := s.worker.metrics.snapshotDebitBytes.Load()
	require.ErrorIs(t, s.checkpoint.persist(ctx, checkpointRequired), sessionstore.ErrTransient)
	require.EqualValues(t, 1, checkpointObservation(s).observations.Successes)
	require.Zero(t, checkpointObservation(s).observations.Failures)
	require.Equal(t, debitBefore, s.worker.metrics.snapshotDebitBytes.Load())
	store.readFailures = 3
	for range 3 {
		require.ErrorIs(t, s.checkpoint.persist(ctx, checkpointRequired), sessionstore.ErrTransient)
	}
	published, err := store.Memory.ReadComponents(ctx, s.id)
	require.NoError(t, err)
	store.clockFailure = true
	require.ErrorIs(t, s.checkpoint.persist(ctx, checkpointRequired), errCheckpointClockProbe)
	require.EqualValues(t, 3, checkpointObservation(s).observations.Attempts)
	require.EqualValues(t, 2, checkpointObservation(s).observations.Successes)
	savedSnapshot, _, err := assembleSessionComponents(sessionComponents(published.Components), published.Metadata.StateVersion)
	require.NoError(t, err)
	saved, err := savedSnapshot.State.Agent.V2.Quota.balance()
	require.NoError(t, err)
	charge := uint64(store.lastWritten + componentProtocolReserve)
	require.Equal(t, charge, before-saved)
	require.Equal(t, charge, s.worker.metrics.snapshotDebitBytes.Load()-debitBefore)
	require.ErrorIs(t, s.checkpoint.persist(ctx, checkpointRequired), errCheckpointClockProbe)
	require.EqualValues(t, 2, checkpointObservation(s).observations.Successes)
	require.Equal(t, charge, s.worker.metrics.snapshotDebitBytes.Load()-debitBefore, "repeated settlement cannot debit twice")
}

func TestSplitSaveAdmissionRejectsOversizeBeforePublication(t *testing.T) {
	for _, established := range []bool{false, true} {
		t.Run(map[bool]string{true: "established", false: "before-handshake"}[established], func(t *testing.T) {
			s := componentWriterSession(t, sessionstore.NewMemory(), false)
			if !established {
				connection := s.dtlsConn
				s.dtlsConn = nil
				defer func() { s.dtlsConn = connection }()
			}
			state := agent.State{Version: 1, Bytes: bytes.Repeat([]byte{'a'}, 7000)}
			prior := cloneAgentState(s.state.Agent)
			require.ErrorIs(t, s.validateAgentState(state), agent.ErrStateTooLarge)
			require.Equal(t, prior, s.state.Agent)
			state.Bytes = state.Bytes[:4096]
			require.NoError(t, s.validateAgentState(state))
			s.componentMode = false
			state.Bytes = bytes.Repeat([]byte{'a'}, 8192)
			require.NoError(t, s.validateAgentState(state), "flag off retains the legacy limit")
		})
	}
}

func TestSplitSaveAdmissionMatchesFirstCaptureBoundary(t *testing.T) {
	for _, phase := range []string{"before-first-ack", "after-first-ack", "before-handshake"} {
		t.Run(phase, func(t *testing.T) {
			s := componentWriterSession(t, sessionstore.NewMemory(), false)
			if phase == "after-first-ack" {
				require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
			} else {
				require.True(t, s.state.Checkpoint.CapturedAt.IsZero())
			}
			// Find the real non-zero-time capture boundary without publishing or
			// arming a DTLS bound. Binary search keeps this a short-mode test.
			capture := func(size int) error {
				candidate := cloneAgentState(s.state.Agent)
				candidate.State = agent.State{Version: 1, Bytes: bytes.Repeat([]byte{'a'}, size)}
				candidate.Revision++
				state := candidate.State
				_, err := s.checkpoint.admit(checkpointAdmission{legacy: &state})
				return err
			}
			low, high := 0, s.worker.cfg.Agent.MaxStateBytes+1
			for low+1 < high {
				mid := (low + high) / 2
				err := capture(mid)
				if errors.Is(err, agent.ErrStateTooLarge) {
					high = mid
				} else {
					require.NoError(t, err)
					low = mid
				}
			}
			require.Positive(t, low)
			require.NoError(t, capture(low))
			require.ErrorIs(t, capture(low+1), agent.ErrStateTooLarge)
			connection := s.dtlsConn
			if phase == "before-handshake" {
				s.dtlsConn = nil
				t.Cleanup(func() { s.dtlsConn = connection })
			}
			state := agent.State{Version: 1, Bytes: bytes.Repeat([]byte{'a'}, low)}
			require.NoError(t, s.validateAgentState(state))
			state.Bytes = append(state.Bytes, 'a')
			require.ErrorIs(t, s.validateAgentState(state), agent.ErrStateTooLarge, "Save must refuse the first byte the capture cannot encode")
			s.dtlsConn = connection
			state.Bytes = state.Bytes[:low]
			s.mu.Lock()
			s.state.Agent.State = state
			s.state.Agent.Revision++
			s.mu.Unlock()
			require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired), "the largest admitted Save must checkpoint")
			t.Logf("largest admitted/captured v1 state: %d bytes", low)
		})
	}
}

type componentTupleOverrideStore struct {
	*componentReplyStore
	tuple *sessionstore.ComponentTuple
}

func (s *componentTupleOverrideStore) ReadComponents(ctx context.Context, id string) (sessionstore.ComponentTuple, error) {
	if s.tuple != nil {
		return *s.tuple, nil
	}
	return s.componentReplyStore.ReadComponents(ctx, id)
}

func TestSplitSettlementRejectsForeignLeaseAndPublication(t *testing.T) {
	for _, kind := range []string{"lease-owner", "lease-epoch", "publication-owner", "publication-epoch"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store := &componentTupleOverrideStore{componentReplyStore: &componentReplyStore{Memory: sessionstore.NewMemory()}}
			s := componentWriterSession(t, store, false)
			require.NoError(t, s.checkpoint.persist(ctx, checkpointRequired))
			first, err := store.ReadComponents(ctx, s.id)
			require.NoError(t, err)
			store.unknown.Store(true)
			setComponentTestHistory(s, []byte("pending newer history"))
			require.ErrorIs(t, s.checkpoint.persist(ctx, checkpointRequired), sessionstore.ErrTransient)
			published, err := store.ReadComponents(ctx, s.id)
			require.NoError(t, err)
			want := sessionstore.ErrStateSuperseded
			switch kind {
			case "lease-owner":
				published.Lease.Worker = netip.MustParseAddrPort("127.0.0.1:10001")
				want = sessionstore.ErrLeaseLost
			case "lease-epoch":
				published.Lease.Epoch++
				want = sessionstore.ErrLeaseLost
			case "publication-owner":
				published.Components = first.Components
				published.Metadata.Owner = "127.0.0.1:10001"
			case "publication-epoch":
				published.Components = first.Components
				published.Metadata.OwnerEpoch++
			}
			store.tuple = &published // Deliberately inconsistent fake-store tuple.
			require.ErrorIs(t, s.checkpoint.persist(ctx, checkpointRequired), want)
			require.EqualValues(t, 1, s.state.Checkpoint.Successes, "foreign evidence cannot acknowledge a pending write")
			if want == sessionstore.ErrLeaseLost {
				require.True(t, s.fenced.Load())
			}
		})
	}
}

func TestSplitAdoptionReadsTransferredPublication(t *testing.T) {
	ctx := context.Background()
	store := &componentReplyStore{Memory: sessionstore.NewMemory()}
	source := componentWriterSession(t, store, false)
	require.NoError(t, source.checkpoint.persist(ctx, checkpointRequired))
	data, err := source.checkpoint.capture(checkpointLive, nil)
	require.NoError(t, err)
	snap, err := decodeSnapshot(data)
	require.NoError(t, err)
	lease, err := store.Transfer(ctx, source.lease, netip.MustParseAddrPort("127.0.0.1:10001"), time.Hour)
	require.NoError(t, err)
	target, _ := dtlsCheckpointSession(t, store, *snap, lease, ResumeOptions{Kind: agent.PlannedMove})
	require.NoError(t, target.checkpoint.restore(checkpointRestore{snapshot: snap}))
	reads := store.reads.Load()
	require.NoError(t, target.checkpoint.prepareAdoption(ctx))
	require.Equal(t, reads+1, store.reads.Load())
	require.NoError(t, target.checkpoint.persist(ctx, checkpointRequired))
}

func TestSplitRequiredCheckpointBudgetRefusalMatchesLegacy(t *testing.T) {
	for _, available := range []float64{0, 100} {
		t.Run(map[bool]string{true: "no-budget", false: "store-refusal"}[available == 0], func(t *testing.T) {
			s := componentWriterSession(t, sessionstore.NewMemory(), true)
			restoreCheckpointQuota(t, s, available, time.Now().Add(time.Hour))
			require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointRequired), agentv2.ErrCapacity)
			require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointIntent), errCheckpointSkipped)
			require.False(t, s.fenced.Load())
		})
	}
}
