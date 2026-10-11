package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/stretchr/testify/require"
)

// An opt-in real-process integration smoke on either Mac or Linux. It drives
// the production profile trial seam with a loopback LAN transport, so it can
// prove moves/buffering/report integration without claiming netem evidence.
func TestProfileProcessLoopbackProbe(t *testing.T) {
	if os.Getenv("RELAIS_TEST_PROFILE_PROCESS") != "1" {
		t.Skip("set RELAIS_TEST_PROFILE_PROCESS=1 after make build; owns throwaway Redis, needs ffmpeg")
	}
	bin, err := filepath.Abs("../../bin")
	require.NoError(t, err)
	require.NoError(t, binaries(bin))
	manager := &clusterprocess.Manager{}
	ctx, cancel := manager.Context()
	defer cancel()
	dir := t.TempDir()
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
	for _, scenario := range []string{"move", "takeover"} {
		t.Run(scenario, func(t *testing.T) {
			options := profileTrialOptions{Name: "lan", Scenario: scenario}
			trialDir := filepath.Join(dir, scenario)
			require.NoError(t, os.Mkdir(trialDir, 0700))
			env := processEnv(strings.Repeat("00", 32), addr, fmt.Sprintf("relais:profile-probe:%s:%s:", filepath.Base(dir), scenario))
			trialCtx, stop := context.WithTimeout(ctx, 30*time.Second)
			defer stop()
			r, err := trial(trialCtx, manager, bin, trialDir, env, 5*time.Second, 5*time.Second, false, false, false, nil, &options)
			require.NoError(t, err)
			require.NotNil(t, r.ProfileRow)
			require.False(t, r.ProfileRow.Integrity.Failed, r.ProfileRow.Integrity.Reasons)
			require.False(t, r.ProfileRow.Integrity.Trusted, "short smoke cannot claim 60s consent")
			require.Equal(t, []string{"60s consent observation NOT-RUN"}, r.ProfileRow.Integrity.Reasons)
			require.GreaterOrEqual(t, r.ProfileRow.AudioBaseline.Samples, 10)
			require.GreaterOrEqual(t, r.ProfileRow.VideoBaseline.Samples, 10)
			loss := r.ProfileRow.Measurement.ContentLoss
			require.True(t, loss.Event.Audio.Trusted)
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
			require.Equal(t, "inconclusive", judgeProfile(options, []profileRow{*r.ProfileRow}, nil).Verdict)
		})
	}
}
