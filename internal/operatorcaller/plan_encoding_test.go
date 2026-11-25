package operatorcaller

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExistingPlanEncodings(t *testing.T) {
	for _, row := range []struct {
		phase Phase
		want  string
	}{
		{Survivor, `{"version":1,"phase":"survivor","jobs":1,"calls":1,"workerCallCap":2,"durationSeconds":60,"publicSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`},
		{AdmissionBoundary, `{"version":1,"phase":"admission-boundary","jobs":1,"calls":2,"workerCallCap":2,"durationSeconds":60,"publicSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`},
	} {
		p := NewPlan(row.phase, 60, strings.Repeat("a", 40))
		b, err := json.Marshal(p)
		if err != nil || string(b) != row.want || p.Validate() != nil {
			t.Fatalf("plan encoding changed: %s %v", b, err)
		}
		p.Calls = 4
		if p.Validate() == nil {
			t.Fatal("existing plan admitted four calls")
		}
	}
}
