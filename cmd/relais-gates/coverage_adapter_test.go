package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatortransport"
)

// runSession runs the session command in-process with an empty stdin, so the
// session itself ends at its first read. It returns the exit code and stderr.
func runSession(t *testing.T, config operatorgates.ResourceConfig, start func(context.Context, string, []string) (*operatortransport.Subprocess, error), args ...string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	open := func(name string, flag int) *os.File {
		f, err := os.OpenFile(filepath.Join(dir, name), flag|os.O_CREATE, 0600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	oldArgs, oldIn, oldOut, oldErr := os.Args, os.Stdin, os.Stdout, os.Stderr
	defer func() { os.Args, os.Stdin, os.Stdout, os.Stderr = oldArgs, oldIn, oldOut, oldErr }()
	os.Args = append([]string{"relais-gates", "session", "-config", path}, args...)
	os.Stdin, os.Stdout, os.Stderr = open("stdin", os.O_RDONLY), open("stdout", os.O_WRONLY), open("stderr", os.O_WRONLY)
	code := runWithAdapter(start)
	stderr, err := os.ReadFile(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	return code, string(stderr)
}

func TestSessionRefusesUnfitTimingBeforeStart(t *testing.T) {
	fixture := sessionFixture(t)
	fixture.Config.ClockTiming = operatorgates.ClockTiming{PollSeconds: 3, HostReadLatencyMS: 4000}
	started := false
	code, stderr := runSession(t, fixture.Config, func(context.Context, string, []string) (*operatortransport.Subprocess, error) {
		started = true
		return nil, errors.New("unused")
	}, "-coverage-adapter", "/unused")
	if code != 2 || stderr != "operator-config-refused: clock-timing-lease-validity\n" || started {
		t.Fatal("MUTATION: timing-refused-before-session-start", code, stderr, started)
	}
}

func TestSessionStartsCoverageAdapter(t *testing.T) {
	fixture := sessionFixture(t)
	fixture.Config.ClockTiming = operatorgates.ClockTiming{PollSeconds: 8, HostReadLatencyMS: 4000}
	var path string
	code, stderr := runSession(t, fixture.Config, func(ctx context.Context, executable string, args []string) (*operatortransport.Subprocess, error) {
		path = executable
		if len(args) != 0 {
			t.Fatal("coverage adapter received arguments")
		}
		return operatortransport.Start(ctx, "/bin/cat", nil)
	}, "-coverage-adapter", "/opt/adapter/coverage")
	// The empty stdin ends the session after the adapter has started.
	if path != "/opt/adapter/coverage" || code != 1 || !strings.Contains(stderr, "operator-session-incomplete") {
		t.Fatal("MUTATION: coverage-adapter-not-started", path, code, stderr)
	}
	code, stderr = runSession(t, fixture.Config, func(context.Context, string, []string) (*operatortransport.Subprocess, error) {
		return nil, errors.New("adapter-start-failed")
	}, "-coverage-adapter", "/opt/adapter/coverage")
	if code != 1 || stderr != "operator-coverage-adapter-unavailable\n" {
		t.Fatal("a missing coverage adapter did not stop the session", code, stderr)
	}
	started := false
	code, _ = runSession(t, fixture.Config, func(context.Context, string, []string) (*operatortransport.Subprocess, error) {
		started = true
		return nil, errors.New("unused")
	})
	if started || code != 1 {
		t.Fatal("a session without a coverage adapter started one", code)
	}
}
