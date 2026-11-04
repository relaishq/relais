package clusterprocess

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/processidentity"
	"github.com/stretchr/testify/require"
)

type parentDeathReady struct {
	Ready
	Reaper    int
	Bystander processidentity.Identity
}

func TestDarwinParentDeath(t *testing.T) {
	dir := os.Getenv("RELAIS_TEST_PARENT_DEATH_DIR")
	switch os.Getenv("RELAIS_TEST_PARENT_DEATH_ROLE") {
	case "leaf":
		// A descendant in the same group proves cleanup reaches beyond the leader.
		cmd := exec.Command("/bin/sleep", "60")
		require.NoError(t, cmd.Start())
		_ = json.NewEncoder(os.Stdout).Encode(Ready{HTTP: strconv.Itoa(os.Getppid()), PID: os.Getpid()})
		deadline := time.Now().Add(30 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil || time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				os.Exit(0)
			}
			time.Sleep(20 * time.Millisecond)
		}
	case "parent":
		executable, err := os.Executable()
		require.NoError(t, err)
		env := append(os.Environ(), "RELAIS_TEST_PARENT_DEATH_ROLE=leaf")
		child, err := Start(dir, "leaf", env, executable, "-test.run=^TestDarwinParentDeath$")
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ready, err := child.Ready(ctx)
		require.NoError(t, err)
		require.Equal(t, strconv.Itoa(os.Getpid()), ready.HTTP)
		require.NotNil(t, parentReaper.cmd)
		// A direct, unregistered sibling must survive the same launcher's death.
		path := filepath.Join(dir, "bystander.log")
		log, err := os.Create(path)
		require.NoError(t, err)
		command := exec.Command(executable, "-test.run=^TestDarwinParentDeath$")
		command.Env, command.Stdout, command.Stderr = env, log, log
		require.NoError(t, configureCommand(command))
		require.NoError(t, command.Start())
		bystander := &Child{cmd: command, pid: command.Process.Pid, done: make(chan struct{}), log: log, path: path}
		go bystander.wait()
		defer bystander.Stop()
		bystanderReady, err := bystander.Ready(ctx)
		require.NoError(t, err)
		require.Equal(t, strconv.Itoa(os.Getpid()), bystanderReady.HTTP)
		identity, err := processidentity.Observe(bystander.PID())
		require.NoError(t, err)
		data, err := json.Marshal(parentDeathReady{Ready: ready, Reaper: parentReaper.cmd.Process.Pid, Bystander: identity})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "ready"), data, 0600))
		if os.Getenv("RELAIS_TEST_PARENT_EXIT") == "normal" {
			os.Exit(0) // No cleanup callback, as with a test timeout or panic.
		}
		<-child.Done()
		os.Exit(1)
	}

	if _, err := processidentity.Observe(os.Getpid()); err != nil {
		t.Skipf("process identities unavailable: %v", err)
	}
	for _, exit := range []string{"killed", "normal"} {
		t.Run(exit, func(t *testing.T) {
			dir := t.TempDir()
			// On a red run, the leaf stops its own descendant. Never signal a
			// numeric PID or group that might have been reused after parent death.
			var ready parentDeathReady
			t.Cleanup(func() {
				_ = os.WriteFile(filepath.Join(dir, "stop"), nil, 0600)
				for _, pid := range []int{ready.PID, ready.Bystander.PID} {
					if pid > 0 {
						require.Eventually(t, func() bool {
							return syscall.Kill(-pid, 0) == syscall.ESRCH
						}, 5*time.Second, 20*time.Millisecond, "fallback did not stop owned group %d", pid)
					}
				}
			})
			executable, err := os.Executable()
			require.NoError(t, err)
			cmd := exec.Command(executable, "-test.run=^TestDarwinParentDeath$")
			cmd.Env = append(os.Environ(), "RELAIS_TEST_PARENT_DEATH_ROLE=parent", "RELAIS_TEST_PARENT_DEATH_DIR="+dir, "RELAIS_TEST_PARENT_EXIT="+exit)
			log, err := os.Create(filepath.Join(dir, "parent.log"))
			require.NoError(t, err)
			defer func() { _ = log.Close() }()
			cmd.Stdout, cmd.Stderr = log, log
			require.NoError(t, cmd.Start())
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(filepath.Join(dir, "ready"))
				return err == nil && json.Unmarshal(data, &ready) == nil
			}, 10*time.Second, 20*time.Millisecond)
			require.Positive(t, ready.PID)
			if exit != "normal" {
				require.NoError(t, cmd.Process.Kill())
			}
			_ = cmd.Wait()
			require.Eventually(t, func() bool {
				return syscall.Kill(-ready.PID, 0) == syscall.ESRCH
			}, 5*time.Second, 20*time.Millisecond, "owned group %d survived parent %s", ready.PID, exit)
			require.Eventually(t, func() bool {
				return syscall.Kill(ready.Reaper, 0) == syscall.ESRCH
			}, 5*time.Second, 20*time.Millisecond, "reaper survived parent %s", exit)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			gone, err := processidentity.Gone(ctx, ready.Bystander)
			require.NoError(t, err)
			require.False(t, gone, "unrecorded sibling was affected")
		})
	}
}

func TestDarwinChildKeepsLauncherParent(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	env := append(os.Environ(), "RELAIS_TEST_PARENT_DEATH_ROLE=leaf", "RELAIS_TEST_PARENT_DEATH_DIR="+dir)
	child, err := Start(dir, "parent-pid", env, executable, "-test.run=^TestDarwinParentDeath$")
	require.NoError(t, err)
	defer child.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready, err := child.Ready(ctx)
	require.NoError(t, err)
	require.Equal(t, strconv.Itoa(os.Getpid()), ready.HTTP)
}
