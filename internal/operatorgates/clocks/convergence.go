package clocks

import (
	"fmt"
	"math/big"
	"slices"
	"strconv"

	"github.com/relaishq/relais/pkg/operator"
)

// CheckConvergence checks the latest tracking interval on every host with
// the selector's exact column-14 rounding and B(host) formula. This is only
// a warm-up check: Select must still admit a fresh full window with probes.
func CheckConvergence(input SelectionEvidence, policy SelectionPolicy) error {
	declared, err := CheckPolicy(policy)
	if err != nil {
		return err
	}
	if !validSelectionHosts(input.Hosts) || !selectionHasSlot(input.Hosts, input.FrontendSlot) || !selectionHasSlot(input.Hosts, input.ControlSlot) {
		return fmt.Errorf("clock-select-admission")
	}
	bounds := map[string]int64{}
	for _, h := range input.Hosts {
		if _, exists := bounds[h.Slot]; exists || !slices.Contains(operator.HostSlots(true), h.Slot) || !h.Tracking.ReadComplete {
			return fmt.Errorf("clock-select-admission")
		}
		rows := h.Tracking.Rows
		if len(rows) < 2 {
			return fmt.Errorf("clock-select-coverage")
		}
		previous, latest := rows[len(rows)-2], rows[len(rows)-1]
		if latest.UnixSeconds <= previous.UnixSeconds {
			return fmt.Errorf("clock-select-updates")
		}
		// Keep the selector's opening-boundary rule and all recorded breaks.
		m, _, err := selectionMaximum(h.Tracking, previous.UnixSeconds, latest.UnixSeconds-1)
		if err != nil {
			return err
		}
		lease, err := RecordLease(h.Lease, h.Lease.HostElapsedNS)
		if err != nil {
			return err
		}
		if lease.Expired {
			return fmt.Errorf("clock-lease-expired")
		}
		measured, _ := strconv.ParseInt(h.Lease.MeasuredUnixNS, 10, 64)
		age := measured - latest.UnixSeconds*1000000000
		if age < -1000000000 || age > 60000000000 {
			return fmt.Errorf("clock-select-stale")
		}
		growth, _ := new(big.Rat).SetString(lease.GrowthPPM)
		if growth.Cmp(declared) > 0 {
			return fmt.Errorf("clock-select-growth")
		}
		bounds[h.Slot], err = selectionHostBound(m, policy)
		if err != nil {
			return err
		}
	}
	front := big.NewInt(bounds[input.FrontendSlot])
	control := new(big.Int).Add(front, big.NewInt(bounds[input.ControlSlot]))
	worker := new(big.Int).Add(front, big.NewInt(max(bounds["w1"], bounds["w2"])))
	worker.Add(worker, big.NewInt(policy.SignalReserveNS))
	if control.Cmp(big.NewInt(policy.ControlCeilingNS)) > 0 || worker.Cmp(big.NewInt(policy.WorkerCeilingNS)) > 0 {
		return fmt.Errorf("clock-select-ceiling")
	}
	return nil
}
