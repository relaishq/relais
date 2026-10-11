//go:build windows

package metrics

// Capacity resource sampling currently supports Linux and macOS only.
func processResources() []Sample { return nil }
