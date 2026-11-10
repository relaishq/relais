package operatorrehearsal

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

const fixtureLeaseCPU = "\nclock-lease-cpu\n0m0.001s 0m0.002s\n0m0.023s 0m0.004s\n"

func TestLeaseCPUDoesNotChangeEvidence(t *testing.T) {
	for _, size := range []int{0, operator.MaxHostOutputBytes, operator.MaxHostOutputBytes + 1} {
		for _, exit := range []int{0, 3, 137} {
			t.Run(fmt.Sprintf("bytes=%d/exit=%d", size, exit), func(t *testing.T) {
				var diagnostics bytes.Buffer
				a := &Adapter{Topology: fixtureTopology(t), Containers: fixtureContainerIDs(), Diagnostics: &diagnostics}
				stderr := bytes.Repeat([]byte("x"), size)
				result := CommandResult{Code: code(exit), Stdout: []byte("observation"), Stderr: stderr}
				want := a.classify("1", result)
				result.Stderr = append(append([]byte(nil), stderr...), fixtureLeaseCPU...)
				a.Runner = &fakeRunner{result: result}
				ctx, cancel := context.WithTimeout(context.Background(), 1800*time.Millisecond)
				defer cancel()
				req := request()
				req.Operation, req.Action = operator.HostLeaseRead, "clock"
				reply := a.executeHost(ctx, req)
				require.Equal(t, want, reply, "MUTATION: lease-cpu-evidence")
				require.Contains(t, diagnostics.String(), "cpuChildrenUser=0m0.023s cpuChildrenSys=0m0.004s", "MUTATION: lease-cpu-diagnostic")
			})
		}
	}
	for _, stderr := range []string{"", "private diagnostic", "\nclock-lease-cpu\nprivate diagnostic\n"} {
		clean, cpu := splitLeaseCPU([]byte(stderr))
		require.Equal(t, stderr, string(clean))
		require.Equal(t, "cpu=unavailable", cpu)
	}
}

func TestLeaseCPUWrapperPreservesExit(t *testing.T) {
	for _, exit := range []string{"0", "3"} {
		t.Run(exit, func(t *testing.T) {
			runner := &fakeRunner{result: CommandResult{Code: code(0)}}
			a := &Adapter{Topology: fixtureTopology(t), Containers: fixtureContainerIDs(), Runner: runner}
			req := request()
			req.Operation, req.Action, req.Script = operator.HostLeaseRead, "clock", []byte("printf stdout; printf stderr >&2; exit "+exit)
			ctx, cancel := context.WithTimeout(context.Background(), 1800*time.Millisecond)
			defer cancel()
			a.executeHost(ctx, req)
			c := runner.commands[0]
			// The local sandbox forbids setpriority. Command construction owns
			// nice; exercise the real shell trap and exit status without it.
			wrapper := strings.ReplaceAll(c.Args[len(c.Args)-1], "nice -n 10 ", "")
			cmd := exec.Command("sh", "-c", wrapper)
			cmd.Stdin = bytes.NewReader(c.Input)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if exit == "0" {
				require.NoError(t, err)
			} else {
				var failed *exec.ExitError
				require.ErrorAs(t, err, &failed, "MUTATION: lease-cpu-exit")
				require.Equal(t, 3, failed.ExitCode(), "MUTATION: lease-cpu-exit")
			}
			clean, cpu := splitLeaseCPU(stderr.Bytes())
			require.Equal(t, "stdout", stdout.String())
			require.Equal(t, "stderr", string(clean))
			require.NotEqual(t, "cpu=unavailable", cpu)
		})
	}
}

func TestLeaseCPUCapturePreservesStderrBound(t *testing.T) {
	for _, size := range []int{operator.MaxHostOutputBytes, operator.MaxHostOutputBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			command := Command{Label: "host-step", LeaseCPU: true, Args: []string{"sh", "-c", fmt.Sprintf("printf '%%0%dd' 0 >&2; printf '%%s' '%s' >&2", size, fixtureLeaseCPU)}}
			result := (LocalRunner{}).Run(context.Background(), command)
			require.False(t, result.Truncated, "MUTATION: lease-cpu-capture")
			clean, cpu := splitLeaseCPU(result.Stderr)
			require.Len(t, clean, size)
			require.NotEqual(t, "cpu=unavailable", cpu)
			result.Stderr = clean
			a := &Adapter{Topology: fixtureTopology(t)}
			want := operator.Succeeded
			if size > operator.MaxHostOutputBytes {
				want = operator.Unknown
			}
			require.Equal(t, want, a.classify("1", result).Transport)
		})
	}
}
