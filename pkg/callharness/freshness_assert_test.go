package callharness

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Exercise the actual reusable assertion in a child test process, so a
// rejected synthetic timeline must fail the test rather than just set a flag.
type misnamedAcceptanceT struct{ *testing.T }

func (misnamedAcceptanceT) Name() string { return "TestBufferedTakeover" }

func TestAcceptanceFreshnessGate(t *testing.T) {
	const probe = "RELAIS_FRESHNESS_GATE_PROBE"
	if scenario := os.Getenv(probe); scenario != "" {
		units := attackBaseline()
		for i := range 250 {
			sent := 1400*time.Millisecond + time.Duration(i)*20*time.Millisecond
			latency := time.Millisecond
			if scenario == "interleaved" && i%3 != 0 {
				latency = 100 * time.Millisecond
			}
			units = append(units, contentUnit{at: sent, returnedAt: sent + latency, identity: fmt.Sprint("live", i)})
		}
		limit := 60 * time.Second
		if scenario == "truncated" {
			limit = 3050 * time.Millisecond
		}
		if scenario == "absolute-lag" {
			for i := range units {
				units[i].returnedAt = units[i].at + 300*time.Millisecond
			}
		}
		if scenario == "late-recovery" {
			units = append(units, contentUnit{at: 1100 * time.Millisecond, returnedAt: 3400 * time.Millisecond, identity: "outage"})
		}
		required := StrictAcceptancePolicy()
		freshness := measureFreshness(units, time.Second, 1400*time.Millisecond, limit, 0)
		event := EventMeasurement{Audio: freshness}
		switch scenario {
		case "weak-stability":
			units = append(units, contentUnit{at: 1100 * time.Millisecond, returnedAt: 3100 * time.Millisecond, identity: "hidden-tail"})
			event.Audio = measureFreshness(units, time.Second, 1400*time.Millisecond, limit, 0, FreshnessPolicy{StabilityWindow: 50 * time.Millisecond})
		case "weak-ceiling":
			for i := range units {
				units[i].returnedAt = units[i].at + 300*time.Millisecond
			}
			event.Audio = measureFreshness(units, time.Second, 1400*time.Millisecond, limit, 0, FreshnessPolicy{AbsoluteLatencyCeiling: time.Second})
		case "weak-lateness-cap":
			event.Audio = measureFreshness(units, time.Second, 1400*time.Millisecond, limit, 0, FreshnessPolicy{LiveLatenessCap: time.Second})
		case "network-profile":
			for i := range units {
				units[i].returnedAt = units[i].at + 300*time.Millisecond
			}
			required.AbsoluteLatencyCeiling = 400 * time.Millisecond
			event.Audio = measureFreshness(units, time.Second, 1400*time.Millisecond, limit, 0, required)
		case "stronger-policy":
			event.Audio = measureFreshness(units, time.Second, 1400*time.Millisecond, limit, 0, FreshnessPolicy{StabilityWindow: 4 * time.Second, AbsoluteLatencyCeiling: 40 * time.Millisecond})
		case "video-conflict":
			r := testMeasurementRecorder(t)
			r.contentIdentityErrors = 1
			event = r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
		case "missing-video":
			event.VideoExpected = true
		case "policy-mismatch":
			event.Audio = measureFreshness(units, time.Second, 1400*time.Millisecond, limit, 0, FreshnessPolicy{RecoveryLimit: 5 * time.Second})
		case "loss-only":
			event.AudioLoss = MetricVerdict{Trusted: true, Failed: true}
			event.LostAudio = 120 * time.Millisecond
		}
		if scenario == "misnamed" {
			RequireFreshnessRecovered(misnamedAcceptanceT{t}, event, required)
			return
		}
		RequireFreshnessRecovered(t, event, required)
		return
	}
	for _, scenario := range []struct {
		name, reason string
	}{
		{"recovered", ""},
		{"network-profile", ""},
		{"stronger-policy", ""},
		{"misnamed", "must be named TestAcceptance"},
		{"loss-only", ""},
		{"interleaved", "freshness"},
		{"truncated", "freshness"},
		{"absolute-lag", "AbsoluteLag"},
		{"late-recovery", "freshness"},
		{"video-conflict", "conflicting video content identities"},
		{"missing-video", "video freshness"},
		{"policy-mismatch", "recovery limit"},
		{"weak-stability", "stability window"},
		{"weak-ceiling", "absolute ceiling"},
		{"weak-lateness-cap", "live lateness cap"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestAcceptanceFreshnessGate$")
			cmd.Env = append(os.Environ(), probe+"="+scenario.name)
			output, err := cmd.CombinedOutput()
			if scenario.reason == "" {
				require.NoError(t, err, "%s", output)
			} else {
				require.Error(t, err, "freshness gate accepted %s: %s", scenario.name, output)
				require.Contains(t, string(output), scenario.reason)
			}
		})
	}
}
