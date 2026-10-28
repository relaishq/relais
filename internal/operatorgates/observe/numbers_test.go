package observe

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates/oracletest"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
)

func TestEvidenceNumberBounds(t *testing.T) {
	for _, value := range []any{int64(-1), uint64(math.MaxUint64), math.Inf(1), math.NaN(), time.Duration(math.MaxInt64), []int64{MaxInteger + 1}, callharness.DurationInterval{Lower: 2, Upper: 1}} {
		if Numbers(value) == nil {
			t.Fatal("MUTATION: invalid-evidence-number", value)
		}
	}
	// Clock coordinates can be negative; opaque identities are not counters.
	for _, value := range []any{callharness.OffsetInterval{Lower: -10, Upper: 10}, callharness.ClockRateInterval{LowerPPM: -1, UpperPPM: 1}, callharness.EventMeasurement{FirstContentIdentity: math.MaxUint64}} {
		if err := Numbers(value); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := AddCount(MaxInteger, 1); ok {
		t.Fatal("MUTATION: count-overflow-guard")
	}
	if _, ok := AddCount(1, -1); ok {
		t.Fatal("negative counter accepted")
	}
	for _, pair := range [][2]int64{{math.MaxInt64, 1}, {math.MinInt64, -1}} {
		if _, ok := AddSigned(pair[0], pair[1]); ok {
			t.Fatal("MUTATION: signed-overflow-guard")
		}
	}
	values := make([]int64, 1024)
	for i := range values {
		values[i] = MaxInteger
	}
	if Numbers(values) == nil {
		t.Fatal("arithmetic budget overflow accepted")
	}
	at := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	if Numbers([]time.Time{at, at.Add(200 * 24 * time.Hour)}) == nil {
		t.Fatal("unbounded timestamp subtraction")
	}
	if Numbers(loadgen.Stats{CPUPerGOMAXPROCS: -1}) == nil {
		t.Fatal("negative CPU accepted")
	}
}

func TestDistributedMoveUnknownWorkerIndex(t *testing.T) {
	for _, field := range []string{"From", "To"} {
		for _, value := range []int{-2, -1, 0, 1, int(MaxInteger), int(MaxInteger + 1)} {
			t.Run(fmt.Sprintf("%s/%d", field, value), func(t *testing.T) {
				move := callharness.MoveReport{}
				reflect.ValueOf(&move).Elem().FieldByName(field).SetInt(int64(value))
				err := Numbers(loadgen.Message{Type: "call", Call: &callharness.Report{Moves: []callharness.MoveReport{move}}})
				want := value >= -1 && int64(value) <= MaxInteger
				if (err == nil) != want {
					t.Fatalf("MUTATION: worker-index-sentinel %s=%d: %v", field, value, err)
				}
			})
		}
	}
	if err := Numbers(callharness.MoveReport{From: -1, To: -1, Start: -1}); err == nil {
		t.Fatal("worker sentinel relaxed an unrelated duration")
	}
}

func TestNumericSentinelLedgerIsNarrowAndCopied(t *testing.T) {
	ledger := EvidenceNumericSentinels()
	if len(ledger) != 2 || ledger["callharness.MoveReport.From"] == "" || ledger["callharness.MoveReport.To"] == "" {
		t.Fatal(ledger)
	}
	delete(ledger, "callharness.MoveReport.From")
	if len(EvidenceNumericSentinels()) != 2 {
		t.Fatal("numeric sentinel ledger aliased")
	}
	// A surrogate with the same member names cannot inherit product exceptions.
	if Numbers(struct{ From, To int }{-1, -1}) == nil {
		t.Fatal("numeric sentinel escaped owning product type")
	}
}

func TestNumericRulesMatchFrozenTypedObservations(t *testing.T) {
	checked := 0
	for _, c := range oracletest.Typed(t) {
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(c.Result.Value, &envelope); err != nil {
			t.Fatal(err)
		}
		var value any
		switch c.Decoder {
		case "control":
			value = &controlplane.Status{}
		case "drain":
			value = &controlplane.DrainResult{}
		case "move":
			value = &controlplane.MoveResult{}
		default:
			continue
		}
		if err := json.Unmarshal(envelope.Data, value); err != nil {
			t.Fatal(err)
		}
		if err := Numbers(value); err != nil {
			t.Fatalf("frozen %s: %v", c.Decoder, err)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no frozen numeric observations")
	}
	t.Logf("checked %d frozen typed observations", checked)
}
