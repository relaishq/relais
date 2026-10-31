package operatorcaller

import (
	"testing"
	"time"
)

func TestCallerManifestAllowsTwentyTwoMinuteWindow(t *testing.T) {
	m := fixtureManifest()
	m.Plan.DurationSeconds = 1020
	m.Deadline = m.CreatedAt + int64(22*time.Minute/time.Millisecond)
	if err := m.Validate(m.Identity, time.UnixMilli(m.CreatedAt)); err != nil {
		t.Fatal("22-minute caller window refused", err)
	}
	m.Deadline++
	if err := m.Validate(m.Identity, time.UnixMilli(m.CreatedAt)); err == nil {
		t.Fatal("caller maximum exceeded")
	}
}

func TestCallerLauncherAllowsTwentyTwoMinuteWindow(t *testing.T) {
	c := fixtureLaunchConfig()
	now := time.Now()
	c.RuntimeDeadline = now.Add(22 * time.Minute)
	if _, err := StartScript(c, fixtureManifest().Identity, now.Add(time.Minute)); err != nil {
		t.Fatal("22-minute launcher refused", err)
	}
	c.RuntimeDeadline = now.Add(23 * time.Minute)
	if _, err := StartScript(c, fixtureManifest().Identity, now.Add(time.Minute)); err == nil {
		t.Fatal("launcher maximum exceeded")
	}
}
