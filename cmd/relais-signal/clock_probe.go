package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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
			_, _ = fmt.Fprintf(os.Stderr, "relais-signal: clock probe recording stopped: %s\n", clockProbeWarning(err))
		}
	}()
	return func() {
		cancel()
		<-done
		_ = log.Close()
	}, nil
}

func clockProbeWarning(err error) string {
	// Run consumes HTTP failures as records. Its returned errors are ErrFull,
	// fixed record errors, short writes, or local file errors. Strip file paths;
	// also redact URL errors defensively at the injected runner boundary.
	var pathError *os.PathError
	if errors.As(err, &pathError) {
		err = pathError.Err
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return "HTTP request failed"
	}
	return err.Error()
}
