package main

import (
	"context"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/redistestcluster"
	"github.com/stretchr/testify/require"
)

// An opt-in real-process integration smoke on either Mac or Linux. It drives
// the production profile trial seam with a loopback LAN transport, so it can
// prove moves/buffering/report integration without claiming netem evidence.
func TestProfileProcessLoopbackProbe(t *testing.T) {
	if testing.Short() || os.Getenv("RELAIS_TEST_PROFILE_PROCESS") != "1" {
		t.Skip("set RELAIS_TEST_PROFILE_PROCESS=1 after make build; owns throwaway Redis, needs ffmpeg")
	}
	bin, err := filepath.Abs("../../bin")
	require.NoError(t, err)
	require.NoError(t, binaries(bin))
	manager := &clusterprocess.Manager{}
	ctx, cancel := manager.Context()
	defer cancel()
	dir := redistestcluster.Directory(t, "process")
	addr, err := clusterprocess.FreeTCP()
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	args, remove, err := redisCommand(nil, "redis-server", port, dir, "")
	require.NoError(t, err)
	defer remove()
	redis, err := manager.Start(dir, "redis", os.Environ(), args...)
	require.NoError(t, err)
	defer redis.Stop()
	startup, stop := context.WithTimeout(ctx, 5*time.Second)
	require.NoError(t, clusterprocess.WaitTCP(startup, addr, redis))
	stop()
	runProcessProfileProbe(t, ctx, manager, bin, dir, processEnv(strings.Repeat("00", 32), addr, "relais:profile-probe:"+filepath.Base(dir)+":"), nil)
}

func runProcessProfileProbe(t *testing.T, ctx context.Context, manager *clusterprocess.Manager, bin, dir string, env []string, storeFault func(context.Context, string, string) error) {
	t.Helper()
	for _, scenario := range []string{"move", "takeover"} {
		t.Run(scenario, func(t *testing.T) {
			trialEnv := freshTrialEnv(env)
			options := profileTrialOptions{Name: "lan", Scenario: scenario, Links: profileTestLinks(t, "lan")}
			if storeFault != nil {
				options.StoreFault = func(ctx context.Context, id string) error {
					return storeFault(ctx, id, envValue(trialEnv, "RELAIS_REDIS_PREFIX"))
				}
			}
			trialDir := filepath.Join(dir, scenario)
			require.NoError(t, os.Mkdir(trialDir, 0700))

			trialCtx, stop := context.WithTimeout(ctx, 30*time.Second)
			defer stop()
			r, err := trial(trialCtx, manager, bin, trialDir, trialEnv, 5*time.Second, 5*time.Second, false, false, false, false, nil, &options)
			require.NoError(t, err)
			require.NotNil(t, r.ProfileRow)
			require.Positive(t, r.ProfileRow.EventResult.Duration, "acceptance event must retain real controller timings")
			require.Positive(t, r.ProfileRow.EventResult.Resume)
			require.True(t, r.ProfileRow.AudioGapWindow.Complete)
			require.True(t, r.ProfileRow.VideoGapWindow.Complete)
			require.True(t, r.ProfileRow.ObservedUncertainMoveResumesAvailable)
			require.Zero(t, r.ProfileRow.ObservedUncertainMoveResumes)
			if scenario == "move" {
				require.Positive(t, r.ProfileRow.EventResult.Drain)
				require.Positive(t, r.ProfileRow.EventResult.Export)
			}
			require.False(t, r.ProfileRow.Integrity.Failed, r.ProfileRow.Integrity.Reasons)
			require.False(t, r.ProfileRow.Integrity.Trusted, "short smoke cannot claim 60s consent")
			require.Equal(t, []string{"60s consent observation NOT-RUN"}, r.ProfileRow.Integrity.Reasons)
			require.GreaterOrEqual(t, r.ProfileRow.AudioBaseline.Samples, 10)
			require.GreaterOrEqual(t, r.ProfileRow.VideoBaseline.Samples, 10)
			loss := r.ProfileRow.Measurement.ContentLoss
			require.True(t, loss.Event.Audio.Trusted)
			require.True(t, loss.Event.Audio.RunTrusted)
			require.True(t, loss.Event.Video.RunTrusted)
			require.True(t, loss.Event.Video.Trusted)
			require.Positive(t, loss.Event.Audio.Sent)
			require.Positive(t, loss.Event.Video.Sent)
			require.NotEmpty(t, loss.Baselines, "real driver must supply no-event loss windows")
			for _, w := range loss.Baselines {
				require.Equal(t, loss.Event.End-loss.Event.Start, w.End-w.Start)
				require.True(t, w.Audio.Trusted)
				require.True(t, w.Video.Trusted)
			}
			if scenario == "takeover" {
				require.True(t, r.Measurement.AudioLoss.Trusted, r.Measurement.Summary())
				require.True(t, r.Measurement.VideoLoss.Trusted, r.Measurement.Summary())
				require.Zero(t, r.Measurement.LostAudio, r.Measurement.Summary())
				require.Zero(t, r.Measurement.LostVideoFrames, r.Measurement.Summary())
			}
			require.Equal(t, "inconclusive", judgeProfile(options, []profileRow{*r.ProfileRow}, profileTestLinks(t, options.Name)).Verdict)
		})
	}
}

func freshTrialEnv(env []string) []string {
	out := append([]string(nil), env...)
	prefix := envValue(env, "RELAIS_REDIS_PREFIX") + rand.Text() + ":"
	for i, entry := range out {
		if strings.HasPrefix(entry, "RELAIS_REDIS_PREFIX=") {
			out[i] = "RELAIS_REDIS_PREFIX=" + prefix
			return out
		}
	}
	return append(out, "RELAIS_REDIS_PREFIX="+prefix)
}
func TestClusterTrialNamespaceIsolation(t *testing.T) {
	base := []string{"RELAIS_REDIS_PREFIX=base:", "OTHER=kept"}
	first, second := freshTrialEnv(base), freshTrialEnv(base)
	require.NotEqual(t, envValue(first, "RELAIS_REDIS_PREFIX"), envValue(second, "RELAIS_REDIS_PREFIX"))
	require.Equal(t, "base:", envValue(base, "RELAIS_REDIS_PREFIX"))
	require.Equal(t, "kept", envValue(first, "OTHER"))
}
