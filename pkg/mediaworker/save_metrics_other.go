//go:build !linux

package mediaworker

// Do not present process-wide CPU deltas as per-Save CPU on other platforms.
func beginSaveCPU() func() (uint64, bool) { return func() (uint64, bool) { return 0, false } }
