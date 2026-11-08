package clocks

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/observe"
)

func TestUnboundMainBytes(t *testing.T) {
	e := CoverageEvidence{Bounds: FixedCoverageBounds(), FrontendSlot: "shared", ControlSlot: "shared", Windows: []observe.CoverageWindow{{Kind: "move", StartUnixNS: "1793930010000000000", EndUnixNS: "1793930020000000000"}}, Attempts: []CoverageAttempt{{Version: 2, Clocks: map[string]observe.CoverageTimeline{"shared": {ReadComplete: true, Rows: []observe.CoverageRow{{UnixSeconds: 1793930000, LeapStatus: "N", MaxErrorSeconds: .0001}, {UnixSeconds: 1793930030, LeapStatus: "N", MaxErrorSeconds: .0001}}}}}}}
	raw, err := json.Marshal(struct {
		Evidence      CoverageEvidence
		Qualification observe.CoverageQualification
	}{e, e.Qualification()})
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(raw))
	if got != "954e41f2af76c1f8c85f2d707de3644673c0487ecbb688fcd3fa0a8f668cdb28" {
		t.Fatal("MUTATION: unbound-main-bytes", got)
	}
}
