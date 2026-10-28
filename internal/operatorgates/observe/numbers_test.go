package observe

import (
	"math"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
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
