package main

import (
	"context"
	"fmt"
	"os"

	"github.com/relaishq/relais/internal/clockprobe"
)

// Keep the log open until every request has returned and recorded its result.
// A recording failure never stops the signaling server.
func startClockProber(ctx context.Context, c config, runner func(context.Context, clockprobe.Targets, *clockprobe.Log) error) (func(), error) {
	log, err := clockprobe.OpenLog(c.ClockProbeFile)
	if err != nil {
		return nil, fmt.Errorf("open clock probe file: %w", err)
	}
	probeCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := runner(probeCtx, c.ClockTargets, log); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "relais-signal: clock probe recording stopped")
		}
	}()
	return func() {
		cancel()
		<-done
		_ = log.Close()
	}, nil
}
