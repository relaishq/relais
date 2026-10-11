package loadgen

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// watchParent detects reparenting after owner death, without relying on
// whether a PID has been recycled or the parent remains a zombie.
func watchParent(ctx context.Context, parent int, every time.Duration, currentParent func() int) (context.Context, context.CancelFunc) {
	owned, cancel := context.WithCancelCause(ctx)
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			if currentParent() != parent {
				cancel(fmt.Errorf("caller parent %d exited", parent))
				return
			}
			select {
			case <-owned.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return owned, func() { cancel(context.Canceled) }
}

func waitForStart(ctx context.Context, start <-chan time.Time, deadline time.Duration) error {
	bounded, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	var at time.Time
	select {
	case at = <-start:
	case <-bounded.Done():
		return startError(bounded)
	}
	timer := time.NewTimer(max(time.Until(at), 0))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-bounded.Done():
		return startError(bounded)
	}
}

func startError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("caller start deadline exceeded")
	}
	return context.Cause(ctx)
}
