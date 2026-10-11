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

func TestLinuxCurrentWithoutMachineID(t *testing.T) {
	for _, hostID := range []string{"", "explicit-host"} {
		reads := 0
		id, err := currentWithScope(hostID, func(override string) (string, string, error) {
			return scopeLinux(override, func(string) ([]byte, error) { reads++; return nil, os.ErrNotExist })
		})
		require.NoError(t, err)
		require.NotEmpty(t, id.Start)
		require.NotEmpty(t, id.Namespace)
		require.NotEmpty(t, id.Executable)
		if hostID == "" {
			require.Equal(t, UnknownHost, id.Host)
			require.Equal(t, 2, reads)
		} else {
			require.Equal(t, "host-id:"+hostID, id.Host)
			require.Zero(t, reads)
		}
	}
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
	id.Start = "previous-boot:" + ticks
	require.NoError(t, Fence(context.Background(), id))
	exited, err := Gone(context.Background(), id)
	require.NoError(t, err)
	require.True(t, exited)
}
func TestPidfdIdentityRejectsInvalidNamespaceAndRecognizesExit(t *testing.T) {
	require.NoError(t, checkPidfd("Pid:\t42\nNSpid:\t42\n", 42))
	require.ErrorIs(t, checkPidfd("Pid:\t-1\n", 42), unix.ESRCH)
	require.Error(t, checkPidfd("Pid:\t0\n", 42))
	require.Error(t, checkPidfd("Pid:\t43\n", 42))
	require.Error(t, checkPidfd("Pid:\tbroken\n", 42))
	require.Error(t, checkPidfd("flags:\t02000002\n", 42))
}
func TestLinuxPreviousBootNamespaceCannotSurvive(t *testing.T) {
	id, err := Current()
	require.NoError(t, err)
	_, ticks, ok := strings.Cut(id.Start, ":")
	require.True(t, ok)
	id.Start = "00000000-0000-0000-0000-000000000000:" + ticks
	id.Namespace = "previous-boot-namespace"
	exited, err := Gone(context.Background(), id)
	require.NoError(t, err)
	require.True(t, exited)
	require.NoError(t, Fence(context.Background(), id))
}
func TestLinuxHiddenProcEntryKeepsLivePinnedProcessUnverifiable(t *testing.T) {
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	require.NoError(t, err)
	defer func() { _ = unix.Close(fd) }()
	read := func(pid int) (string, bool, error) {
		return inspectPinnedWith(pid, func(int) (string, bool, error) { return "", false, os.ErrNotExist }, func() error { return checkPinned(fd, pid) }, func() error { return unix.PidfdSendSignal(fd, 0, nil, 0) })
	}
	id, err := Current()
	require.NoError(t, err)
	exited, err := gone(context.Background(), id, read, func(int) error { return nil })
	require.Error(t, err)
	require.False(t, exited)
}
