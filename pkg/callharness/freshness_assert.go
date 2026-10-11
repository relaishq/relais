package callharness

import "time"

// RequireFreshnessRecovered gates acceptance freshness, independently of loss.
// Phase-1 scenarios only report freshness; buffered takeovers in #27 can use
// this helper without the race detector. Measure the event with the same limit
// through CallOptions.Freshness and observe the full stability window first.
// Audio is required; video is required when the call expects video. Unknown
// or failed freshness cannot pass this gate.
func RequireFreshnessRecovered(t interface {
	Helper()
	Fatalf(string, ...any)
}, event EventMeasurement, limit time.Duration) {
	t.Helper()
	if limit <= 0 {
		t.Fatalf("freshness recovery limit must be positive: %s", limit)
		return
	}
	tracks := []struct {
		name   string
		report FreshnessReport
	}{{"audio", event.Audio}}
	if event.VideoExpected {
		tracks = append(tracks, struct {
			name   string
			report FreshnessReport
		}{"video", event.Video})
	}
	for _, track := range tracks {
		f := track.report
		if !f.Verdict.Trusted || f.Verdict.Failed {
			t.Fatalf("%s freshness: %s", track.name, f.Verdict.Summary())
			return
		}
		if f.RecoveryLimit != limit {
			t.Fatalf("%s freshness recovery limit measured at %s, acceptance requires %s", track.name, f.RecoveryLimit, limit)
			return
		}
		if !f.Recovered || f.BackToBaseline > limit {
			t.Fatalf("%s freshness recovery %s exceeds %s or was not observed", track.name, f.BackToBaseline, limit)
			return
		}
		if f.StabilityWindow <= 0 || f.StabilityObserved < f.StabilityWindow {
			t.Fatalf("%s freshness stability observed %s, need %s", track.name, f.StabilityObserved, f.StabilityWindow)
			return
		}
	}
}
