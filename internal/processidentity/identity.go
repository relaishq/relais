// Package processidentity fences only the recorded same-host process.
package processidentity

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

var ErrExecutable = errors.New("fencing target is not the relay executable")
var ErrUnsupported = errors.New("standby process fencing requires Linux or macOS")
var ErrForeignScope = errors.New("relay process host or PID namespace is unverifiable")

// UnknownHost permits plain relay startup, but never process verification.
const UnknownHost = "unknown"

type Identity struct {
	PID                         int
	Start                       string
	Host, Namespace, Executable string
}

func Current() (Identity, error) {
	return CurrentWithHostID("")
}

// CurrentWithHostID overrides platform discovery with an operator-supplied ID.
func CurrentWithHostID(hostID string) (Identity, error) {
	return currentWithScope(strings.TrimSpace(hostID), scopePlatform)
}

func currentWithScope(hostID string, scope func(string) (string, string, error)) (Identity, error) {
	start, _, err := inspect(os.Getpid())
	if err != nil {
		return Identity{}, err
	}
	host, namespace, err := scope(hostID)
	if err != nil {
		return Identity{}, err
	}
	exe, err := executableIdentity(os.Getpid())
	return Identity{PID: os.Getpid(), Start: start, Host: host, Namespace: namespace, Executable: exe}, err
}

// SameScope must run before interpreting a local PID or advancing a lease.
// A prior boot on the same machine has no surviving namespace or processes.
func SameScope(local, target Identity) error {
	if local.Host == "" || local.Host == UnknownHost || target.Host == UnknownHost || target.Host != local.Host || local.Namespace == "" || target.Namespace == "" {
		return ErrForeignScope
	}
	if local.Namespace != target.Namespace && !differentBoot(local.Start, target.Start) {
		return ErrForeignScope
	}
	return nil
}
func differentBoot(a, b string) bool {
	boot := func(start string) string {
		value, _, ok := strings.Cut(start, ":")
		if !ok || len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
			return ""
		}
		if _, err := hex.DecodeString(strings.ReplaceAll(value, "-", "")); err != nil {
			return ""
		}
		return strings.ToLower(value)
	}
	x, y := boot(a), boot(b)
	return x != "" && y != "" && x != y
}
func verifyRecordedExecutable(pid int, recorded string) error {
	if recorded == "" {
		return ErrExecutable
	}
	actual, err := executableIdentity(pid)
	if err != nil {
		return err
	}
	if actual != recorded {
		return ErrExecutable
	}
	return nil
}

// Fence verifies start time immediately before SIGKILL and waits for exit.
// A zombie ends the identity wait; shared socket references may drain later.
// Callers must confirm exclusive binds before starting a new forwarder.
// A reused PID proves the recorded process exited and is never signalled.
func Fence(ctx context.Context, id Identity) error {
	local, err := Current()
	if err != nil {
		return err
	}
	return FenceFrom(ctx, local, id)
}

// FenceFrom uses the local identity captured with the configured host ID.
func FenceFrom(ctx context.Context, local, id Identity) error {
	if err := SameScope(local, id); err != nil {
		return err
	}
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

// A missing/hidden /proc entry does not prove exit while the pinned process
// remains live. Keep this seam shared with production Linux reads for testing.
func inspectPinnedWith(pid int, read func(int) (string, bool, error), check func() error, signalZero func() error) (string, bool, error) {
	if err := check(); err != nil {
		return "", false, err
	}
	start, dead, readErr := read(pid)
	if err := check(); err != nil {
		return "", false, err
	}
	if readErr != nil {
		if err := signalZero(); errors.Is(err, syscall.ESRCH) {
			return "", false, syscall.ESRCH
		} else if err != nil {
			return "", false, fmt.Errorf("cannot confirm pinned process exit: %v (identity read: %v)", err, readErr)
		}
		return "", false, fmt.Errorf("pinned process is live but its identity is unreadable: %v", readErr)
	}
	return start, dead, nil
}

// Gone verifies that the recorded process has exited, without signalling it.
// A live or stopped matching identity returns false. Unknown states are errors,
// never evidence of exit. Socket exclusivity remains required after a zombie.
func Gone(ctx context.Context, id Identity) (bool, error) {
	local, err := Current()
	if err != nil {
		return false, err
	}
	return GoneFrom(ctx, local, id)
}

// GoneFrom uses the local identity captured with the configured host ID.
func GoneFrom(ctx context.Context, local, id Identity) (bool, error) {
	if err := SameScope(local, id); err != nil {
		return false, err
	}
	return gonePlatform(ctx, id)
}

// Shared with Linux production discovery so missing machine IDs can be tested
// without changing the host filesystem. An override does not read these files.
func linuxHost(hostID string, read func(string) ([]byte, error)) string {
	if hostID != "" {
		return "host-id:" + hostID
	}
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		data, err := read(path)
		if err == nil {
			if machine := strings.TrimSpace(string(data)); machine != "" {
				return "linux:" + machine
			}
		}
	}
	return UnknownHost
}
func gone(ctx context.Context, id Identity, read func(int) (string, bool, error), executable func(int) error) (bool, error) {
	if id.PID <= 0 || id.Start == "" {
		return false, errors.New("invalid process identity")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	check := func() (bool, error) {
		start, dead, err := read(id.PID)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return start != id.Start || dead, nil
	}
	exited, err := check()
	if err != nil || exited {
		return exited, err
	}
	if err := executable(id.PID); err != nil {
		return false, err
	}
	// Recheck after the executable lookup. Absence, mismatch and zombies do
	// not need executable access, because the recorded process is already gone.
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return check()
}
