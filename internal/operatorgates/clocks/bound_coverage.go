package clocks

import "github.com/relaishq/relais/internal/operatorgates/observe"

// WithBinding constructs evidence for a run whose caller uses this receipt.
// Offline evaluation must retain the original binding, never attach a new one.
func (e CoverageEvidence) WithBinding(b ReceiptBinding, r SelectionReceipt) (CoverageEvidence, error) {
	if observe.IsBound(e.ClockReceiptSHA256, e.Binding) {
		return CoverageEvidence{}, &BindingError{"clock-coverage-binding"}
	}
	proof := &observe.ClockBinding{Binding: b, Receipt: r}
	e.ClockReceiptSHA256, e.Binding, e.Bounds = b.ReceiptSHA256, proof, b.Bounds
	if err := e.ValidateBinding(b.ReceiptSHA256); err != nil {
		return CoverageEvidence{}, err
	}
	return e, nil
}

// coverageParameters is the single policy source for bound and unbound APIs.
func (e CoverageEvidence) coverageParameters() (Bounds, int64, error) {
	if observe.IsBound(e.ClockReceiptSHA256, e.Binding) {
		if e.Binding == nil || !e.Binding.Valid(e.ClockReceiptSHA256) || e.Bounds != e.Binding.Binding.Bounds || e.FrontendSlot != e.Binding.Receipt.FrontendSlot || e.ControlSlot != e.Binding.Receipt.ControlSlot {
			return Bounds{}, 0, &BindingError{"clock-coverage-binding"}
		}
		return e.Binding.Binding.Bounds, e.Binding.Receipt.Policy.SignalReserveNS, nil
	}
	fixed := FixedCoverageBounds()
	if e.Bounds != fixed {
		return fixed, 0, &BindingError{"clock-coverage-bound"}
	}
	return fixed, 0, nil
}

func (e CoverageEvidence) ValidateBinding(expected string) error {
	if e.ClockReceiptSHA256 != expected {
		return &BindingError{"clock-coverage-binding"}
	}
	_, _, err := e.coverageParameters()
	return err
}
func (e CoverageEvidence) Check(rows map[string]Sample) (Evidence, error) {
	b, k, err := e.coverageParameters()
	if err != nil {
		return Evidence{}, err
	}
	b.FrontendWorkerBoundNS -= k
	return Check(rows, b, e.FrontendSlot, e.ControlSlot)
}
func (e CoverageEvidence) SignalMidpoint(before, after string, workerBound int64) (string, error) {
	b, _, err := e.coverageParameters()
	if err != nil {
		return "", err
	}
	return SignalMidpoint(before, after, workerBound, b.FrontendWorkerBoundNS)
}
