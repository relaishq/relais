//go:build (!linux && !darwin) || (darwin && !cgo)

package processrss

import "fmt"

func read(_ int) (Sample, error) {
	return Sample{}, fmt.Errorf("%w: needs Linux or macOS with cgo", ErrUnavailable)
}
