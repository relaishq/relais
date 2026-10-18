package mediaworker

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckpointRefusesPlaintextDTLSEpoch(t *testing.T) {
	s := &session{}
	require.ErrorIs(t, s.armDTLSBound(dtlsSequenceCheckpoint{epoch: 0, next: 7}), errDTLSCheckpointEpoch)
	require.Nil(t, s.dtlsProvisional)
	require.Nil(t, s.dtlsAcknowledged)
	require.NoError(t, s.armDTLSBound(dtlsSequenceCheckpoint{epoch: 1, next: 9}))
	require.Equal(t, &dtlsSequenceCheckpoint{epoch: 1, next: 9}, s.dtlsProvisional)
}

func TestCheckpointEpochRefusalHasDistinctError(t *testing.T) {
	s := &session{}
	err := s.armDTLSBound(dtlsSequenceCheckpoint{epoch: 0, next: 7})
	require.ErrorIs(t, err, errBadState)
	require.NotErrorIs(t, err, ErrNotEstablished, "unexpected epoch-zero captures must not take the quiet not-established path")
}

func TestCheckpointEpochWarningIsLoggedOnce(t *testing.T) {
	logger := &cacheWarningLogger{}
	s := &session{log: logger, id: "plaintext-capture"}
	for range 3 {
		err := s.armDTLSBound(dtlsSequenceCheckpoint{epoch: 0, next: 7})
		require.ErrorIs(t, err, errDTLSCheckpointEpoch)
		s.warnSnapshot(err)
	}
	require.Equal(t, 1, logger.count())
	require.Nil(t, s.dtlsProvisional)
	require.Nil(t, s.dtlsAcknowledged)
	s.warnSnapshot(errBadState)
	require.Equal(t, 2, logger.count(), "ordinary snapshot failures retain their existing warning")
}
