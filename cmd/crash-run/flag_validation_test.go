package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCrashRunFlagValidationHelper(t *testing.T) {
	encoded := os.Getenv("RELAIS_FLAG_VALIDATION_ARGS")
	if encoded == "" {
		return
	}
	var args []string
	require.NoError(t, json.Unmarshal([]byte(encoded), &args))
	flag.CommandLine = flag.NewFlagSet("crash-run", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{os.Args[0]}, args...)
	err := run()
	require.Error(t, err, "validation must stop before a drill starts")
	fmt.Println(err)
}

// Exercise the actual CLI guards. Even an accidentally accepted combination
// stops at a missing binary directory, before sockets or child services start.
func TestCrashRunFlagRefusalsPreserveFaultModes(t *testing.T) {
	type refusal struct {
		args []string
		want string
	}
	var cases []refusal
	competing := []string{"-relay-standby", "-relay-restart", "-sigterm", "-worker-stop", "-buffer", "-compare-buffer", "-store-stall=live", "-scenario=move", "-agent=custom"}
	for _, other := range append(append([]string{}, competing...), "-relay-loss", "-relay-loss-processes", "-relay-loss-netns", "-release-pair", "-topology=netns") {
		cases = append(cases, refusal{[]string{"-relay-drain", other}, "relay-drain requires"})
	}
	for _, mode := range []string{"-relay-loss", "-relay-loss-processes", "-relay-loss-netns"} {
		args := []string{mode}
		if mode == "-relay-loss-netns" {
			args = append(args, "-topology=netns")
		}
		for _, other := range competing {
			cases = append(cases, refusal{append(append([]string{}, args...), other), "relay-loss requires echo"})
		}
	}
	cases = append(cases,
		refusal{[]string{"-relay-loss", "-relay-loss-processes"}, "select only one relay-loss mode"},
		refusal{[]string{"-relay-loss", "-relay-loss-netns"}, "select only one relay-loss mode"},
		refusal{[]string{"-relay-loss-processes", "-relay-loss-netns"}, "select only one relay-loss mode"},
		refusal{[]string{"-relay-loss-netns"}, "relay-loss-netns requires -topology=netns"},
		refusal{[]string{"-relay-loss", "-topology=netns"}, "process-signal relay loss requires the loopback topology"},
		refusal{[]string{"-relay-loss-processes", "-topology=netns"}, "process-signal relay loss requires the loopback topology"},
		refusal{[]string{"-worker-restart", "-runs=1"}, "worker-restart requires at least two crashes"},
	)
	for _, other := range []string{"-relay-drain", "-relay-drain=false", "-relay-loss-netns", "-relay-loss", "-relay-loss-processes", "-relay-restart", "-worker-stop", "-release-pair", "-topology=loopback"} {
		cases = append(cases, refusal{[]string{"-worker-restart", other}, "worker-restart cannot combine"})
	}
	missing := filepath.Join(t.TempDir(), "missing-binaries")
	for _, allowed := range [][]string{{"-relay-drain"}, {"-relay-loss"}, {"-relay-loss-processes"}, {"-relay-loss-netns", "-topology=netns"}, {"-worker-restart", "-runs=2", "-after=1s", "-redis=127.0.0.1:16453", "-redis-server=unused"}} {
		cases = append(cases, refusal{allowed, missing})
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.args), func(t *testing.T) {
			args := append(append([]string{}, tc.args...), "-bin="+missing)
			encoded, err := json.Marshal(args)
			require.NoError(t, err)
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCrashRunFlagValidationHelper$")
			cmd.Env = append(os.Environ(), "RELAIS_REDIS_ADDR=", "RELAIS_FLAG_VALIDATION_ARGS="+string(encoded))
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
			require.Contains(t, string(output), tc.want)
		})
	}
}
