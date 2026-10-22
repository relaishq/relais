package mediaworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/agent"
	agentv2 "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type componentReplyStore struct {
	*sessionstore.Memory
	unknown        atomic.Bool
	reads          atomic.Int64
	puts           atomic.Int64
	committedBytes atomic.Uint64
	fixedNow       time.Time
	clockFailure   bool
}

func (s *componentReplyStore) Clock(ctx context.Context, id string) (time.Time, error) {
	if s.clockFailure {
		return time.Time{}, errCheckpointClockProbe
	}
	if !s.fixedNow.IsZero() {
		return s.fixedNow, nil
	}
	return s.Memory.Clock(ctx, id)
}

func (s *componentReplyStore) CommitComponents(ctx context.Context, l sessionstore.Lease, c sessionstore.ComponentCommit) (sessionstore.ComponentTuple, error) {
	result, err := s.Memory.CommitComponents(ctx, l, c)
	if err == nil {
		s.committedBytes.Add(uint64(result.WrittenBytes + componentProtocolReserve))
	}
	if err == nil && s.unknown.CompareAndSwap(true, false) {
		return sessionstore.ComponentTuple{}, &sessionstore.TransientError{Op: "test lost reply", Err: context.DeadlineExceeded}
	}
	return result, err
}
func (s *componentReplyStore) ReadComponents(ctx context.Context, id string) (sessionstore.ComponentTuple, error) {
	s.reads.Add(1)
	return s.Memory.ReadComponents(ctx, id)
}
func (s *componentReplyStore) PutState(ctx context.Context, l sessionstore.Lease, b []byte) error {
	s.puts.Add(1)
	return s.Memory.PutState(ctx, l, b)
}

func componentWriterSession(t *testing.T, store sessionstore.Store, v2 bool) *session {
	t.Helper()
	snap := dtlsFixture(t)
	if v2 {
		snap = componentFixture(t)
	}
	lease, err := store.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(t, err)
	s, _ := dtlsCheckpointSession(t, store, snap, lease, ResumeOptions{})
	s.componentMode = true
	setComponentTestHistory(s, []byte("bounded history fixture"))
	return s
}
func TestSplitWriterCaptureAndUnknownReplySettlement(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-agent", true: "v2-agent"}[v2], func(t *testing.T) {
			store := &componentReplyStore{Memory: sessionstore.NewMemory(), fixedNow: time.Now()}
			s := componentWriterSession(t, store, v2)
			require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
			first, err := store.ReadComponents(context.Background(), s.id)
			require.NoError(t, err)
			require.Zero(t, store.puts.Load())
			store.unknown.Store(true)
			s.mu.Lock()
			setComponentTestHistory(s, []byte("new exact history"))
			s.mu.Unlock()
			before := s.worker.metrics.snapshotDebitBytes.Load()
			require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointRequired), sessionstore.ErrTransient)
			require.Equal(t, before, s.worker.metrics.snapshotDebitBytes.Load(), "an unknown reply cannot debit")
			published, err := store.Memory.ReadComponents(context.Background(), s.id)
			require.NoError(t, err)
			require.NotEqual(t, first.Metadata.References[2], published.Metadata.References[2])
			// Settle the pending write, then stop recapture at the store clock.
			// Repeating this must leave both the real bucket and acknowledgement alone.
			store.clockFailure = true
			for range 2 {
				require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointRequired), errCheckpointClockProbe)
				require.EqualValues(t, 2, checkpointObservation(s).observations.Successes)
			}
			var settledBalance uint64
			if v2 {
				settledBalance = storedCheckpointQuota(t, store, s.id)
			}
			debit := store.committedBytes.Load()
			store.clockFailure = false
			require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
			if v2 {
				charge := store.committedBytes.Load() - debit
				require.Equal(t, settledBalance-charge, storedCheckpointQuota(t, store, s.id), "next fixed-clock write must spend exactly one charge from the settled bucket")
			}
			require.EqualValues(t, 3, checkpointObservation(s).observations.Successes)
			if v2 {
				require.Equal(t, store.committedBytes.Load(), s.worker.metrics.snapshotDebitBytes.Load(), "each actual commit is acknowledged and charged once")
			}
			progress, err := store.Memory.ReadComponents(context.Background(), s.id)
			require.NoError(t, err)
			require.Equal(t, published.Metadata.References[2], progress.Metadata.References[2])
			require.Equal(t, published.Components.History, progress.Components.History)
		})
	}
}

func TestSplitCaptureAdmissionRefusesEveryChangingEncoding(t *testing.T) {
	store := sessionstore.NewMemory()
	s := componentWriterSession(t, store, false)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	first, err := store.ReadComponents(context.Background(), s.id)
	require.NoError(t, err)
	s.mu.Lock()
	setComponentTestHistory(s, bytes.Repeat([]byte{1}, 8448))
	s.mu.Unlock()
	require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointRequired), agentv2.ErrCapacity)
	current, err := store.ReadComponents(context.Background(), s.id)
	require.NoError(t, err)
	require.Equal(t, first.Metadata, current.Metadata)
	s.mu.Lock()
	setComponentTestHistory(s, []byte("small"))
	s.state.Audio.ID = string(bytes.Repeat([]byte{'x'}, 8192))
	s.mu.Unlock()
	require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointRequired), agentv2.ErrCapacity)
	current, err = store.ReadComponents(context.Background(), s.id)
	require.NoError(t, err)
	require.Equal(t, first.Metadata, current.Metadata)
}
func TestSplitRecoveryEnvelopePreservesHistoryAndFormatWithAdmissionOff(t *testing.T) {
	store := sessionstore.NewMemory()
	s := componentWriterSession(t, store, false)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	tuple, err := store.ReadComponents(context.Background(), s.id)
	require.NoError(t, err)
	raw, metadata, err := ReadRecoverySnapshot(context.Background(), store, s.id)
	require.NoError(t, err)
	require.NotNil(t, metadata)
	require.True(t, IsComponentSnapshot(raw))
	snap, err := decodeSnapshot(raw)
	require.NoError(t, err)
	require.Equal(t, s.componentHistory, snap.split.history)
	target, _ := dtlsCheckpointSession(t, store, *snap, s.lease, ResumeOptions{Kind: agent.Takeover})
	require.False(t, target.worker.cfg.SplitCheckpoints)
	require.NoError(t, target.checkpoint.restore(checkpointRestore{snapshot: snap}))
	require.NoError(t, target.checkpoint.persist(context.Background(), checkpointRequired))
	restored, err := store.ReadComponents(context.Background(), s.id)
	require.NoError(t, err)
	require.Equal(t, tuple.Components.Work, restored.Components.Work)
	require.Equal(t, tuple.Components.History, restored.Components.History)
	require.Zero(t, target.worker.cfg.Agent.MaxStateBytes-agent.DefaultMaxStateBytes)
	wire, err := target.checkpoint.capture(checkpointExact, nil)
	require.NoError(t, err)
	require.True(t, IsComponentSnapshot(wire))
	decoded, err := decodeSnapshot(wire)
	require.NoError(t, err)
	require.Equal(t, s.componentHistory, decoded.split.history)
	require.Nil(t, decoded.split.metadata)
}
func TestSplitAssemblyRejectsVersionSix(t *testing.T) {
	c := mutatedComponents(t, func(_ *connectionSetupComponent, tr *transportComponent) { tr.LegacyVersion = 6; tr.Host = nil })
	_, _, err := assembleSessionComponents(c, 1)
	require.ErrorIs(t, err, errStateVersion)
}
func TestAdoptionEpochZeroUsesNotEstablishedWireCode(t *testing.T) {
	rw := httptest.NewRecorder()
	workerAPIError(rw, errors.Join(errDTLSCheckpointEpoch, errBadState))
	var reply struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rw.Body.Bytes(), &reply))
	require.Contains(t, rw.Body.String(), "not_established")
}

func TestSplitPrivateEnvelopeGoldens(t *testing.T) {
	for _, test := range []struct {
		name    string
		history []byte
		control bool
	}{{"empty", nil, false}, {"history", []byte("bounded fixture conversation"), false}, {"control", []byte("bounded fixture conversation"), true}} {
		t.Run(test.name, func(t *testing.T) {
			snap := componentFixture(t)
			if !test.control {
				snap.State.Agent.V2.ControlHigh = 0
				snap.State.Agent.V2.ControlFloor = 0
				snap.State.Agent.V2.Controls = nil
				snap.State.Version = 8
				snap.Version = 8
			}
			snap.split = &componentSnapshotState{history: test.history}
			data, err := json.Marshal(snap)
			require.NoError(t, err)
			path := "testdata/v10-" + test.name + ".json"
			if os.Getenv("RELAIS_UPDATE_SPLIT_GOLDEN") == "1" {
				require.NoError(t, os.WriteFile(path, append(data, '\n'), 0600))
			}
			golden, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, bytes.TrimSpace(golden), data)
			restored, err := decodeSnapshot(data)
			require.NoError(t, err)
			require.True(t, bytes.Equal(test.history, restored.split.history))
			again, err := json.Marshal(restored)
			require.NoError(t, err)
			require.Equal(t, data, again)
		})
	}
}
func TestSplitDTLSGuardUsesCommittedTuple(t *testing.T) {
	store := sessionstore.NewMemory()
	s, wire := func() (*session, *dtlsWireConn) {
		snap := dtlsFixture(t)
		lease, err := store.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
		require.NoError(t, err)
		return dtlsCheckpointSession(t, store, snap, lease, ResumeOptions{})
	}()
	s.componentMode = true
	setComponentTestHistory(s, []byte("history survives the guarded cut"))
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	seen := map[[8]byte]bool{}
	for range 64 {
		_, err := s.dtlsConn.Write([]byte("tail"))
		require.NoError(t, err)
		seen[dtlsNonceInput(t, dtlsPacket(t, wire))] = true
	}
	_, err := s.dtlsConn.Write([]byte("blocked tail"))
	require.NoError(t, err)
	require.Empty(t, wire.packets)
	require.Positive(t, s.worker.metrics.dtlsSequenceDrops.Load())
	s.fenced.Store(true)
	s.close()
	lease, err := store.Transfer(context.Background(), s.lease, netip.MustParseAddrPort("127.0.0.1:10001"), time.Hour)
	require.NoError(t, err)
	raw, _, err := ReadRecoverySnapshot(context.Background(), store, s.id)
	require.NoError(t, err)
	snap, err := decodeSnapshot(raw)
	require.NoError(t, err)
	target, output := dtlsCheckpointSession(t, store, *snap, lease, ResumeOptions{Kind: agent.Takeover})
	require.NoError(t, target.checkpoint.restore(checkpointRestore{snapshot: snap}))
	require.NoError(t, target.checkpoint.persist(context.Background(), checkpointRequired))
	for range 64 {
		_, err := target.dtlsConn.Write([]byte("after takeover"))
		require.NoError(t, err)
		pair := dtlsNonceInput(t, dtlsPacket(t, output))
		require.False(t, seen[pair])
		seen[pair] = true
	}
	_, err = target.dtlsConn.Write([]byte("blocked after takeover"))
	require.NoError(t, err)
	require.Empty(t, output.packets)
}

func TestSplitExactAdoptionBeforeFirstPublication(t *testing.T) {
	store := sessionstore.NewMemory()
	s := componentWriterSession(t, store, false)
	require.NoError(t, s.checkpoint.prepareAdoption(context.Background()))
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	tuple, err := store.ReadComponents(context.Background(), s.id)
	require.NoError(t, err)
	_, history, err := assembleSessionComponents(sessionComponents(tuple.Components), tuple.Metadata.StateVersion)
	require.NoError(t, err)
	require.Equal(t, s.componentHistory, history)
}

func TestSplitUnknownWriteSettlementFencesOldOwner(t *testing.T) {
	store := &componentReplyStore{Memory: sessionstore.NewMemory()}
	s := componentWriterSession(t, store, false)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	store.unknown.Store(true)
	require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointRequired), sessionstore.ErrTransient)
	_, err := store.Transfer(context.Background(), s.lease, netip.MustParseAddrPort("127.0.0.1:10001"), time.Hour)
	require.NoError(t, err)
	require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointRequired), sessionstore.ErrLeaseLost)
	require.True(t, s.fenced.Load())
	require.ErrorIs(t, s.ctx.Err(), context.Canceled)
}

type adoptionCheckpointErrorStore struct{ sessionstore.Store }

func (s *adoptionCheckpointErrorStore) Clock(context.Context, string) (time.Time, error) {
	return time.Time{}, errDTLSCheckpointEpoch
}

func TestAdoptionCheckpointEpochRefusalReturnsNotEstablished(t *testing.T) {
	// Inject the distinct capture error at the checkpoint boundary. Resume must
	// preserve its typed adoption outcome instead of returning generic bad_state.
	store := &adoptionCheckpointErrorStore{Store: sessionstore.NewMemory()}
	router, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, router.Close()) })
	w, err := New(Config{Relay: &RelayConfig{Owners: store, Addr: router.WorkerAddr(), PublicAddr: router.PublicAddr()}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	snap := dtlsFixture(t)
	lease, err := store.Claim(context.Background(), snap.State.ID, w.LocalAddr(), time.Minute)
	require.NoError(t, err)
	state, err := json.Marshal(snap)
	require.NoError(t, err)
	_, err = w.ResumeSession(state, ResumeOptions{Lease: lease})
	require.ErrorIs(t, err, ErrNotEstablished)
	require.Zero(t, w.SessionCount())
}

func setComponentTestHistory(s *session, history []byte) {
	s.componentHistory = history
	if s.state.Agent.V2 != nil {
		s.state.Agent.History = history
	}
}
