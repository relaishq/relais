package main

import (
	"net/netip"
	"testing"

	"github.com/pion/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStartMedia checks the demo's two media paths: the worker alone on the
// media address, or a relay on it with the worker on a private socket
// behind it. Calls through both are covered by the call harness.
func TestStartMedia(t *testing.T) {
	addr := netip.MustParseAddrPort("127.0.0.1:0")

	t.Run("worker alone", func(t *testing.T) {
		m, err := startMedia(addr, false, logging.NewDefaultLoggerFactory())
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, m.Close()) })

		assert.Nil(t, m.relay)
		assert.Equal(t, m.worker.LocalAddr(), m.worker.MediaAddr(), "answers advertise the worker's socket")
	})

	t.Run("behind a relay", func(t *testing.T) {
		m, err := startMedia(addr, true, logging.NewDefaultLoggerFactory())
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, m.Close()) })

		require.NotNil(t, m.relay)
		assert.Equal(t, m.relay.PublicAddr(), m.worker.MediaAddr(), "answers advertise the relay")
		assert.NotEqual(t, m.relay.PublicAddr(), m.worker.LocalAddr(), "the worker has its own socket")
		assert.True(t, m.worker.LocalAddr().Addr().IsLoopback(), "the worker's socket is private")
	})
}
