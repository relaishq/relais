package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/relaishq/relais/internal/clockprobe"
	"github.com/stretchr/testify/require"
)

func TestClockProbeFlagsRequireCompleteOptInConfiguration(t *testing.T) {
	base := []string{"-listen=127.0.0.1:9443", "-control=http://127.0.0.1:9200"}
	targets := []string{"-clock-probe-target=control=http://127.0.0.1:9200", "-clock-probe-target=w1=http://127.0.0.1:9201", "-clock-probe-target=w2=http://127.0.0.1:9202"}
	for _, tc := range []struct {
		name string
		args []string
		ok   bool
	}{
		{"off", nil, true},
		{"complete", append(append([]string{"-clock-prober", "-clock-probe-file=probes"}, targets...), "-cert-out=certificate"), true},
		{"missing-targets-and-file", []string{"-clock-prober"}, false},
		{"missing-file", append([]string{"-clock-prober"}, targets...), false},
		{"missing-targets", []string{"-clock-prober", "-clock-probe-file=probes"}, false},
		{"partial-targets", append([]string{"-clock-prober", "-clock-probe-file=probes"}, targets[:2]...), false},
		{"file-without-opt-in", []string{"-clock-probe-file=probes"}, false},
		{"targets-without-opt-in", targets, false},
		{"explicitly-off", append([]string{"-clock-prober=false", "-clock-probe-file=probes"}, targets...), false},
		{"duplicate-target", append(append([]string{"-clock-prober", "-clock-probe-file=probes"}, targets...), targets[0]), false},
		{"public-target", []string{"-clock-probe-target=control=http://203.0.113.1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseConfig(append(append([]string{}, base...), tc.args...))
			if err == nil {
				_, err = c.validate()
			}
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestClockProberLifetimeJoinsBeforeClosingFile(t *testing.T) {
	for _, parentCancel := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "probes")
		synctest.Test(t, func(t *testing.T) {
			c := config{ClockProber: true, ClockProbeFile: path}
			require.NoError(t, c.ClockTargets.Set("control=http://127.0.0.1"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan struct{})
			var gotTargets clockprobe.Targets
			stop, err := startClockProber(ctx, c, func(ctx context.Context, targets clockprobe.Targets, log *clockprobe.Log) error {
				gotTargets = targets
				<-ctx.Done()
				// This final write must complete before stop closes the file.
				err := log.Append(clockprobe.Record{Target: clockprobe.Control, Result: clockprobe.RequestFailed})
				close(finished)
				return err
			})
			require.NoError(t, err)
			synctest.Wait()
			if parentCancel {
				cancel()
				synctest.Wait()
				select {
				case <-finished:
				default:
					t.Fatal("prober did not follow frontend cancellation")
				}
			}
			stop()
			<-finished
			require.Equal(t, c.ClockTargets, gotTargets)
		})
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Len(t, data, clockprobe.LineBytes)
		r, err := clockprobe.ParseLine(data)
		require.NoError(t, err)
		require.Equal(t, clockprobe.RequestFailed, r.Result)
	}
}
