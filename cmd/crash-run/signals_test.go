package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDriverDoubleSignalCleanup(t *testing.T) {
	switch os.Getenv("RELAIS_TEST_DRIVER_ROLE") {
	case "leaf":
		signal.Ignore(os.Interrupt, syscall.SIGTERM)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		// The outer test can safely stop leaked descendants on a red run through
		// their loopback listeners, without signalling any potentially reused PID.
		go func() {
			conn, err := listener.Accept()
			if err == nil {
				_ = conn.Close()
				os.Exit(0)
			}
		}()
		_ = json.NewEncoder(os.Stdout).Encode(ready{HTTP: listener.Addr().String(), PID: os.Getpid()})
		select {}
	case "manager":
		manager := &processManager{}
		ctx, stop := manager.context()
		defer stop()
		dir := os.Getenv("RELAIS_TEST_DRIVER_DIR")
		var children []*child
		for _, name := range []string{"fake-child", "fake-redis"} {
			env := append(os.Environ(), "RELAIS_TEST_DRIVER_ROLE=leaf")
			c, err := manager.start(dir, name, env, os.Args[0], "-test.run=^TestDriverDoubleSignalCleanup$")
			require.NoError(t, err)
			children = append(children, c)
			defer c.stop()
			startup, cancel := context.WithTimeout(ctx, 3*time.Second)
			_, err = c.ready(startup)
			cancel()
			require.NoError(t, err)
		}
		_ = json.NewEncoder(os.Stdout).Encode([]int{children[0].pid, children[1].pid})
		<-ctx.Done()
		fmt.Println("cleanup")
		return
	}
	root, err := filepath.Abs(filepath.Join("..", "..", "bin"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(root, 0o755))
	dir, err := os.MkdirTemp(root, "driver-signals-test-")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	defer func() {
		for _, name := range []string{"fake-child", "fake-redis"} {
			data, err := os.ReadFile(filepath.Join(dir, name+".log"))
			if err != nil {
				continue
			}
			var leaf ready
			if json.Unmarshal(data, &leaf) != nil {
				continue
			}
			conn, err := net.DialTimeout("tcp", leaf.HTTP, time.Second)
			if err == nil {
				_ = conn.Close()
			}
		}
	}()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDriverDoubleSignalCleanup$")
	cmd.Env = append(os.Environ(), "RELAIS_TEST_DRIVER_ROLE=manager", "RELAIS_TEST_DRIVER_DIR="+dir)
	output, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()
	scanner := bufio.NewScanner(output)
	require.True(t, scanner.Scan())
	var pids []int
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &pids))
	require.Len(t, pids, 2)
	require.NoError(t, cmd.Process.Signal(os.Interrupt))
	require.True(t, scanner.Scan())
	require.Equal(t, "cleanup", scanner.Text())
	require.NoError(t, cmd.Process.Signal(os.Interrupt))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("second signal did not force a prompt driver exit")
	}
	for _, pid := range pids {
		require.Eventually(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH }, time.Second, 10*time.Millisecond, "owned child %d survived double SIGINT", pid)
	}
}
