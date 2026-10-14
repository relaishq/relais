package udpbuffer

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func listen(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// hostLimits is the most each buffer can grant: rmem_max and wmem_max on
// Linux, kern.ipc.maxsockbuf for both on macOS.
func hostLimits(t *testing.T) Size {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		return Size{Read: procInt(t, "rmem_max"), Write: procInt(t, "wmem_max")}
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "kern.ipc.maxsockbuf").Output()
		require.NoError(t, err)
		n, err := strconv.Atoi(strings.TrimSpace(string(out)))
		require.NoError(t, err)
		return Size{Read: n, Write: n}
	default:
		t.Skipf("no effective-size expectations on %s", runtime.GOOS)
		return Size{}
	}
}

func procInt(t *testing.T, name string) int {
	t.Helper()
	data, err := os.ReadFile("/proc/sys/net/core/" + name)
	require.NoError(t, err)
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	return n
}

func TestApplyReportsEffectiveSize(t *testing.T) {
	limits := hostLimits(t)
	conn := listen(t)
	// Below every default cap, including Linux's 212992 before 6.18.
	want := Size{Read: 128 << 10, Write: 64 << 10}
	result := Apply(conn, want)
	require.NoError(t, result.Err)
	require.Equal(t, want, result.Requested)
	// Linux halves what getsockopt reports, so both platforms match the request.
	require.Equal(t, Size{Read: min(want.Read, limits.Read), Write: min(want.Write, limits.Write)}, result.Effective)
	require.Equal(t, result.Effective != want, result.Short())
}

func TestApplyReportsClampWithoutFailing(t *testing.T) {
	limits := hostLimits(t)
	conn := listen(t)
	huge := Size{Read: 1 << 30, Write: 1 << 30}
	result := Apply(conn, huge)
	// Linux clamps without an error; macOS does on a socket's first
	// over-limit request.
	require.NoError(t, result.Err)
	require.Equal(t, limits, result.Effective)
	require.True(t, result.Short())

	// The clamped socket still carries datagrams.
	_, err := conn.WriteToUDP([]byte("ping"), conn.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	buf := make([]byte, 16)
	n, _, err := conn.ReadFromUDP(buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf[:n]))
}

// opaque hides the socket's sizing methods, like a wrapped PacketConn.
type opaque struct{ net.PacketConn }

func TestApplyReportsUnsizeableSocket(t *testing.T) {
	result := Apply(opaque{listen(t)}, Caller)
	require.Error(t, result.Err)
	require.Zero(t, result.Effective)
	require.False(t, result.Short())
}

func TestLogWarnsOnlyWhenShort(t *testing.T) {
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

	Log(logf, "relay public", Result{Requested: Relay, Effective: Relay})
	require.Equal(t, []string{"relay public udp buffers: read effective=16777216 requested=16777216, write effective=4194304 requested=4194304"}, lines)

	lines = nil
	Log(logf, "worker", Result{Requested: Worker, Effective: Size{Read: 4 << 20, Write: Worker.Write}})
	require.Len(t, lines, 2)
	require.Equal(t, "warning: worker udp receive buffer is 4194304 bytes, below the 8388608 requested; raise "+limit("rmem_max")+" to 8388608 or more on the host", lines[1])
	if runtime.GOOS == "linux" {
		require.Contains(t, lines[1], "net.core.rmem_max")
	}

	lines = nil
	Log(logf, "caller", Result{Requested: Caller, Err: errors.New("boom")})
	require.Equal(t, []string{
		"caller udp buffers: read effective=unknown requested=4194304, write effective=unknown requested=1048576",
		"warning: caller udp buffers: boom",
	}, lines)
}
