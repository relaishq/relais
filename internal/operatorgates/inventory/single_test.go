package inventory

import (
	"strings"
	"testing"

	"github.com/relaishq/relais/pkg/operator"
)

func TestSinglePinOnlyForPartR(t *testing.T) {
	pin := Pin{Version: "0.4.0", Commit: strings.Repeat("a", 40), ArchiveSHA256: strings.Repeat("b", 64), ManifestSHA256: strings.Repeat("c", 64), BootSHA256: strings.Repeat("d", 64)}
	identity := operator.Identity{SourceCommit: pin.Commit, SHA256: pin.ArchiveSHA256}
	scenario := operator.Scenario{RelayTwo: true, Gates: []operator.Gate{operator.RelayHostLoss}, ReleaseA: identity, ReleaseB: identity}
	if err := ValidatePins([]Pin{pin}, scenario); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"pair", "four-host", "abm", "mixed", "identity", "manifest"} {
		s := scenario
		pins := []Pin{pin}
		switch mode {
		case "pair":
			pins = append(pins, pin)
		case "four-host":
			s.RelayTwo = false
		case "abm":
			s.Gates = []operator.Gate{operator.Rollout}
		case "mixed":
			s.ReleaseB.SourceCommit = strings.Repeat("e", 40)
		case "identity":
			pins[0].Commit = strings.Repeat("e", 40)
		case "manifest":
			pins[0].ManifestSHA256 = ""
		}
		if ValidatePins(pins, s) == nil {
			t.Fatal("accepted", mode)
		}
	}
}
