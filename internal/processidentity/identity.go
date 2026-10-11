// Package processidentity fences only the recorded same-host process.
package processidentity

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

var ErrExecutable = errors.New("fencing target is not the relay executable")
var ErrUnsupported = errors.New("standby process fencing requires Linux or macOS")

type Identity struct {
	PID   int
	Start string
}

func Current() (Identity, error) {
	start, _, err := inspect(os.Getpid())
	return Identity{PID: os.Getpid(), Start: start}, err
}

// Fence verifies start time immediately before SIGKILL and waits for exit.
// A zombie ends the identity wait; shared socket references may drain later.
// Callers must confirm exclusive binds before starting a new forwarder.
// A reused PID proves the recorded process exited and is never signalled.
func Fence(ctx context.Context, id Identity) error {
	return fencePlatform(ctx, id)
}
func fence(ctx context.Context, id Identity, read func(int) (string, bool, error), kill func(int) error) error {
	if id.PID <= 0 || id.Start == "" {
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
		return nil
	}
	if dead {
		return nil
	}
	if id.PID == os.Getpid() {
		return errors.New("refusing to fence the current process")
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
			return nil
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

func sameExecutable(target, self string) error {
	a, err := os.Stat(target)
	if err != nil {
		return err
	}
	b, err := os.Stat(self)
	if err != nil {
		return err
	}
	if !os.SameFile(a, b) {
		return ErrExecutable
	}
	return nil
}
