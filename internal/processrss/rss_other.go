//go:build (!linux && !darwin) || (darwin && !cgo)

package processrss

import "fmt"

func current(_ int) (uint64, error) {
	return 0, fmt.Errorf("current RSS needs Linux or macOS with cgo")
}
