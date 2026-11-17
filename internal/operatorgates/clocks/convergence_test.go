package clocks

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Constant-quality histories make warm-up and full selection agree, including
// exact decimal rounding at the ceilings and co-located frontend/control.
func TestClockConvergenceUsesSelectionBounds(t *testing.T) {
	for _, c := range []struct {
		name, front, slot, decimal string
		value                      float64
		cause                      string
	}{
		{"floor", "shared", "shared", "1.000e-05", 1e-5, ""},
		{"shared-below", "shared", "shared", "5.745e-05", 5.745e-5, ""},
		{"shared-rounds-over", "shared", "shared", "5.750e-05", 5.750e-5, "clock-select-ceiling"},
		{"restored-shared", "shared", "shared", "1.099e-04", 1.099e-4, "clock-select-ceiling"},
		{"worker-one", "relay", "w1", "1.150e-04", 1.150e-4, "clock-select-ceiling"},
		{"worker-two", "relay", "w2", "1.150e-04", 1.150e-4, "clock-select-ceiling"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _ := selectionFixture()
			e.FrontendSlot = c.front
			e.Falsification.FrontendSlot = c.front
			p := SelectionPolicy{WindowSeconds: 120, FloorNS: 50000, ScaleNumerator: 2, ScaleDenominator: 1, AllowanceNS: 10000, SignalReserveNS: 50000, ControlCeilingNS: 250000, WorkerCeilingNS: 350000, MaxDispersionGrowthPPM: "5"}
			for i := range e.Hosts {
				if e.Hosts[i].Slot == c.slot {
					for j := range e.Hosts[i].Tracking.Rows {
						e.Hosts[i].Tracking.Rows[j].MaxErrorSeconds = c.value
						e.Hosts[i].Tracking.Rows[j].MaxErrorDecimal = c.decimal
					}
				}
			}
			_, selected := Select(e, p)
			converged := CheckConvergence(e, p)
			if c.cause == "" {
				require.NoError(t, selected)
				require.NoError(t, converged)
			} else {
				require.EqualError(t, selected, c.cause)
				require.EqualError(t, converged, c.cause)
			}
		})
	}
}

func TestClockConvergenceRequiresRecentTrustedEvidence(t *testing.T) {
	for name, change := range map[string]func(*SelectionEvidence){
		"clock-select-coverage":  func(e *SelectionEvidence) { e.Hosts[0].Tracking.Rows = nil },
		"clock-select-admission": func(e *SelectionEvidence) { e.Hosts = e.Hosts[:3] },
		"clock-select-column14": func(e *SelectionEvidence) {
			e.Hosts[0].Tracking.Rows[len(e.Hosts[0].Tracking.Rows)-1].MaxErrorDecimal = "invalid"
		},
		"clock-select-leap": func(e *SelectionEvidence) {
			e.Hosts[0].Tracking.Rows[len(e.Hosts[0].Tracking.Rows)-1].LeapStatus = "?"
		},
		"clock-lease-expired": func(e *SelectionEvidence) {
			e.Hosts[0].Lease.HostElapsedNS = 6e9
		},
		"clock-select-growth": func(e *SelectionEvidence) { e.Hosts[0].Lease.DispersionEndSeconds = "0.000011000" },
	} {
		t.Run(name, func(t *testing.T) {
			e, p := selectionFixture()
			change(&e)
			require.EqualError(t, CheckConvergence(e, p), name)
		})
	}
}
