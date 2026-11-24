//go:build !windows

package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/redistestenv"
	"github.com/stretchr/testify/require"
)

func TestWorkerRestartSupervisorHelper(t *testing.T) {
	path := os.Getenv("RELAIS_RESTART_HELPER_COUNT")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		os.Exit(2)
	}
	count, _ := strconv.Atoi(string(data))
	if os.WriteFile(path, []byte(strconv.Itoa(count+1)), 0600) != nil {
		os.Exit(2)
	}
	if os.Getenv("RELAIS_RESTART_HELPER_FAIL") == "1" && count < 2 {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestWorkerRestartSupervisorRetriesFailureAndStopsOnCleanExit(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(strconv.FormatBool(fails), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "attempts")
			env := append(os.Environ(), "RELAIS_RESTART_HELPER_COUNT="+path)
			if fails {
				env = append(env, "RELAIS_RESTART_HELPER_FAIL=1")
			}
			manager := &clusterprocess.Manager{}
			t.Cleanup(func() { manager.Stop(false) })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			delay := 20 * time.Millisecond
			started := time.Now()
			attempts := superviseWorker(ctx, manager, dir, env, delay, os.Args[0], "-test.run=^TestWorkerRestartSupervisorHelper$")
			count := 0
			for {
				select {
				case attempt, ok := <-attempts:
					if !ok {
						want := 1
						if fails {
							want = 3
							// Two failures each incur a delay, even with fast exits.
							require.GreaterOrEqual(t, time.Since(started), 2*delay)
						}
						require.Equal(t, want, count)
						data, err := os.ReadFile(path)
						require.NoError(t, err)
						require.Equal(t, strconv.Itoa(want), string(data))
						return
					}
					require.NoError(t, attempt.err)
					count++
				case <-ctx.Done():
					t.Fatal("supervisor did not stop after clean exit")
				}
			}
		})
	}
}

func TestWorkerRestartSupervisorCancellationStopsRetry(t *testing.T) {
	dir := t.TempDir()
	manager := &clusterprocess.Manager{}
	t.Cleanup(func() { manager.Stop(false) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := append(os.Environ(), "RELAIS_RESTART_HELPER_COUNT="+filepath.Join(dir, "attempts"), "RELAIS_RESTART_HELPER_FAIL=1")
	attempts := superviseWorker(ctx, manager, dir, env, time.Hour, os.Args[0], "-test.run=^TestWorkerRestartSupervisorHelper$")
	wait, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	child, err := nextWorkerAttempt(wait, attempts)
	require.NoError(t, err)
	select {
	case <-child.Done():
	case <-wait.Done():
		t.Fatal("helper did not exit")
	}
	cancel()
	select {
	case _, ok := <-attempts:
		require.False(t, ok, "cancellation must prevent another launch")
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not interrupt back-off")
	}
}

func TestWorkerRestartFlagsRefuseOtherFaultModes(t *testing.T) {
	for _, name := range []string{"runs", "after", "bin", "redis", "redis-server", "worker-restart", "relay-restart", "worker-stop", "scenario", "release-pair"} {
		flags := flag.NewFlagSet("restart", flag.ContinueOnError)
		flags.String(name, "", "")
		require.NoError(t, flags.Parse([]string{"-" + name + "=test"}))
		err := validateWorkerRestartFlags(flags)
		if strings.Contains(" runs after bin redis redis-server worker-restart ", " "+name+" ") {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
}

// Opt-in process coverage: two crashes on the same worker, using an existing
// dedicated test store. The ordinary unit lane exercises the supervisor above.
func TestWorkerRestartProcessDrill(t *testing.T) {
	if testing.Short() || os.Getenv("RELAIS_TEST_WORKER_RESTART") != "1" {
		t.Skip("set RELAIS_TEST_WORKER_RESTART=1 after make build, with dedicated test Redis")
	}
	addr := redistestenv.Address(t)
	require.NotEmpty(t, addr)
	bin, err := filepath.Abs("../../bin")
	require.NoError(t, err)
	manager := &clusterprocess.Manager{}
	ctx, stop := context.WithTimeout(context.Background(), time.Minute)
	defer stop()
	defer manager.Stop(false)
	env := processEnv(strings.Repeat("00", 32), addr, "relais:restart-test:"+strconv.FormatInt(time.Now().UnixNano(), 10)+":")
	require.NoError(t, workerRestartDrill(ctx, manager, bin, t.TempDir(), env, 2, time.Second))
}
