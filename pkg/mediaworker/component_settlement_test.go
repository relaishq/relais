package mediaworker

import (
	"bytes"
	"context"
	"encoding/json"
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
	require.NoError(t, s.persistSnapshotMode(ctx, true))
	bound := *s.dtlsAcknowledged
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
	s.componentHistory = []byte("history P2")
	s.mu.Unlock()
	require.ErrorIs(t, s.persistSnapshotMode(ctx, true), sessionstore.ErrTransient)
	require.Equal(t, bound, *s.dtlsAcknowledged, "unknown reply cannot advance the guard")
	require.EqualValues(t, 1, s.state.Checkpoint.Successes)
	require.Zero(t, s.state.Checkpoint.Failures, "unknown outcome is not yet a failure")
	store.landOnNext = true
	s.mu.Lock()
	s.componentHistory = []byte("history P3")
	s.mu.Unlock()
	require.ErrorIs(t, s.persistSnapshotMode(ctx, true), sessionstore.ErrStateSuperseded)
	published, err := store.ReadComponents(ctx, s.id)
	require.NoError(t, err)
	require.NoError(t, s.settleComponentWrite(ctx))
	require.Nil(t, s.componentPending)
	require.Equal(t, published.Metadata, *s.componentMetadata)
	require.Equal(t, bound, *s.dtlsAcknowledged, "rebasing is not an acknowledgment")
	require.EqualValues(t, 1, s.state.Checkpoint.Successes)
	require.EqualValues(t, 2, s.state.Checkpoint.Failures)
	send(54)
	_, err = s.dtlsConn.Write([]byte("guarded"))
	require.NoError(t, err)
	require.Empty(t, wire.packets)
	require.Positive(t, s.worker.metrics.dtlsSequenceDrops.Load())
	for range 3 {
		require.NoError(t, s.persistSnapshotMode(ctx, true))
	}
	latest, err := store.ReadComponents(ctx, s.id)
	require.NoError(t, err)
	var transport transportComponent
	require.NoError(t, unmarshalComponent(latest.Components.Transport, componentTransport, &transport))
	require.Equal(t, transport.DTLS.SequenceNumber, s.dtlsAcknowledged.next)
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
	target.restoreComponentMode(restored)
	require.NoError(t, target.persistSnapshotMode(ctx, true))
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
	store := &delayedComponentStore{Memory: sessionstore.NewMemory()}
	s := componentWriterSession(t, store, true)
	require.NoError(t, s.persistSnapshotMode(ctx, true))
	store.lostAfterCommit = true
	s.mu.Lock()
	s.componentHistory = []byte("history requiring a bank write")
	s.mu.Unlock()
	debitBefore := s.worker.metrics.snapshotDebitBytes.Load()
	require.ErrorIs(t, s.persistSnapshotMode(ctx, true), sessionstore.ErrTransient)
	pending := s.componentPending
	require.NotNil(t, pending)
	require.EqualValues(t, 1, s.state.Checkpoint.Successes)
	require.Zero(t, s.state.Checkpoint.Failures)
	require.Equal(t, debitBefore, s.worker.metrics.snapshotDebitBytes.Load())
	store.readFailures = 3
	for range 3 {
		require.ErrorIs(t, s.persistSnapshotMode(ctx, true), sessionstore.ErrTransient)
	}
	published, err := store.Memory.ReadComponents(ctx, s.id)
	require.NoError(t, err)
	require.NoError(t, s.settleComponentWrite(ctx))
	require.EqualValues(t, 2, s.state.Checkpoint.Attempts)
	require.EqualValues(t, 2, s.state.Checkpoint.Successes)
	require.Zero(t, s.state.Checkpoint.Failures)
	var transport transportComponent
	require.NoError(t, unmarshalComponent(published.Components.Transport, componentTransport, &transport))
	var host v2Envelope
	require.NoError(t, json.Unmarshal(transport.Host, &host))
	saved, err := host.Quota.balance()
	require.NoError(t, err)
	before, err := pending.pair.V2.Quota.balance()
	require.NoError(t, err)
	// Charge is determined by the store's selected banks, then embedded by
	// FinalizeTransport. Comparing only local and saved balances misses its removal.
	require.Equal(t, store.lastWritten+componentProtocolReserve, pending.bytes)
	require.Equal(t, uint64(pending.bytes), before-saved)
	require.Equal(t, uint64(pending.bytes), s.worker.metrics.snapshotDebitBytes.Load()-debitBefore)
	require.NoError(t, s.settleComponentWrite(ctx))
	require.EqualValues(t, 2, s.state.Checkpoint.Successes)
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
				require.NoError(t, s.persistSnapshotMode(context.Background(), true))
			} else {
				require.True(t, s.state.Checkpoint.CapturedAt.IsZero())
			}
			// Find the real non-zero-time capture boundary without publishing or
			// arming a DTLS bound. Binary search keeps this a short-mode test.
			capture := func(size int) error {
				candidate := cloneAgentState(s.state.Agent)
				candidate.State = agent.State{Version: 1, Bytes: bytes.Repeat([]byte{'a'}, size)}
				candidate.Revision++
				_, _, err := s.captureSnapshotCandidate(time.Now(), &candidate)
				return err
			}
			low, high := 0, s.worker.cfg.Agent.MaxStateBytes+1
			for low+1 < high {
				mid := (low + high) / 2
				err := capture(mid)
				if errors.Is(err, agentv2.ErrCapacity) {
					high = mid
				} else {
					require.NoError(t, err)
					low = mid
				}
			}
			require.Positive(t, low)
			require.NoError(t, capture(low))
			require.ErrorIs(t, capture(low+1), agentv2.ErrCapacity)
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
			require.NoError(t, s.persistSnapshotMode(context.Background(), true), "the largest admitted Save must checkpoint")
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
			require.NoError(t, s.persistSnapshotMode(ctx, true))
			first, err := store.ReadComponents(ctx, s.id)
			require.NoError(t, err)
			bound := *s.dtlsAcknowledged
			store.unknown.Store(true)
			s.componentHistory = []byte("pending newer history")
			require.ErrorIs(t, s.persistSnapshotMode(ctx, true), sessionstore.ErrTransient)
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
			require.ErrorIs(t, s.settleComponentWrite(ctx), want)
			require.Equal(t, first.Metadata, *s.componentMetadata)
			require.Equal(t, bound, *s.dtlsAcknowledged)
			require.EqualValues(t, 1, s.state.Checkpoint.Successes, "foreign evidence cannot acknowledge a pending write")
			if want == sessionstore.ErrLeaseLost {
				require.Nil(t, s.componentPending)
			} else {
				require.NotNil(t, s.componentPending, "another tenure cannot clear or rebase the pending write")
			}
		})
	}
}

func TestSplitAdoptionReadsTransferredPublication(t *testing.T) {
	ctx := context.Background()
	store := &componentReplyStore{Memory: sessionstore.NewMemory()}
	source := componentWriterSession(t, store, false)
	require.NoError(t, source.persistSnapshotMode(ctx, true))
	data, err := source.snapshotBytes()
	require.NoError(t, err)
	snap, err := decodeSnapshot(data)
	require.NoError(t, err)
	lease, err := store.Transfer(ctx, source.lease, netip.MustParseAddrPort("127.0.0.1:10001"), time.Hour)
	require.NoError(t, err)
	target, _ := dtlsCheckpointSession(t, store, *snap, lease, ResumeOptions{Kind: agent.PlannedMove})
	target.restoreComponentMode(snap)
	require.Nil(t, target.componentMetadata)
	reads := store.reads.Load()
	require.NoError(t, target.prepareComponentAdoption(ctx))
	require.Equal(t, reads+1, store.reads.Load())
	require.Equal(t, *source.componentMetadata, *target.componentMetadata)
	require.NoError(t, target.persistSnapshotMode(ctx, true))
}

func TestSplitRequiredCheckpointBudgetRefusalMatchesLegacy(t *testing.T) {
	for _, available := range []float64{0, 100} {
		t.Run(map[bool]string{true: "no-budget", false: "store-refusal"}[available == 0], func(t *testing.T) {
			s := componentWriterSession(t, sessionstore.NewMemory(), true)
			s.snapshotBudgetMu.Lock()
			s.snapshotBytesBudget = snapshotBucket{tokens: available, at: time.Now().Add(time.Hour)}
			s.snapshotBudgetMu.Unlock()
			require.ErrorIs(t, s.persistSnapshotBudget(context.Background(), true, false), agentv2.ErrCapacity)
			require.Nil(t, s.componentPending)
			require.ErrorIs(t, s.persistSnapshotBudget(context.Background(), true, true), errCheckpointSkipped)
			require.False(t, s.fenced.Load())
		})
	}
}
