package main

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCallerHostBindWithoutNamespace(t *testing.T) {
	external, err := childTopology("http://127.0.0.1/calls", netip.MustParseAddrPort("127.0.0.1:20000"), "", "127.0.0.1:0")
	require.NoError(t, err)
	require.NotNil(t, external.CallerSocket)
	conn, err := external.CallerSocket()
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.Equal(t, "127.0.0.1", conn.LocalAddr().(*net.UDPAddr).IP.String())
	require.NotZero(t, conn.LocalAddr().(*net.UDPAddr).Port)
	_, err = conn.WriteTo([]byte("host socket"), conn.LocalAddr())
	require.NoError(t, err)
	buf := make([]byte, 32)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	n, _, err := conn.ReadFrom(buf)
	require.NoError(t, err)
	require.Equal(t, "host socket", string(buf[:n]))
}

func TestExternalCallerRequiresBindForRemoteRelay(t *testing.T) {
	output := filepath.Join(t.TempDir(), "caller.jsonl")
	err := runArgs([]string{"-external-caller", "-relay=192.0.2.10:41000", "-output=" + output})
	require.ErrorContains(t, err, "non-loopback relay requires -caller-bind")
	_, err = os.Stat(output)
	require.ErrorIs(t, err, os.ErrNotExist, "reject before creating output or dialing")
}

func TestExternalCallerRejectsSharedFixedBindPort(t *testing.T) {
	err := runArgs([]string{"-external-caller", "-relay=127.0.0.1:41000", "-caller-bind=127.0.0.1:41001", "-callers=2", "-output=" + filepath.Join(t.TempDir(), "caller.jsonl")})
	require.ErrorContains(t, err, "multiple external callers require -caller-bind port 0")
}

func TestCallerProcessFlag(t *testing.T) {
	output := filepath.Join(t.TempDir(), "caller.jsonl")
	err := runArgs([]string{"-caller-process=1", "-output=" + output})
	require.ErrorContains(t, err, "-caller-process requires -external-caller or -child")
	err = runArgs([]string{"-external-caller", "-seed=7", "-caller-process=-1", "-relay=127.0.0.1:41000", "-output=" + output})
	require.ErrorContains(t, err, "caller-process must be >= 0", "external callers accept -seed and -caller-process")
	_, err = os.Stat(output)
	require.ErrorIs(t, err, os.ErrNotExist, "reject before creating output or dialing")
}
