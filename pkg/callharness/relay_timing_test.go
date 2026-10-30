package callharness

import "time"

// Race instrumentation checks gross regressions. The timing and smoke targets
// enforce the real policy without race; all media integrity checks stay strict.
func relayTimingLimit(limit time.Duration) time.Duration {
	if raceDetector {
		return 500 * time.Millisecond
	}
	return limit
}
