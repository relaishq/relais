package callharness

import (
	"strings"
	"time"
)

// RequireFreshnessRecovered gates acceptance freshness, independently of loss.
// Phase-1 scenarios only report freshness; buffered takeovers in #27 can use
// this helper without the race detector. Acceptance tests must be named
// TestAcceptance… and must not skip when run without race; the timing CI
// target includes that prefix. Measure using CallOptions.Freshness with the
// required recovery limit, at least its stability window, and at most its
// absolute ceiling and live lateness cap. Observe the full measured stability
// window first. Use
// StrictAcceptancePolicy for in-process/LAN tests; #37 network profiles must
// pass their required absolute ceiling explicitly.
// Audio is required; video is required when the call expects video. Unknown
// or failed freshness cannot pass this gate.
func RequireFreshnessRecovered(t interface {
	Helper()
	Name() string
	Fatalf(string, ...any)
}, event EventMeasurement, required FreshnessPolicy) {
	t.Helper()
	if !strings.HasPrefix(t.Name(), "TestAcceptance") {
		t.Fatalf("freshness acceptance tests must be named TestAcceptance…: %s", t.Name())
		return
	}
	required = required.defaults()
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
		if f.RecoveryLimit != required.RecoveryLimit {
			t.Fatalf("%s freshness recovery limit measured at %s, acceptance requires %s", track.name, f.RecoveryLimit, required.RecoveryLimit)
			return
		}
		if f.StabilityWindow < required.StabilityWindow {
			t.Fatalf("%s freshness stability window measured at %s, acceptance requires at least %s", track.name, f.StabilityWindow, required.StabilityWindow)
			return
		}
		if f.AbsoluteLatencyCeiling <= 0 || f.AbsoluteLatencyCeiling > required.AbsoluteLatencyCeiling {
			t.Fatalf("%s freshness absolute ceiling measured at %s, acceptance requires at most %s", track.name, f.AbsoluteLatencyCeiling, required.AbsoluteLatencyCeiling)
			return
		}
		if f.LiveLatenessCap <= 0 || f.LiveLatenessCap > required.LiveLatenessCap {
			t.Fatalf("%s freshness live lateness cap measured at %s, acceptance requires at most %s", track.name, f.LiveLatenessCap, required.LiveLatenessCap)
			return
		}
		if !f.Recovered || f.BackToBaseline > required.RecoveryLimit {
			t.Fatalf("%s freshness recovery %s exceeds %s or was not observed", track.name, f.BackToBaseline, required.RecoveryLimit)
			return
		}
		if f.StabilityWindow <= 0 || f.StabilityObserved < f.StabilityWindow {
			t.Fatalf("%s freshness stability observed %s, need %s", track.name, f.StabilityObserved, f.StabilityWindow)
			return
		}
	}
}

// StrictAcceptancePolicy is the in-process/LAN buffered-takeover acceptance
// policy. Network profiles can set their required absolute ceiling explicitly.
func StrictAcceptancePolicy() FreshnessPolicy {
	return FreshnessPolicy{
		RecoveryLimit:          2 * time.Second,
		StabilityWindow:        3 * time.Second,
		AbsoluteLatencyCeiling: 50 * time.Millisecond,
		LiveLatenessCap:        200 * time.Millisecond,
	}
}
