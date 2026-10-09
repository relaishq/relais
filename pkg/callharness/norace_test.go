//go:build !race

package callharness_test

// raceDetector reports whether the tests run with the race detector (see
// race_test.go).
const raceDetector = false
