package processidentity

import (
	"context"
	"fmt"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseStatCommParentheses(t *testing.T) {
	start, dead, err := parseStat("123 (worker (test)) Z 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 123456")
	require.NoError(t, err)
	require.Equal(t, "123456", start)
	require.True(t, dead)
	_, _, err = parseStat("123 broken")
	require.Error(t, err)
}

// Capture only the owned test child's threads, descriptors and UDP port.
// A leader can be Z while another thread still references its shared files.
func logSocketExitState(t *testing.T, pid int, udpAddr string) {
	t.Helper()
	proc := fmt.Sprintf("/proc/%d", pid)
	tasks, err := os.ReadDir(filepath.Join(proc, "task"))
	t.Logf("fenced socket diagnostics: pid=%d task_count=%d error=%v", pid, len(tasks), err)
	for _, task := range tasks {
		base := filepath.Join(proc, "task", task.Name())
		stat, statErr := os.ReadFile(filepath.Join(base, "stat"))
		children, childrenErr := os.ReadFile(filepath.Join(base, "children"))
		t.Logf("tid=%s stat=%s error=%v children=%s error=%v", task.Name(), stat, statErr, children, childrenErr)
		fds, fdErr := os.ReadDir(filepath.Join(base, "fd"))
		t.Logf("tid=%s fd_list_error=%v", task.Name(), fdErr)
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(base, "fd", fd.Name()))
			t.Logf("tid=%s fd=%s target=%s error=%v", task.Name(), fd.Name(), target, err)
		}
	}
	_, port, err := net.SplitHostPort(udpAddr)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	output, err := exec.CommandContext(ctx, "ss", "-lunp", "sport = :"+port).CombinedOutput()
	t.Logf("owned UDP socket ss=%s error=%v", output, err)
}

func TestLinuxBootIdentityMakesSameTicksFromOldBootAlreadyFenced(t *testing.T) {
	id, err := Current()
	require.NoError(t, err)
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(id.Start, strings.TrimSpace(string(boot))+":"))
	_, ticks, ok := strings.Cut(id.Start, ":")
	require.True(t, ok)
	require.NoError(t, Fence(context.Background(), Identity{PID: id.PID, Start: "previous-boot:" + ticks}))
}
func TestPidfdIdentityRejectsInvalidNamespaceAndRecognizesExit(t *testing.T) {
	require.NoError(t, checkPidfd("Pid:\t42\nNSpid:\t42\n", 42))
	require.ErrorIs(t, checkPidfd("Pid:\t-1\n", 42), unix.ESRCH)
	require.Error(t, checkPidfd("Pid:\t0\n", 42))
	require.Error(t, checkPidfd("Pid:\t43\n", 42))
	require.Error(t, checkPidfd("Pid:\tbroken\n", 42))
	require.Error(t, checkPidfd("flags:\t02000002\n", 42))
}
