package redistestcluster

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/processidentity"
)

const watchdogEnvironment = "RELAIS_TEST_REDIS_WATCHDOG"

// A separate copy of the test binary retains the pipe's read end. Unlike a
// cleanup callback or an in-process goroutine, it survives panic, timeout and
// SIGKILL of the owning test. Exact process identity prevents PID-reuse kills.
func init() {
	if os.Getenv(watchdogEnvironment) != "1" {
		return
	}
	var target processidentity.Identity
	if json.NewDecoder(os.Stdin).Decode(&target) != nil {
		os.Exit(2)
	}
	if _, err := os.Stdout.Write([]byte{1}); err != nil {
		os.Exit(2)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	err := processidentity.Fence(ctx, target)
	cancel()
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func watchServer(t *testing.T, child *clusterprocess.Child) {
	t.Helper()
	target, err := processidentity.Observe(child.PID())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0])
	configureWatchdog(cmd)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GORACE=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, watchdogEnvironment+"=1", "GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		_ = input.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = input.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Redis watchdog: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("Redis watchdog did not exit")
		}
	})
	if err := json.NewEncoder(input).Encode(target); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() { var b [1]byte; _, err := io.ReadFull(output, b[:]); ready <- err }()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Redis watchdog readiness timed out")
	}
}
