package mediaworker

import (
	"bytes"
	"context"
	"encoding/gob"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func plaintextCheckpointSession(t *testing.T) *session {
	t.Helper()
	store := sessionstore.NewMemory()
	snap := dtlsFixture(t)
	serialized, err := decodeDTLSSerializedState(snap.DTLSConnection)
	require.NoError(t, err)
	serialized.LocalEpoch = 0
	var wire bytes.Buffer
	require.NoError(t, gob.NewEncoder(&wire).Encode(serialized))
	plaintext := wire.Bytes()
	lease, err := store.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(t, err)
	s, _ := dtlsCheckpointSession(t, store, snap, lease, ResumeOptions{})
	// Preserve an established transport's SRTP contexts while injecting the
	// invalid DTLS capture. Plaintext epochs cannot themselves derive SRTP keys.
	var state dtls.State
	require.NoError(t, state.UnmarshalBinary(plaintext))
	invalid, err := dtls.ResumeWithOptions(&state, s.dtlsEndpoint, s.dtlsEndpoint.RemoteAddr(), dtls.WithLoggerFactory(s.worker.cfg.LoggerFactory))
	require.NoError(t, err)
	require.NoError(t, invalid.HandshakeContext(s.ctx))
	original := s.dtlsConn
	s.dtlsConn = invalid
	t.Cleanup(func() { s.fenced.Store(true); _ = original.Close() })
	return s
}
func TestCheckpointRefusesPlaintextDTLSEpoch(t *testing.T) {
	s := plaintextCheckpointSession(t)
	err := s.checkpoint.persist(context.Background(), checkpointRequired)
	require.ErrorIs(t, err, errDTLSCheckpointEpoch)
	require.False(t, checkpointKnown(s))
	s.mu.Lock()
	require.True(t, s.checkpoint.allowDTLSLocked(dtlsHeaderRecord(22, 1, 1000)), "invalid plaintext capture cannot arm a bound")
	s.mu.Unlock()
}
func TestCheckpointEpochRefusalHasDistinctError(t *testing.T) {
	s := plaintextCheckpointSession(t)
	err := s.checkpoint.persist(context.Background(), checkpointRequired)
	require.ErrorIs(t, err, errBadState)
	require.NotErrorIs(t, err, ErrNotEstablished)
}
func TestCheckpointEpochWarningIsLoggedOnce(t *testing.T) {
	s := plaintextCheckpointSession(t)
	logger := &cacheWarningLogger{LeveledLogger: s.log}
	s.log = logger
	// Requests run the actual capture and warning path. A completed attempt is
	// visible through checkpoint observations, without exposing the writer latch.
	s.worker.cfg.SnapshotInterval = time.Hour
	done := make(chan struct{})
	go func() { defer close(done); s.checkpoint.run(s.ctx) }()
	defer func() { s.cancel(); <-done }()
	for attempt := uint64(1); attempt <= 3; attempt++ {
		s.checkpoint.request()
		require.Eventually(t, func() bool { return checkpointObservation(s).observations.Failures >= attempt }, 2*time.Second, time.Millisecond)
	}
	require.Equal(t, 1, logger.count())
	require.False(t, checkpointKnown(s))
}
