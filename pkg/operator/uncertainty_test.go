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

func TestUncertaintyPublicBoundsHTTPErrorEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		codes  []string
		want   string
	}{
		{"valid", 500, []string{"transient"}, `"httpStatus":500,"codes":["transient"]`},
		{"bounds", 100, []string{strings.Repeat("a", 40)}, `"httpStatus":100,"codes":["` + strings.Repeat("a", 40) + `"]`},
		{"top", 599, nil, `"httpStatus":599`},
		{"unsafe-dropped", 503, []string{"Private text", "1bad", strings.Repeat("a", 41), "", "lease-lost"}, `"httpStatus":503,"codes":["lease-lost"]`},
		{"at-most-four", 409, []string{"a", "b", "c", "d", "e"}, `"httpStatus":409,"codes":["a","b","c","d"]`},
		{"status-low", 99, []string{"transient"}, `"codes":["transient"]`},
		{"status-high", 600, nil, ``},
		{"status-negative", -500, []string{"Bad"}, ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"httpStatus": tc.status, "codes": tc.codes, "action": "drain"})
			var u Uncertainty
			if err := json.Unmarshal(raw, &u); err != nil {
				t.Fatal(err)
			}
			public := u.Public()
			data, _ := json.Marshal(public)
			if tc.want == "" {
				if strings.Contains(string(data), "httpStatus") || strings.Contains(string(data), "codes") {
					t.Fatalf("unsafe HTTP evidence retained: %s", data)
				}
				return
			}
			if !strings.Contains(string(data), tc.want) {
				t.Fatalf("HTTP evidence not bounded: %s, want %s", data, tc.want)
			}
			if len(u.Codes) > 0 && len(public.Codes) > 0 {
				public.Codes[0] = "changed"
				if u.Codes[0] == "changed" {
					t.Fatal("Public aliases the caller's codes")
				}
			}
		})
	}
}
