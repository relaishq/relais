// Package processrss samples current resident memory of owned processes.
package processrss

import "fmt"

// Current returns current RSS in bytes, never a lifetime high-water mark.
func Current(pid int) (uint64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid owned PID")
	}
	return current(pid)
}
