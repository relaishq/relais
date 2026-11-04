package clusterprocess

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/processidentity"
)

const reaperEnvironment = "RELAIS_CLUSTERPROCESS_REAPER"

var parentReaper struct {
	once  sync.Once
	mu    sync.Mutex
	input *os.File
	cmd   *exec.Cmd
}

// A single copy of the launcher survives its death and reads EOF when the
// launcher closes its only pipe writer. Children retain their original parent.
func init() {
	if os.Getenv(reaperEnvironment) != "1" {
		return
	}
	runParentReaper(os.Stdin)
	os.Exit(0)
}

func runParentReaper(input io.Reader) {
	targets := make(map[int]processidentity.Identity)
	decoder := json.NewDecoder(input)
	for {
		var target processidentity.Identity
		if decoder.Decode(&target) != nil {
			break
		}
		targets[target.PID] = target
	}
	// A decode error alone does not mean the launcher died. Wait for actual
	// pipe EOF before acting, retaining only complete identity records.
	if _, err := io.Copy(io.Discard, input); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, target := range targets {
		group, err := syscall.Getpgid(target.PID)
		if err != nil || group != target.PID {
			continue
		}
		gone, err := processidentity.Gone(ctx, target)
		if err == nil && !gone {
			_ = syscall.Kill(-target.PID, syscall.SIGKILL)
		}
	}
}

// Reaping is best effort: helper or identity failures never change Start's result.
// Linux retains its Pdeathsig path.
func prepareParentReaper() {
	// Shipped tools must keep their exact process tree; only Go tests get a reaper.
	if !testing.Testing() {
		return
	}
	parentReaper.once.Do(func() {
		// RELAIS_CLUSTERPROCESS_NO_REAPER=1 allows fixtures to leave intentional orphans.
		if os.Getenv("RELAIS_CLUSTERPROCESS_NO_REAPER") == "1" {
			return
		}
		executable, err := os.Executable()
		if err != nil {
			return
		}
		input, writer, err := os.Pipe()
		if err != nil {
			return
		}
		defer func() { _ = input.Close() }()
		cmd := exec.Command(executable)
		if err := configureCommand(cmd); err != nil {
			_ = writer.Close()
			return
		}
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "GORACE=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, reaperEnvironment+"=1", "GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
		cmd.Stdin = input
		if err := cmd.Start(); err != nil {
			_ = writer.Close()
			return
		}
		parentReaper.input, parentReaper.cmd = writer, cmd
		go func() { _ = cmd.Wait() }()
	})
}

func recordParentChild(pid int) {
	parentReaper.mu.Lock()
	defer parentReaper.mu.Unlock()
	if parentReaper.input == nil {
		return
	}
	target, err := processidentity.Observe(pid)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "clusterprocess: child %d not tracked by parent reaper: %v\n", pid, err)
		return
	}
	if err := json.NewEncoder(parentReaper.input).Encode(target); err != nil {
		_ = parentReaper.input.Close()
		parentReaper.input = nil
	}
}
