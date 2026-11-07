package clocks

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func bindingFixture(t *testing.T, edits ...func(*SelectionEvidence)) ReceiptBindingInput {
	t.Helper()
	e, p := selectionFixture()
	// One faster host has the selector's closing successor after end+1.
	last := e.Hosts[0].Tracking.Rows[len(e.Hosts[0].Tracking.Rows)-1]
	last.UnixSeconds++
	e.Hosts[0].Tracking.Rows = append(e.Hosts[0].Tracking.Rows, last)
	for _, edit := range edits {
		edit(&e)
	}
	s := selectGood(t, e, p)
	raw, err := json.Marshal(s.Receipt)
	require.NoError(t, err)
	evidence, err := json.Marshal(e)
	require.NoError(t, err)
	in := ReceiptBindingInput{ReceiptJSON: raw, EvidenceJSON: evidence, ReceiptSHA256: s.SHA256, EvidenceSHA256: s.Receipt.EvidenceSHA256, RunID: e.RunID, FrontendSlot: e.FrontendSlot, ControlSlot: e.ControlSlot, CallerLeapMode: e.CallerLeapMode, ConfigBounds: s.Receipt.Bounds, CallerBounds: s.Receipt.Bounds, ExpectedPolicy: p}
	for _, h := range e.Hosts {
		log := h.Tracking
		log.RunStartUnixMS = (e.EndUnixSeconds + 3) * 1000
		log.Rows = slices.Clone(log.Rows)
		for i := range log.Rows {
			log.Rows[i].Reference = "local-source"
		}
		in.BeforeRun = append(in.BeforeRun, BindingHost{Slot: h.Slot, LeapMode: h.LeapMode, Tracking: log})
	}
	return in
}

func editBindingReceipt(t *testing.T, in *ReceiptBindingInput, edit func(*SelectionReceipt)) {
	t.Helper()
	var r SelectionReceipt
	require.NoError(t, json.Unmarshal(in.ReceiptJSON, &r))
	edit(&r)
	var err error
	in.ReceiptJSON, err = json.Marshal(r)
	require.NoError(t, err)
	in.ReceiptSHA256, err = selectionDigest(r)
	require.NoError(t, err)
}

func TestReceiptBindingRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, cause string
		edit        func(*ReceiptBindingInput)
	}{
		{"run", "clock-binding-run-differ", func(in *ReceiptBindingInput) { in.RunID = "other-run" }},
		{"roles", "clock-binding-roles-differ", func(in *ReceiptBindingInput) { in.ControlSlot = "relay" }},
		{"mode", "clock-binding-leap-differ", func(in *ReceiptBindingInput) { in.CallerLeapMode = "smeared" }},
		{"policy", "clock-binding-policy-differ", func(in *ReceiptBindingInput) {
			editBindingReceipt(t, in, func(r *SelectionReceipt) { r.Policy.FloorNS++ })
		}},
		{"receipt-digest", "clock-binding-digest-differ", func(in *ReceiptBindingInput) { in.ReceiptSHA256 = strings.Repeat("a", 64) }},
		{"evidence-digest", "clock-binding-digest-differ", func(in *ReceiptBindingInput) { in.EvidenceSHA256 = strings.Repeat("a", 64) }},
		{"config", "gate-caller-bounds-differ", func(in *ReceiptBindingInput) { in.ConfigBounds.FrontendControlBoundNS++ }},
		{"caller", "gate-caller-bounds-differ", func(in *ReceiptBindingInput) { in.CallerBounds.FrontendWorkerBoundNS++ }},
		{"drift", "gate-caller-bounds-differ", func(in *ReceiptBindingInput) { in.CallerBounds.DriftPPM = 99 }},
		{"selected-bounds", "gate-caller-bounds-differ", func(in *ReceiptBindingInput) {
			editBindingReceipt(t, in, func(r *SelectionReceipt) { r.Bounds.DriftPPM = 99 })
			in.ConfigBounds.DriftPPM, in.CallerBounds.DriftPPM = 99, 99
		}},
		{"selection", "clock-binding-selection-differ", func(in *ReceiptBindingInput) {
			editBindingReceipt(t, in, func(r *SelectionReceipt) { r.Hosts[0].Updates++ })
		}},
		{"invalid", "clock-binding-invalid", func(in *ReceiptBindingInput) { in.ReceiptJSON = []byte(`{"version":1,"version":1}`) }},
		{"unknown", "clock-binding-invalid", func(in *ReceiptBindingInput) {
			in.EvidenceJSON = bytes.Replace(in.EvidenceJSON, []byte(`"runId"`), []byte(`"extra":0,"runId"`), 1)
		}},
		{"bad-selection", "clock-binding-selection-differ", func(in *ReceiptBindingInput) {
			in.EvidenceJSON = bytes.ReplaceAll(in.EvidenceJSON, []byte(`"maxErrorDecimal":"1.234e-06"`), []byte(`"maxErrorDecimal":""`))
		}},
		{"host-count", "clock-binding-readback-differ", func(in *ReceiptBindingInput) { in.BeforeRun = in.BeforeRun[:3] }},
		{"host-slot", "clock-binding-roles-differ", func(in *ReceiptBindingInput) { in.BeforeRun[0].Slot = "shared" }},
		{"host-mode", "clock-binding-leap-differ", func(in *ReceiptBindingInput) { in.BeforeRun[0].LeapMode = "smeared" }},
		{"incomplete", "clock-binding-readback-differ", func(in *ReceiptBindingInput) { in.BeforeRun[0].Tracking.ReadComplete = false }},
		{"missing-row", "clock-binding-readback-differ", func(in *ReceiptBindingInput) {
			in.BeforeRun[0].Tracking.Rows = slices.Delete(in.BeforeRun[0].Tracking.Rows, 10, 11)
		}},
		{"token", "clock-binding-readback-differ", func(in *ReceiptBindingInput) { in.BeforeRun[0].Tracking.Rows[10].MaxErrorDecimal = "1.234e-006" }},
		{"numeric-row", "clock-binding-readback-differ", func(in *ReceiptBindingInput) { in.BeforeRun[0].Tracking.Rows[10].SkewPPM++ }},
		{"predecessor", "clock-binding-readback-differ", func(in *ReceiptBindingInput) { in.BeforeRun[0].Tracking.Rows[0].SkewPPM++ }},
		{"successor", "clock-binding-readback-differ", func(in *ReceiptBindingInput) {
			rows := in.BeforeRun[0].Tracking.Rows
			rows[len(rows)-1].SkewPPM++
		}},
		{"before-run", "clock-binding-readback-differ", func(in *ReceiptBindingInput) { in.BeforeRun[0].Tracking.Rows[10].BeforeRun = false }},
		{"run-boundary", "clock-binding-readback-differ", func(in *ReceiptBindingInput) { in.BeforeRun[0].Tracking.RunStartUnixMS = 1 }},
		{"source", "clock-binding-readback-differ", func(in *ReceiptBindingInput) { in.BeforeRun[0].Tracking.Rows[10].Reference = "different-source" }},
		{"leap", "clock-binding-readback-differ", func(in *ReceiptBindingInput) { in.BeforeRun[0].Tracking.Rows[10].LeapStatus = "+" }},
		{"break", "clock-binding-readback-differ", func(in *ReceiptBindingInput) {
			log := &in.BeforeRun[0].Tracking
			at := log.Rows[10].UnixSeconds
			log.Breaks = []TrackingBreak{{Kind: "source-change", FromUnixSeconds: at, UnixSeconds: at}}
		}},
		{"order", "clock-binding-readback-differ", func(in *ReceiptBindingInput) {
			log := &in.BeforeRun[0].Tracking
			last := log.Rows[len(log.Rows)-1]
			last.UnixSeconds += 20
			log.Rows = append(log.Rows, last)
			last.UnixSeconds--
			log.Rows = append(log.Rows, last)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := bindingFixture(t)
			tc.edit(&in)
			got, err := BindReceipt(in)
			var refusal *BindingError
			if !errors.As(err, &refusal) || refusal.Cause != tc.cause || got != (ReceiptBinding{}) {
				t.Fatalf("MUTATION: binding-%s: got %+v, %v; want %s", tc.name, got, err, tc.cause)
			}
		})
	}
}

func TestReceiptBindingEqualSecondBoundaries(t *testing.T) {
	for _, side := range []string{"predecessor", "successor"} {
		t.Run(side, func(t *testing.T) {
			in := bindingFixture(t, func(e *SelectionEvidence) {
				rows, index := e.Hosts[0].Tracking.Rows, 0
				if side == "successor" {
					index = len(rows) - 1
				}
				row := rows[index]
				row.SkewPPM++
				e.Hosts[0].Tracking.Rows = slices.Insert(rows, index, row)
			})
			if _, err := BindReceipt(in); err != nil {
				t.Fatal("MUTATION: binding-equal-second", side, err)
			}
		})
	}
}

func TestReceiptBindingEveryHost(t *testing.T) {
	for i, slot := range selectionSlots {
		t.Run(slot, func(t *testing.T) {
			in := bindingFixture(t)
			in.BeforeRun[i].Tracking.Rows[10].SkewPPM++
			_, err := BindReceipt(in)
			var refusal *BindingError
			if !errors.As(err, &refusal) || refusal.Cause != "clock-binding-readback-differ" {
				t.Fatal("MUTATION: binding-each-host", slot, err)
			}
		})
	}
}

func TestReceiptBindingFrozenReadback(t *testing.T) {
	in := bindingFixture(t)
	// Re-encoding does not require canonical input whitespace or field order.
	var r map[string]any
	require.NoError(t, json.Unmarshal(in.ReceiptJSON, &r))
	var err error
	in.ReceiptJSON, err = json.MarshalIndent(r, "", "  ")
	require.NoError(t, err)
	slices.Reverse(in.BeforeRun)
	// Head Init can be hours later. Keep the frozen rows, and validate all later
	// trust state without selecting a newer window or imposing a freeze gap.
	for i := range in.BeforeRun {
		log := &in.BeforeRun[i].Tracking
		last := log.Rows[len(log.Rows)-1]
		for range 120 {
			last.UnixSeconds += 60
			last.BeforeRun = false
			log.Rows = append(log.Rows, last)
		}
	}
	before, err := json.Marshal(in)
	require.NoError(t, err)
	got, err := BindReceipt(in)
	if err != nil || got.ReceiptSHA256 != in.ReceiptSHA256 || got.EvidenceSHA256 != in.EvidenceSHA256 || got.Bounds != in.ConfigBounds {
		t.Fatal("MUTATION: binding-frozen-window", got, err)
	}
	after, err := json.Marshal(in)
	require.NoError(t, err)
	require.Equal(t, before, after, "binding must not mutate caller-owned reads or bytes")
}
