// Package socketbind waits for fenced processes' socket references to drain.
package socketbind

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"
)

const DefaultTimeout = time.Second
const backoff = 5 * time.Millisecond

// Retry retries only EADDRINUSE within ctx's deadline. Nil ctx preserves a
// single bind attempt. The attempt must release any resources on error.
// Wait measures time from the first occupied-port result to completion.
func Retry[T io.Closer](ctx context.Context, attempt func() (T, error)) (value T, wait time.Duration, err error) {
	if ctx == nil {
		value, err = attempt()
		return value, 0, err
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return value, 0, errors.New("socket bind retry needs a deadline")
	}
	var waiting time.Time
	defer func() {
		if !waiting.IsZero() {
			wait = time.Since(waiting)
		}
	}()
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return value, 0, fmt.Errorf("socket bind stopped: %w", errors.Join(err, last))
		}
		bound, err := attempt()
		if err == nil {
			if err := ctx.Err(); err != nil {
				_ = bound.Close()
				return value, 0, err
			}
			return bound, 0, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return value, 0, err
		}
		last = err
		if waiting.IsZero() {
			waiting = time.Now()
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return value, 0, fmt.Errorf("socket bind stopped: %w", errors.Join(ctx.Err(), last))
		case <-timer.C:
		}
	}
}
