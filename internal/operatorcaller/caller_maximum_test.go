package operatorcaller

import (
	"testing"
	"time"
)

func TestCallerManifestAllowsThirtyThreeMinuteWindow(t *testing.T) {
	m := fixtureManifest()
	m.Plan.DurationSeconds = 1500
	m.Deadline = m.CreatedAt + int64(33*time.Minute/time.Millisecond)
	if err := m.Validate(m.Identity, time.UnixMilli(m.CreatedAt)); err != nil {
		t.Fatal("33-minute caller window refused", err)
	}
	m.Deadline++
	if err := m.Validate(m.Identity, time.UnixMilli(m.CreatedAt)); err == nil {
		t.Fatal("caller maximum exceeded")
	}
}

func TestCallerLauncherAllowsThirtyThreeMinuteWindow(t *testing.T) {
	c := fixtureLaunchConfig()
	now := time.Now()
	c.RuntimeDeadline = now.Add(33 * time.Minute)
	if _, err := StartScript(c, fixtureManifest().Identity, now.Add(time.Minute)); err != nil {
		t.Fatal("33-minute launcher refused", err)
	}
	c.RuntimeDeadline = now.Add(34 * time.Minute)
	if _, err := StartScript(c, fixtureManifest().Identity, now.Add(time.Minute)); err == nil {
		t.Fatal("launcher maximum exceeded")
	}
}

func TestCallerPlanDurationCeiling(t *testing.T) {
	for _, row := range []struct {
		seconds int
		valid   bool
	}{{1200, true}, {1500, true}, {1680, true}, {1681, false}} {
		plan := NewPlan(Survivor, row.seconds, fixtureManifest().PublicSHA)
		if (plan.Validate() == nil) != row.valid {
			t.Fatal("MUTATION: caller-plan-ceiling", row.seconds)
		}
	}
	if CallerMaximum != 33*time.Minute || CallerMaximum-1680*time.Second != 5*time.Minute {
		t.Fatal("MUTATION: caller-plan-ceiling: lifecycle reserve")
	}
}
