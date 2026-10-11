package callharness_test

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

// Run the actual planned-move assertion in a child test process. A green
// child would prove that unrelated video uncertainty hid trusted audio loss.
func TestPlannedLossGateRejectsAudioWithVideoNoise(t *testing.T) {
	const probe = "RELAIS_YARDSTICK_LOSS_GATE_PROBE"
	if os.Getenv(probe) == "1" {
		assertNoLostContent(t, callharness.MoveReport{Measurement: callharness.EventMeasurement{
			SettleWindow: 500 * time.Millisecond, LostAudio: 120 * time.Millisecond,
			AudioLoss: callharness.MetricVerdict{Trusted: true, Failed: true}, VideoLoss: callharness.MetricVerdict{Trusted: true},
			Video: callharness.FreshnessReport{Verdict: callharness.MetricVerdict{Reasons: []string{"noisy baseline"}}}, Inconclusive: true,
		}})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPlannedLossGateRejectsAudioWithVideoNoise$")
	cmd.Env = append(os.Environ(), probe+"=1")
	output, err := cmd.CombinedOutput()
	require.Error(t, err, "trusted audio loss must fail the actual planned-move gate")
	require.Contains(t, string(output), "planned move lost sent audio")
}
