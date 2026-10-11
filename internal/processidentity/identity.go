// Package processidentity fences only the recorded same-host process.
package processidentity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

var ErrMismatch = errors.New("process identity does not match")

type Identity struct {
	PID   int
	Start string
}

func Current() (Identity, error) {
	start, _, err := inspect(os.Getpid())
	return Identity{PID: os.Getpid(), Start: start}, err
}

// Fence verifies start time immediately before SIGKILL and waits for exit.
// Zombies are gone for this purpose: they cannot forward or own sockets.
// A reused PID is never signalled and prevents activation.
func Fence(ctx context.Context, id Identity) error {
	return fence(ctx, id, inspect, func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) })
}
func fence(ctx context.Context, id Identity, read func(int) (string, bool, error), kill func(int) error) error {
	if id.PID <= 1 || id.Start == "" || id.PID == os.Getpid() {
		return errors.New("invalid fencing target")
	}
	start, dead, err := read(id.PID)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	if start != id.Start {
		return fmt.Errorf("%w: pid %d", ErrMismatch, id.PID)
	}
	if dead {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := kill(id.PID); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		start, dead, err = read(id.PID)
		// Linux can report ESRCH if the parent reaps during a /proc read.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return err
		}
		if start != id.Start {
			return fmt.Errorf("%w after fencing: pid %d", ErrMismatch, id.PID)
		}
		if dead {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
