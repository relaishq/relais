package operator

import "testing"

func TestCallerStoppedPublicReason(t *testing.T) {
	for _, reason := range []string{"generator-saturation-hard-ceiling", "generator-saturation-sustained", "generator-saturation-budget", "invalid-generator-statistics", "caller-stream-failure", "caller-supervision-failure", "private-endpoint", "private endpoint or token"} {
		want := reason
		if reason == "private-endpoint" || reason == "private endpoint or token" {
			want = "unavailable"
		}
		row := (Uncertainty{Kind: "caller.collect", CallerID: "2", Rule: "caller-stopped", Code: reason}).Public()
		if row.Rule != "caller-stopped" || row.Code != want || row.CallerID != "2" {
			t.Fatalf("unsafe or missing stop reason: %+v", row)
		}
	}
}
