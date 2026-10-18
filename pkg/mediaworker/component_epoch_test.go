package mediaworker

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckpointRefusesPlaintextDTLSEpoch(t *testing.T) {
	s := &session{}
	require.ErrorIs(t, s.armDTLSBound(dtlsSequenceCheckpoint{epoch: 0, next: 7}), ErrNotEstablished)
	require.Nil(t, s.dtlsProvisional)
	require.Nil(t, s.dtlsAcknowledged)
	require.NoError(t, s.armDTLSBound(dtlsSequenceCheckpoint{epoch: 1, next: 9}))
	require.Equal(t, &dtlsSequenceCheckpoint{epoch: 1, next: 9}, s.dtlsProvisional)
}
