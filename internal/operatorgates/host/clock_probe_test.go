package host

import (
	"os/exec"
	"testing"
)

func TestClockProbeHostAsset(t *testing.T) {
	cmd := exec.Command("python3", "-B", "-m", "unittest", "clock_probe_test")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clock helper assertions: %s (%v)", out, err)
	}
	if _, err := Assets.ReadFile("clock_probe.py"); err != nil {
		t.Fatal(err)
	}
}
