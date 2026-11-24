//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
)

const workerRestartDelay = 5 * time.Second

type workerAttempt struct {
	child *clusterprocess.Child
	err   error
}

// The supervisor observes only child exit. It never calls control or waits for
// takeover. A zero exit or supervisor cancellation stops it permanently.
func superviseWorker(ctx context.Context, manager *clusterprocess.Manager, dir string, env []string, delay time.Duration, args ...string) <-chan workerAttempt {
	attempts := make(chan workerAttempt)
	go func() {
		defer close(attempts)
		for generation := 1; ; generation++ {
			if ctx.Err() != nil {
				return
			}
			child, err := manager.Start(dir, fmt.Sprintf("supervised-worker-%d", generation), env, args...)
			select {
			case attempts <- workerAttempt{child, err}:
			case <-ctx.Done():
				if child != nil {
					child.Stop()
				}
				return
			}
			if err != nil {
				return
			}
			select {
			case <-ctx.Done():
				child.Stop()
				return
			case <-child.Done():
				child.Stop() // Close the reaped child's log.
			}
			if child.Err() == nil {
				return
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return attempts
}

func nextWorkerAttempt(ctx context.Context, attempts <-chan workerAttempt) (*clusterprocess.Child, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case attempt, ok := <-attempts:
		if !ok {
			return nil, errors.New("worker supervisor stopped")
		}
		return attempt.child, attempt.err
	}
}

func workerRestartDrill(ctx context.Context, manager *clusterprocess.Manager, bin, dir string, env []string, runs int, after time.Duration) error {
	topology, err := newPairTopology(manager, dir, env, "", 0, 0)
	if err != nil {
		return err
	}
	release := &pairRelease{Dir: bin, Version: "restart-drill"}
	if err := topology.startRelay(ctx, release); err != nil {
		return err
	}
	if topology.workers[1], err = topology.startWorker(ctx, release, 1); err != nil {
		return err
	}
	supervisorCtx, stop := context.WithCancel(ctx)
	attempts := superviseWorker(supervisorCtx, manager, dir, env, workerRestartDelay, topology.workerArgs(release, 0)...)
	defer func() {
		stop()
		for range attempts {
		}
	}()
	startup, cancelStartup := context.WithTimeout(ctx, 10*time.Second)
	defer cancelStartup()
	worker, err := nextWorkerAttempt(startup, attempts)
	if err != nil {
		return err
	}
	if _, err := worker.Ready(startup); err != nil {
		return err
	}
	if _, err := topology.startControl(ctx, release); err != nil {
		return err
	}
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{
		SignalingURL: "http://" + topology.controlAddr + "/calls",
		RelayAddr:    netip.MustParseAddrPort(topology.relayMedia),
	}})
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	for cycle := 1; cycle <= runs; cycle++ {
		worker, err = workerRestartCycle(ctx, topology, h, worker, attempts, cycle, after)
		if err != nil {
			return fmt.Errorf("worker restart cycle %d: %w", cycle, err)
		}
	}
	fmt.Printf("PASS: %d repeated worker crashes; automatic same-address restart, re-admission and new-call placement\n", runs)
	return nil
}

func workerRestartCycle(ctx context.Context, topology *pairTopology, h *callharness.Harness, worker *clusterprocess.Child, attempts <-chan workerAttempt, cycle int, after time.Duration) (*clusterprocess.Child, error) {
	call, err := h.Dial(ctx, callharness.CallOptions{Worker: 0})
	if err != nil {
		return nil, err
	}
	mediaCtx, stop := context.WithCancel(ctx)
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(mediaCtx, after+30*time.Second) }()
	joined := false
	defer func() {
		stop()
		if !joined {
			<-sent
		}
	}()
	if err := topology.pause(ctx, 700*time.Millisecond); err != nil {
		return nil, err
	}
	before, err := topology.controlStatus(ctx)
	if err != nil {
		return nil, err
	}
	killed := time.Now()
	if err := worker.SignalGroup(syscall.SIGKILL); err != nil {
		return nil, err
	}
	// This deadline is shorter than the independent supervisor's five-second
	// delay. Existing calls must recover while the crashed worker is still dead.
	settled, err := topology.waitControl(ctx, 3*time.Second, "takeover before restart", func(s controlplane.Status) bool {
		w := pairWorker(s, "0")
		return w != nil && w.Dead && !w.Recovering && len(s.Takeovers) == len(before.Takeovers)+1 && pairCallsOn(s, "1") == 1
	})
	if err != nil {
		return nil, err
	}
	event := settled.Takeovers[len(settled.Takeovers)-1]
	if event.Lost || event.Error != "" || settled.LostCount != 0 {
		return nil, fmt.Errorf("takeover failed: %+v", event)
	}
	wait, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	restarted, err := nextWorkerAttempt(wait, attempts)
	if err != nil {
		return nil, err
	}
	ready, err := restarted.Ready(wait)
	if err != nil {
		return nil, err
	}
	if ready.HTTP != "http://"+topology.workerHTTP[0] || ready.Media != topology.workerMedia[0] {
		return nil, errors.New("worker did not restart at its original addresses")
	}
	if _, err := topology.waitControl(wait, 8*time.Second, "automatic re-admission", func(s controlplane.Status) bool { return pairWorkerLive(s, "0", killed) }); err != nil {
		return nil, err
	}
	// Ordinary placement must select the empty returning worker. The survivor
	// still owns the original call; no drain or operator replace is sent.
	fresh, err := h.Dial(ctx, callharness.CallOptions{Worker: -1})
	if err != nil {
		return nil, err
	}
	status, err := topology.controlStatus(ctx)
	if err != nil {
		return nil, err
	}
	placed := false
	for _, c := range status.Calls {
		if c.ID == fresh.SessionID() && c.Owner == "0" {
			placed = true
		}
	}
	if !placed {
		return nil, errors.New("new call was not placed on the returning worker")
	}
	if err := fresh.SendMedia(ctx, after); err != nil {
		return nil, err
	}
	freshReport, err := fresh.Hangup(ctx)
	if err != nil {
		return nil, err
	}
	stop()
	mediaErr := <-sent
	joined = true
	if mediaErr != nil && !errors.Is(mediaErr, context.Canceled) {
		return nil, mediaErr
	}
	report, err := call.Hangup(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range []*callharness.Report{report, freshReport} {
		audio := r.Track("audio")
		if audio == nil || audio.Packets == 0 || !r.StartedAt.Add(audio.LastArrival).After(killed) || !r.ConnectedThroughout() || r.DecryptionFailures.Total() != 0 || r.ICERestarts != 0 {
			return nil, errors.New("original or new call did not carry clean audio after the crash")
		}
	}
	fmt.Printf("WORKER_RESTART cycle=%d old_pid=%d new_pid=%d takeover_owner=1 new_call_owner=0 original_audio=%d new_audio=%d\n", cycle, worker.PID(), restarted.PID(), report.Track("audio").Packets, freshReport.Track("audio").Packets)
	return restarted, nil
}
