package operator

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUncertaintyPublicBoundsHostEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, code, phase string
		elapsed, deadline int64
		want              bool
	}{
		{"valid", "timeout-error", "post", 5150, 6000, true},
		{"code-boundary", strings.Repeat("a", 40), "post", 5150, 6000, true},
		{"zero", "timeout-error", "status", 0, 6000, true},
		{"deadline", "timeout-error", "post", 6000, 6000, true},
		{"negative", "timeout-error", "post", -1, 6000, false},
		{"over-deadline", "timeout-error", "post", 6001, 6000, false},
		{"missing-deadline", "timeout-error", "post", 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"code": tc.code, "phase": tc.phase, "elapsedMs": tc.elapsed, "stepDeadlineMs": tc.deadline, "action": "move"})
			var u Uncertainty
			if err := json.Unmarshal(raw, &u); err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(u.Public())
			var got map[string]any
			_ = json.Unmarshal(data, &got)
			_, elapsed := got["elapsedMs"]
			if elapsed != tc.want || got["code"] != tc.code || got["phase"] != tc.phase {
				t.Fatalf("host evidence not bounded: %s", data)
			}
		})
	}
	for _, code := range []string{"Private address", "1bad", strings.Repeat("a", 41)} {
		raw, _ := json.Marshal(map[string]any{"code": code, "phase": "private phase", "action": "move"})
		var u Uncertainty
		_ = json.Unmarshal(raw, &u)
		data, _ := json.Marshal(u.Public())
		if strings.Contains(string(data), `"code"`) || strings.Contains(string(data), `"phase"`) {
			t.Fatalf("unsafe host evidence retained: %s", data)
		}
	}
	for _, raw := range []string{`{"action":"status","phase":"post"}`, `{"action":"move","phase":"private"}`} {
		var u Uncertainty
		_ = json.Unmarshal([]byte(raw), &u)
		data, _ := json.Marshal(u.Public())
		if strings.Contains(string(data), `"phase"`) {
			t.Fatal("phase escaped fixed move scope", raw)
		}
	}
}
