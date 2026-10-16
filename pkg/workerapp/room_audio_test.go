package workerapp

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRoomAudioSelectionIsOptIn(t *testing.T) {
	cfg, err := selectAgent("echo", "udp://127.0.0.1:1234")
	require.NoError(t, err)
	require.Nil(t, cfg.Factory)
	_, err = selectAgent("room-audio")
	require.Error(t, err)
	_, err = selectAgent("room-audio", "")
	require.Error(t, err)
	cfg, err = selectAgent("room-audio", "udp://127.0.0.1:1234")
	require.NoError(t, err)
	require.NotNil(t, cfg.Factory)
}
