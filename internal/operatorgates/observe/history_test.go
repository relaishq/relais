package observe

import (
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
)

func TestHistoricalTakeoverStartLedger(t *testing.T) {
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	for _, kind := range []string{"move", "takeover"} {
		m := controlplane.MoveResult{ID: "one", From: "w1", To: "w2", Kind: kind, Start: now, End: now.Add(time.Second), Lost: true}
		raw := without(t, marshal(t, m), "start")
		got, err := HistoricalMove(raw)
		if kind == "takeover" {
			if err != nil || !got.Lost || !got.Start.IsZero() {
				t.Fatal("MUTATION: historical-start-ledger", got, err)
			}
		} else if err == nil {
			t.Fatal("planned move must have start")
		}
		if _, err := Move(raw); err == nil {
			t.Fatal("direct move receipt requires start")
		}
	}
}
