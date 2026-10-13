//go:build !darwin && !linux

package callharness

// The opt-in CPU measurement is available only on supported measurement hosts.
func processCPUSeconds() float64 { panic("CPU measurement requires darwin or linux") }
