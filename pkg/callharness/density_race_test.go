//go:build race

package callharness

// Race checks the lock handoff, not native copying performance.
const densityRaceDetector = true
