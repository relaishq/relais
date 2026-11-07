package clocks

import (
	"bytes"
	"cmp"
	"encoding/json"
	"slices"
	"sort"

	"github.com/relaishq/relais/internal/operatorgates/observe"
)

// BindingError is a definite refusal before execution. Cause contains no host data.
type BindingError struct{ Cause string }

func (e *BindingError) Error() string { return e.Cause }

// BindingHost is a gate-owned precision tracking read, not launcher evidence.
// Reads may accumulate cursor pages. ReadComplete must cover the final page;
// RunStartUnixMS and BeforeRun come from the gate's own run boundary.
type BindingHost struct {
	Slot     string
	LeapMode string
	Tracking TrackingLog
}

// ReceiptBindingInput contains artifact bytes and independently expected values.
// SHA256 values are commitments to recompute, never trusted self-digests.
type ReceiptBindingInput struct {
	ReceiptJSON, EvidenceJSON     []byte
	ReceiptSHA256, EvidenceSHA256 string
	RunID                         string
	FrontendSlot, ControlSlot     string
	CallerLeapMode                string
	ConfigBounds, CallerBounds    Bounds
	ExpectedPolicy                SelectionPolicy
	BeforeRun                     []BindingHost
}

// ReceiptBinding is a value-only identity for later Init/coverage integration.
// This module has no execution call sites until atomic activation.
type ReceiptBinding struct {
	ReceiptSHA256, EvidenceSHA256 string
	Bounds                        Bounds
}

// BindReceipt validates the original trailing-at-freeze selection, then checks
// its frozen rows against independent reads. Later rows never move that window;
// there is no wall-clock age or freeze-gap test here.
func BindReceipt(in ReceiptBindingInput) (ReceiptBinding, error) {
	var receipt SelectionReceipt
	var evidence SelectionEvidence
	refuse := func(cause string) (ReceiptBinding, error) { return ReceiptBinding{}, &BindingError{cause} }
	const limit = 8 << 20 // Same closed artifact limit as clock-select.
	if observe.DecodeClosedLimit(in.ReceiptJSON, &receipt, nil, limit) != nil || observe.DecodeClosedLimit(in.EvidenceJSON, &evidence, nil, limit) != nil {
		return refuse("clock-binding-invalid")
	}
	if receipt.RunID != in.RunID || evidence.RunID != in.RunID {
		return refuse("clock-binding-run-differ")
	}
	if receipt.FrontendSlot != in.FrontendSlot || evidence.FrontendSlot != in.FrontendSlot || receipt.ControlSlot != in.ControlSlot || evidence.ControlSlot != in.ControlSlot {
		return refuse("clock-binding-roles-differ")
	}
	if receipt.CallerLeapMode != in.CallerLeapMode || evidence.CallerLeapMode != in.CallerLeapMode {
		return refuse("clock-binding-leap-differ")
	}
	if receipt.Bounds != in.ConfigBounds || receipt.Bounds != in.CallerBounds {
		return refuse("gate-caller-bounds-differ")
	}
	receiptSHA, err := selectionDigest(receipt)
	if err != nil || receiptSHA != in.ReceiptSHA256 {
		return refuse("clock-binding-digest-differ")
	}
	if receipt.Policy != in.ExpectedPolicy {
		return refuse("clock-binding-policy-differ")
	}
	selected, err := Select(evidence, in.ExpectedPolicy)
	if err != nil {
		return refuse("clock-binding-selection-differ")
	}
	if selected.Receipt.EvidenceSHA256 != in.EvidenceSHA256 || selected.Receipt.EvidenceSHA256 != receipt.EvidenceSHA256 {
		return refuse("clock-binding-digest-differ")
	}
	if selected.Receipt.Bounds != receipt.Bounds {
		return refuse("gate-caller-bounds-differ")
	}
	if selected.SHA256 != receiptSHA {
		return refuse("clock-binding-selection-differ")
	}
	if err := bindingReadback(evidence, in.BeforeRun); err != nil {
		return ReceiptBinding{}, err
	}
	return ReceiptBinding{ReceiptSHA256: receiptSHA, EvidenceSHA256: selected.Receipt.EvidenceSHA256, Bounds: receipt.Bounds}, nil
}

func bindingReadback(e SelectionEvidence, reads []BindingHost) error {
	if len(reads) != len(selectionSlots) {
		return &BindingError{"clock-binding-readback-differ"}
	}
	seen := map[string]bool{}
	for _, h := range reads {
		if !slices.Contains(selectionSlots, h.Slot) || seen[h.Slot] {
			return &BindingError{"clock-binding-roles-differ"}
		}
		seen[h.Slot] = true
		if h.LeapMode != e.CallerLeapMode {
			return &BindingError{"clock-binding-leap-differ"}
		}
		log := h.Tracking
		if !log.ReadComplete || len(log.Rows) > 16384 || len(log.Breaks) > 16384 || !slices.IsSortedFunc(log.Rows, func(a, b TrackingRow) int { return cmp.Compare(a.UnixSeconds, b.UnixSeconds) }) {
			return &BindingError{"clock-binding-readback-differ"}
		}
		// Check private source continuity before projecting away source labels.
		if _, _, err := selectionMaximum(log, e.StartUnixSeconds, e.EndUnixSeconds); err != nil {
			return &BindingError{"clock-binding-readback-differ"}
		}
		var frozen []TrackingRow
		for _, host := range e.Hosts {
			if host.Slot == h.Slot {
				frozen = bindingFrozenRows(host.Tracking.Rows, e.StartUnixSeconds, e.EndUnixSeconds)
			}
		}
		from, to := frozen[0].UnixSeconds, frozen[len(frozen)-1].UnixSeconds
		var observed []TrackingRow
		for _, row := range log.Rows {
			if row.UnixSeconds < from || row.UnixSeconds > to {
				continue
			}
			if !row.BeforeRun || (row.UnixSeconds+1)*1000 > log.RunStartUnixMS {
				return &BindingError{"clock-binding-readback-differ"}
			}
			observed = append(observed, row)
		}
		want, err1 := bindingRowBytes(frozen)
		got, err2 := bindingRowBytes(observed)
		if err1 != nil || err2 != nil || !bytes.Equal(want, got) {
			return &BindingError{"clock-binding-readback-differ"}
		}
	}
	return nil
}

// Keep the predecessor and every row the selector's interval maximum can use,
// including its closing successor. Additional readback history is not frozen.
func bindingFrozenRows(rows []TrackingRow, start, end int64) []TrackingRow {
	first := sort.Search(len(rows), func(i int) bool { return rows[i].UnixSeconds >= start-1 })
	if first > 0 {
		first--
	}
	last := sort.Search(len(rows), func(i int) bool { return rows[i].UnixSeconds > end+1 })
	if last < len(rows) {
		last++
	}
	from, to := rows[first].UnixSeconds, rows[last-1].UnixSeconds
	first = sort.Search(len(rows), func(i int) bool { return rows[i].UnixSeconds >= from })
	last = sort.Search(len(rows), func(i int) bool { return rows[i].UnixSeconds > to })
	return rows[first:last]
}

func bindingRowBytes(rows []TrackingRow) ([]byte, error) {
	rows = slices.Clone(rows)
	for i := range rows {
		rows[i].BeforeRun = false
		rows[i].Reference = ""
	}
	return json.Marshal(rows)
}
