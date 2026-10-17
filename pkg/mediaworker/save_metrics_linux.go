//go:build linux

package mediaworker

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// Linux thread CPU excludes other callbacks, packet readers and store I/O.
func beginSaveCPU() func() (uint64, bool) {
	runtime.LockOSThread()
	var before unix.Timespec
	first := unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &before)
	return func() (uint64, bool) {
		defer runtime.UnlockOSThread()
		var after unix.Timespec
		last := unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &after)
		if first != nil || last != nil || after.Nano() < before.Nano() {
			return 0, false
		}
		return uint64(after.Nano() - before.Nano()), true
	}
}
