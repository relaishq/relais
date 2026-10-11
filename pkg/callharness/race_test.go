//go:build race

package callharness_test

// raceDetector reports whether the tests run with the race detector, which
// slows the workers' cryptography several times over. Timing assertions that
// load many calls at once loosen under it; CI enforces the real thresholds in
// a separate run without it (make test-harness-timing). Freshness is reported
// but not gated in phase-1 scenarios with or without race. Content verdicts
// and coverage remain gated. Buffered-takeover acceptance (#27) uses a strict
// 2s gate without race; measurement defaults retain 2s.
const raceDetector = true
