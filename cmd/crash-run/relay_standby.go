//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/relay"
)

// relayStandbyTrial leaves callers, workers and control alive while an already
// waiting standby fences the old relay and takes over the same socket addresses.
func relayStandbyTrial(ctx context.Context, manager *clusterprocess.Manager, bin, dir string, env []string, after, warmup time.Duration, verbose, pause bool) (result, error) {
	var processes []*clusterprocess.Child
	defer func() {
		for i := len(processes) - 1; i >= 0; i-- {
			processes[i].Stop()
		}
	}()
	start := func(name string, args ...string) (*clusterprocess.Child, clusterprocess.Ready, error) {
		childEnv := env
		if strings.HasPrefix(name, "relay") {
			childEnv = withoutKey(env)
		}
		c, err := manager.Start(dir, name, childEnv, args...)
		if err != nil {
			return nil, clusterprocess.Ready{}, err
		}
		processes = append(processes, c)
		startup, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ready, err := c.Ready(startup)
		return c, ready, err
	}
	relayArgs := []string{filepath.Join(bin, "relais-relay"), "-media", "127.0.0.1:0", "-leg", "127.0.0.1:0", "-http", "127.0.0.1:0"}
	old, ready, err := start("relay", relayArgs...)
	if err != nil {
		return result{}, err
	}

	standbyArgs := []string{filepath.Join(bin, "relais-relay"), "-standby", "-media", ready.Media, "-leg", ready.Leg, "-http", strings.TrimPrefix(ready.HTTP, "http://")}
	standby, err := manager.Start(dir, "relay-standby", withoutKey(env), standbyArgs...)
	if err != nil {
		return result{}, err
	}
	processes = append(processes, standby)
	waiting, stopWaiting := context.WithTimeout(ctx, 3*time.Second)
	err = standby.WaitLog(waiting, "STANDBY_WAIT")
	stopWaiting()
	if err != nil {
		return result{}, err
	}
	controlAddr, err := clusterprocess.FreeTCP()
	if err != nil {
		return result{}, err
	}
	controlURL := "http://" + controlAddr
	controlArgs := []string{filepath.Join(bin, "relais-control"), "-http", controlAddr, "-relay", ready.HTTP}
	for _, name := range []string{"0", "1"} {
		_, worker, err := start("worker-"+name, filepath.Join(bin, "relais-worker"), "-media", "127.0.0.1:0", "-http", "127.0.0.1:0", "-name", name, "-control", controlURL, "-relay-leg", ready.Leg, "-relay-media", ready.Media)
		if err != nil {
			return result{}, err
		}
		controlArgs = append(controlArgs, "-worker", name+"="+worker.HTTP)
	}
	if _, _, err := start("control", controlArgs...); err != nil {
		return result{}, err
	}
	addr, err := netip.ParseAddrPort(ready.Media)
	if err != nil {
		return result{}, err
	}
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: controlURL + "/calls", RelayAddr: addr}})
	if err != nil {
		return result{}, err
	}
	defer func() { _ = h.Close() }()
	call, err := h.Dial(ctx, callharness.CallOptions{Video: true, Worker: -1})
	if err != nil {
		return result{}, err
	}
	mediaCtx, stopMedia := context.WithCancel(ctx)
	defer stopMedia()
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(mediaCtx, after+15*time.Second) }()
	joined := false
	defer func() {
		stopMedia()
		if !joined {
			<-sent
		}
	}()
	select {
	case <-ctx.Done():
		return result{}, ctx.Err()
	case err := <-sent:
		joined = true
		return result{}, fmt.Errorf("media ended before relay SIGKILL: %v", err)
	case <-time.After(warmup):
	}

	remote := &controlplane.RemoteRelay{URL: ready.HTTP}
	oldStatus, err := remote.Status(ctx)
	if err != nil {
		return result{}, err
	}
	killed := time.Now()
	signal, label := syscall.SIGKILL, "SIGKILL"
	if pause {
		signal, label = syscall.SIGSTOP, "SIGSTOP"
	}
	if err := old.SignalGroup(signal); err != nil {
		return result{}, err
	}
	startup, stopStartup := context.WithTimeout(ctx, 3*time.Second)
	restarted, err := standby.Ready(startup)
	stopStartup()
	if err != nil {
		return result{}, err
	}
	readyAfter := time.Since(killed)
	if restarted.Media != ready.Media || restarted.Leg != ready.Leg || restarted.HTTP != ready.HTTP {
		return result{}, errors.New("standby changed socket addresses")
	}
	// Ready must imply that the old process is already dead, including SIGSTOP.
	select {
	case <-old.Done():
	case <-time.After(100 * time.Millisecond):
		return result{}, errors.New("standby ready while old process still exists")
	}
	if pause {
		err := old.SignalGroup(syscall.SIGCONT)
		if !errors.Is(err, os.ErrProcessDone) {
			return result{}, fmt.Errorf("SIGCONT old process was not reaped: %v", err)
		}
		fmt.Printf("SIGCONT old_pid=%d old_reaped=true resurrection=false\n", old.PID())
	}
	var status relay.RelayStatus
	registerDeadline := time.NewTimer(time.Second)
	defer registerDeadline.Stop()
	registerTick := time.NewTicker(10 * time.Millisecond)
	defer registerTick.Stop()
	for {
		status, err = remote.Status(ctx)
		if err != nil {
			return result{}, err
		}
		if status.Stats.Workers == 2 {
			break
		}
		select {
		case <-ctx.Done():
			return result{}, ctx.Err()
		case <-registerDeadline.C:
			return result{}, errors.New("standby workers did not re-register")
		case <-registerTick.C:
		}
	}
	registeredAfter := time.Since(killed)
	fmt.Printf("%s relay old_pid=%d new_pid=%d session=%s public=%s ready_ms=%.1f registered_ms=%.1f old_worker_packets=%d\n", label, old.PID(), standby.PID(), call.SessionID(), ready.Media, float64(readyAfter)/float64(time.Millisecond), float64(registeredAfter)/float64(time.Millisecond), oldStatus.Stats.WorkerPackets)
	select {
	case <-ctx.Done():
		return result{}, ctx.Err()
	case err := <-sent:
		joined = true
		return result{}, fmt.Errorf("media ended before restart observation finished: %v", err)
	case <-time.After(after):
	}
	until := time.Now()
	stopMedia()
	mediaErr := <-sent
	joined = true
	if mediaErr != nil && !errors.Is(mediaErr, context.Canceled) {
		return result{}, mediaErr
	}
	status, err = remote.Status(ctx)
	if err != nil {
		return result{}, err
	}
	report, err := call.Hangup(ctx)
	if err != nil {
		return result{}, err
	}
	if verbose {
		fmt.Println(report.Summary())
	}
	return measureRelayStandby(report, ready.Media, killed, until, oldStatus, status, readyAfter, registeredAfter), nil
}

func measureRelayStandby(report *callharness.Report, public string, killed, until time.Time, oldStatus, status relay.RelayStatus, readyAfter, registeredAfter time.Duration) result {
	r := measureRelayRestart(report, public, killed, until, false, status.Stats.RoutesRestored)
	for _, track := range report.Tracks {
		r.Duplicates += track.DuplicatePackets
	}
	r.OldWorkerPackets = oldStatus.Stats.WorkerPackets
	r.NewWorkerPackets = status.Stats.WorkerPackets
	r.ReadyAfter = readyAfter
	r.RegisteredAfter = registeredAfter
	if status.Continuity == nil || oldStatus.Continuity == nil {
		r.Pass = false
		return r
	}
	r.ClaimAfter = status.Continuity.ClaimAt.Sub(killed)
	r.FenceTime = status.Continuity.Fence
	r.ActivateTime = status.Continuity.Activate
	r.BindRestoreTime = status.Continuity.BindRestore
	r.BindWaitTime = status.Continuity.BindWait
	r.Pass = r.Pass && r.Duplicates == 0 && r.OldWorkerPackets > 0 && r.NewWorkerPackets > 0 && status.Instance != oldStatus.Instance && status.Continuity.Epoch > oldStatus.Continuity.Epoch && status.Stats.SelfFences == 0 && r.ClaimAfter >= 0
	return r
}

func printRelayStandbyTable(label string, results []result) {
	fmt.Printf("RELAY_STANDBY mode=%s old_process_dead_before_new_ready=true\n", label)
	fmt.Println("run gap_ms reconnects decryption_failures duplicate_media routes_restored claim_ms fence_ms activate_ms bind_wait_ms bind_restore_ms ready_ms registered_ms old_worker_packets new_worker_packets result")
	for i, r := range results {
		state := "FAIL"
		if r.Pass {
			state = "PASS"
		}
		ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
		fmt.Printf("%d %.1f %d %d %d %d %.1f %.1f %.1f %.1f %.1f %.1f %.1f %d %d %s\n", i+1, ms(r.Gap), r.Reconnects, r.Decrypt, r.Duplicates, r.RoutesRestored, ms(r.ClaimAfter), ms(r.FenceTime), ms(r.ActivateTime), ms(r.BindWaitTime), ms(r.BindRestoreTime), ms(r.ReadyAfter), ms(r.RegisteredAfter), r.OldWorkerPackets, r.NewWorkerPackets, state)
	}
}
