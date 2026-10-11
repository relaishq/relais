// Package processrss samples current memory of owned processes.
package processrss

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

var ErrUnavailable = errors.New("current memory sampling unavailable")

type Sample struct{ ResidentBytes, PhysicalFootprintBytes uint64 }

// Read retains resident size on every supported host. Darwin additionally
// reports physical footprint, which is the memory trend used by soak.
func Read(pid int) (Sample, error) {
	if pid <= 0 {
		return Sample{}, fmt.Errorf("invalid owned PID")
	}
	return read(pid)
}
func Current(pid int) (uint64, error) { s, err := Read(pid); return s.ResidentBytes, err }
func Check() error {
	sample, err := Read(os.Getpid())
	if err != nil {
		return err
	}
	value := sample.ResidentBytes
	if runtime.GOOS == "darwin" {
		value = sample.PhysicalFootprintBytes
	}
	if value == 0 {
		return fmt.Errorf("%w: zero process memory", ErrUnavailable)
	}
	return nil
}
func TrendMetric() string {
	if runtime.GOOS == "darwin" {
		return "physical-footprint"
	}
	return "rss"
}
