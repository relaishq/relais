package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/pkg/callharness"
)

// relayRestartTrial kills only the relay; the caller, control plane and workers
// stay alive. The replacement binds all three original addresses. Recovery is
// exclusively the production startup restore and control re-registration path.
func relayRestartTrial(ctx context.Context, manager *clusterprocess.Manager, bin, dir string, env []string, after, warmup time.Duration, verbose, restoreOff bool) (result, error) {
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
	if restoreOff {
		relayArgs = append(relayArgs, "-route-restore-off")
	}
	old, ready, err := start("relay", relayArgs...)
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
	killed := time.Now()
	if err := old.SignalGroup(syscall.SIGKILL); err != nil {
		return result{}, err
	}
	// Reap the old process before rebinding; two live forwarders are out of scope.
	select {
	case <-ctx.Done():
		return result{}, ctx.Err()
	case <-old.Done():
	}
	args := []string{filepath.Join(bin, "relais-relay"), "-media", ready.Media, "-leg", ready.Leg, "-http", strings.TrimPrefix(ready.HTTP, "http://")}
	if restoreOff {
		args = append(args, "-route-restore-off")
	}
	replacement, restarted, err := start("relay-restarted", args...)
	if err != nil {
		return result{}, err
	}
	if restarted.Media != ready.Media || restarted.Leg != ready.Leg || restarted.HTTP != ready.HTTP {
		return result{}, errors.New("restarted relay changed its addresses")
	}
	fmt.Printf("SIGKILL relay old_pid=%d new_pid=%d session=%s restore=%t public=%s restart_ms=%.1f\n", old.PID(), replacement.PID(), call.SessionID(), !restoreOff, ready.Media, float64(time.Since(killed))/float64(time.Millisecond))
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
	report, err := call.Hangup(ctx)
	if err != nil {
		return result{}, err
	}
	if verbose {
		fmt.Println(report.Summary())
	}
	return measureRelayRestart(report, ready.Media, killed, until, restoreOff), nil
}

func measureRelayRestart(report *callharness.Report, relayAddr string, killed, until time.Time, restoreOff bool) result {
	r := result{Decrypt: report.DecryptionFailures.Total(), Reconnects: report.ICERestarts, Renegotiations: report.Renegotiations}
	at := killed.Sub(report.StartedAt)
	flowing := len(report.Tracks) == 2
	for _, track := range report.Tracks {
		gap, resumed, ok := relayRestartGap(track.Arrivals, at)
		r.Gap = max(r.Gap, gap)
		flowing = flowing && ok && resumed > at && track.Packets > 0 && until.Sub(report.StartedAt.Add(track.LastArrival)) < 150*time.Millisecond
	}
	// The baseline is an observation, not the <1s persisted-route acceptance.
	limit := time.Second
	if restoreOff {
		limit = 6 * time.Second
	}
	r.Pass = flowing && r.Gap > 0 && r.Gap < limit && r.Decrypt == 0 && r.Reconnects == 0 && r.Renegotiations == 0 && report.OfferAnswerExchanges == 1 && report.ConnectedThroughout() && report.RemoteAddr == relayAddr && len(report.Moves) == 0 && report.Consent.ResponsesAfter > 0
	return r
}
func relayRestartGap(arrivals []time.Duration, at time.Duration) (gap, resumed time.Duration, ok bool) {
	for i := 1; i < len(arrivals); i++ {
		start := arrivals[i-1]
		if start < at-100*time.Millisecond || start > at+100*time.Millisecond {
			continue
		}
		if arrivals[i] > at && arrivals[i]-start > gap {
			gap, resumed, ok = arrivals[i]-start, arrivals[i], true
		}
	}
	return gap, resumed, ok
}
func printRelayRestartTable(label string, results []result) {
	fmt.Printf("RELAY_RESTART mode=%s\n", label)
	fmt.Println("run  gap_ms  reconnects  renegotiations  decryption_failures  result")
	for i, r := range results {
		state := "FAIL"
		if r.Pass {
			state = "PASS"
		}
		fmt.Printf("%3d  %6.1f  %10d  %14d  %19d  %s\n", i+1, float64(r.Gap)/float64(time.Millisecond), r.Reconnects, r.Renegotiations, r.Decrypt, state)
	}
}
