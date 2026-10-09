//go:build race

package callharness_test

// raceDetector reports whether the tests run with the race detector, which
// slows the workers' cryptography several times over. Timing assertions that
// load many calls at once loosen under it; CI enforces the real thresholds in
// a separate run without it (make test-harness-timing).
const raceDetector = true
