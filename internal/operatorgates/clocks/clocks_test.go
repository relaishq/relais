package clocks

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/oracletest"
)

func TestSignalBracketOracle(t *testing.T) {
	for i, c := range oracletest.Cases(t, "clocks.verifySignalBracket") {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			var data struct {
				Before string `json:"signalBeforeUnixNs"`
				After  string `json:"signalAfterUnixNs"`
			}
			_ = json.Unmarshal(c.Request.Inputs[0], &data)
			got, err := SignalMidpoint(data.Before, data.After, oracletest.JSON[int64](t, c.Request.Inputs[1]), oracletest.JSON[int64](t, c.Request.Inputs[2]))
			if (err == nil) != (c.Result.Completion == "returned") {
				t.Fatalf("ignore-signal-bracket-width: oracle verdict %v", err)
			}
			if err == nil && got != oracletest.JSON[string](t, c.Result.Value) {
				t.Fatalf("signal midpoint differs: %s", got)
			}
		})
	}
}
