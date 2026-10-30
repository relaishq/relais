//go:build !windows

package main

import (
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/internal/redistestenv"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

func TestDialOutProcessFullConsent(t *testing.T) {
	for _, cacheOff := range []bool{false, true} {
		t.Run(map[bool]string{false: "cache+pli", true: "pli-cache-off"}[cacheOff], func(t *testing.T) {
			r := newModeProcessRig(t, false, false, cacheOff)
			c := r.caller(t, 90*time.Second)
			r.workerCounters(t)
			killed := time.Now()
			r.harness.RecordProcessKill("0", killed)
			require.NoError(t, r.topology.workers[0].Child.SignalGroup(syscall.SIGKILL))
			<-r.topology.workers[0].Child.Done()
			s, err := r.topology.waitControl(r.ctx, 2*time.Second, "worker takeover", func(s controlplane.Status) bool { return len(s.Takeovers) == 1 })
			require.NoError(t, err)
			takeover := time.Since(killed)
			require.Less(t, takeover, 2*time.Second)
			require.Zero(t, s.LostCount)
			select {
			case <-time.After(60 * time.Second):
			case <-r.ctx.Done():
				t.Fatal(r.ctx.Err())
			}
			report := r.cleanCall(t, c)
			measured := measure(report, r.topology.relayMedia, 60*time.Second)
			require.True(t, measured.Pass, "full native crash/consent gate: %+v\n%s", measured, report.Summary())
			if cacheOff {
				require.True(t, provesPLIRecovery(measured), "cache-off must prove PLI attribution: %+v", measured)
			}
			t.Logf("PROCESS_CONSENT cache_off=%t takeover=%s media_gap=%s consent=%s responses=%d false_takeovers=0 caller_decryption_failures=0 observed_worker_decryption_failures=0 logs=%s", cacheOff, takeover, measured.Gap, report.Consent.ObservedFor, report.Consent.ResponsesAfter, r.topology.dir)
		})
	}
}

func TestDialOutProcessCrashWithLinkDown(t *testing.T) {
	r := newDialProcessRig(t, false)
	c := r.caller(t, 20*time.Second)
	r.workerCounters(t)
	r.gate.fault(true)
	t.Cleanup(r.gate.restore)
	require.Eventually(t, func() bool {
		s, err := r.topology.controlStatus(r.ctx)
		return err == nil && len(s.Workers) == 2 && s.Workers[0].Link != nil && !s.Workers[0].Link.Connected
	}, 4*time.Second, 20*time.Millisecond)
	before, err := r.topology.controlStatus(r.ctx)
	require.NoError(t, err)
	require.Empty(t, before.Takeovers)
	require.Zero(t, before.LostCount)
	killed := time.Now()
	r.harness.RecordProcessKill("0", killed)
	require.NoError(t, r.topology.workers[0].Child.SignalGroup(syscall.SIGKILL))
	<-r.topology.workers[0].Child.Done()
	s, err := r.topology.waitControl(r.ctx, 2*time.Second, "worker takeover with link down", func(s controlplane.Status) bool { return len(s.Takeovers) == 1 })
	require.NoError(t, err)
	takeover := time.Since(killed)
	require.Less(t, takeover, 2*time.Second)
	require.Zero(t, s.LostCount)
	time.Sleep(3 * time.Second)
	report := r.cleanCall(t, c)
	measured := measure(report, r.topology.relayMedia, 3*time.Second)
	require.True(t, measured.Pass, "native link-down crash gate: %+v\n%s", measured, report.Summary())
	t.Logf("DIAL_OUT_LINK_DOWN_CRASH takeover=%s media_gap=%s false_takeovers=0 caller_decryption_failures=0 observed_worker_decryption_failures=0 logs=%s", takeover, measured.Gap, r.topology.dir)
}

func TestDialOutProcessRelayStandby(t *testing.T) {
	for _, pause := range []bool{false, true} {
		t.Run(strconv.FormatBool(pause), func(t *testing.T) {
			r := newDialProcessRig(t, false)
			c := r.caller(t, 20*time.Second)
			remote := &controlplane.RemoteRelay{URL: "http://" + r.topology.relayHTTP}
			before, err := remote.Status(r.ctx)
			require.NoError(t, err)
			require.NoError(t, r.topology.startStandby(r.ctx, r.release))
			old := r.topology.relay
			killed := time.Now()
			signal := syscall.SIGKILL
			if pause {
				signal = syscall.SIGSTOP
			}
			require.NoError(t, old.Child.SignalGroup(signal))
			require.NoError(t, r.topology.ready(r.ctx, r.topology.standby, 3*time.Second))
			readyAfter := time.Since(killed)
			select {
			case <-old.Child.Done():
			case <-time.After(100 * time.Millisecond):
				t.Fatal("standby ready before the old relay was reaped")
			}
			r.topology.relay = r.topology.standby
			var afterStatusError error
			require.Eventually(t, func() bool {
				s, e := remote.Status(r.ctx)
				afterStatusError = e
				return e == nil && s.Stats.Workers == 2
			}, time.Second, 10*time.Millisecond, "relay registration: %v", afterStatusError)
			registeredAfter := time.Since(killed)
			time.Sleep(2200 * time.Millisecond)
			after, err := remote.Status(r.ctx)
			require.NoError(t, err)
			s, err := r.topology.controlStatus(r.ctx)
			require.NoError(t, err)
			require.Empty(t, s.Takeovers)
			require.Zero(t, s.LostCount)
			until := time.Now()
			report := r.cleanCall(t, c)
			measured := measureRelayStandby(report, r.topology.relayMedia, killed, until, before, after, readyAfter, registeredAfter)
			require.True(t, measured.Pass, "native standby gate: %+v\n%s", measured, report.Summary())
			t.Logf("DIAL_OUT_STANDBY paused=%t gap=%s ready=%s registered=%s false_takeovers=0 caller_decryption_failures=0 worker_decryption_failures=0 logs=%s", pause, measured.Gap, readyAfter, registeredAfter, r.topology.dir)
		})
	}
}

// This test pauses the dedicated Redis itself. Run it alone, never concurrently
// with other tests using that endpoint; no process or budget is reconfigured.
func TestDialOutProcessStoreOutage(t *testing.T) {
	for _, idle := range []bool{true, false} {
		t.Run(strconv.FormatBool(idle), func(t *testing.T) {
			r := newDialProcessRig(t, false)
			var c *callharness.Call
			if !idle {
				c = r.caller(t, 50*time.Second)
			}
			before := r.ready(t)
			client := redis.NewClient(&redis.Options{Addr: redistestenv.Address(t), Protocol: 2, DisableIdentity: true, MaxRetries: -1})
			defer func() { require.NoError(t, client.Close()) }()
			require.NoError(t, client.Do(r.ctx, "CLIENT", "PAUSE", "30000", "ALL").Err())
			time.Sleep(30 * time.Second)
			after := r.ready(t)
			require.Empty(t, after.Takeovers)
			require.Zero(t, after.LostCount)
			if idle {
				c = r.caller(t, 10*time.Second)
			} else {
				require.Len(t, after.Calls, 1)
				require.Equal(t, before.Calls[0].Epoch, after.Calls[0].Epoch)
			}
			require.NoError(t, c.MoveExternal(r.ctx, 1))
			time.Sleep(700 * time.Millisecond)
			report := r.cleanCall(t, c)
			for _, track := range report.Tracks {
				require.Less(t, track.MediaGap, 100*time.Millisecond, "live store stall gate")
			}
			t.Logf("DIAL_OUT_STORE idle=%t outage=30s false_takeovers=0 lost_calls=0 caller_decryption_failures=0 worker_decryption_failures=0 logs=%s", idle, r.topology.dir)
		})
	}
}
