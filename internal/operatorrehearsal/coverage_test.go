package operatorrehearsal

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

func TestCoverageSanitizationPreservesClockRelationships(t *testing.T) {
	base := time.Unix(1793930000, 250000000).UTC()
	e := clocks.CoverageEvidence{Version: 2, FrontendSlot: "shared", ControlSlot: "shared", Bounds: clocks.Bounds{FrontendControlBoundNS: 2000000, FrontendWorkerBoundNS: 3000000}, Windows: []observe.CoverageWindow{{Kind: "move", Worker: "w1", StartUnixNS: strconv.FormatInt(base.Add(10*time.Second).UnixNano(), 10), EndUnixNS: strconv.FormatInt(base.Add(20*time.Second).UnixNano(), 10)}}}
	sample := clocks.Sample{Tracking: "private-label", MeasuredUnixNS: strconv.FormatInt(base.UnixNano(), 10), TrackingLog: &clocks.TrackingLog{ReadComplete: true, RunStartUnixMS: base.UnixMilli(), Rows: []clocks.TrackingRow{{UnixSeconds: 1793930000, LeapStatus: "N", MaxErrorSeconds: .0001, Reference: "private-label"}, {UnixSeconds: 1793930030, LeapStatus: "N", MaxErrorSeconds: .0001}}}}
	e.Attempts = []clocks.CoverageAttempt{clocks.NewCoverageAttempt(clocks.Evidence{Version: 2, At: base, Clocks: map[string]clocks.Sample{"shared": sample}})}
	e.Evaluate()
	pool := operatorevidence.Pool{ClockCoverage: &e, Phases: []operatorevidence.MediaInput{{RawMedia: operatorevidence.RawMedia{CallerJSONL: `{"type":"stats","stats":{"at":"` + base.Format(time.RFC3339Nano) + `"}}` + "\n"}}}}
	bare := pool
	bare.ClockCoverage = nil
	without, err := SanitizeMediaEvidence(bare)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := SanitizeMediaEvidence(pool)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-label") || strings.Contains(string(raw), base.Format(time.RFC3339Nano)) {
		t.Fatal("private data retained", string(raw))
	}
	position := bytes.LastIndex(raw, []byte(`,"clockCoverage":`))
	if position < 0 || !bytes.Equal(append(append([]byte{}, raw[:position]...), '}'), without) {
		t.Fatal("existing artifact fields changed bytes when coverage was added")
	}
	var got operatorevidence.Pool
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	got.ClockCoverage.Evaluate()
	if !reflect.DeepEqual(e.Results, got.ClockCoverage.Results) {
		t.Fatal("coverage verdict changed after sanitizing", e.Results, got.ClockCoverage.Results)
	}
	var message struct {
		Stats struct {
			At time.Time `json:"at"`
		} `json:"stats"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(got.Phases[0].CallerJSONL)), &message); err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.ParseInt(got.ClockCoverage.Windows[0].StartUnixNS, 10, 64)
	if time.Unix(0, n).Add(-time.Duration(got.ClockCoverage.MediaTimeOffsetNS)).Sub(message.Stats.At) != 10*time.Second {
		t.Fatal("media and coverage use different clock shifts")
	}
	report := Report{ClockCoverage: &e}
	report.sanitize()
	encoded, _ := json.Marshal(report.ClockCoverage)
	if strings.Contains(string(encoded), "private-label") {
		t.Fatal("report leaked coverage identity")
	}
	if e.Attempts[0].Clocks["shared"].Rows[0].UnixSeconds != 1793930000 {
		t.Fatal("sanitizer mutated source timeline")
	}
}
