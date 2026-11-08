package observe

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

const ClockSelectionFormula = "ceil(max(f,s*M_f)+A)+ceil(max(f,s*M_h)+A);worker:+K"

func IsBound(receiptSHA string, binding *ClockBinding) bool {
	return receiptSHA != "" || binding != nil
}

type SelectionPolicy struct {
	WindowSeconds          int64  `json:"windowSeconds"`
	FloorNS                int64  `json:"floorNs"`
	ScaleNumerator         int64  `json:"scaleNumerator"`
	ScaleDenominator       int64  `json:"scaleDenominator"`
	AllowanceNS            int64  `json:"allowanceNs"`
	SignalReserveNS        int64  `json:"signalReserveNs"`
	ControlCeilingNS       int64  `json:"controlCeilingNs"`
	WorkerCeilingNS        int64  `json:"workerCeilingNs"`
	MaxDispersionGrowthPPM string `json:"maxDispersionGrowthPpm"`
}

type SelectedHost struct {
	Slot              string        `json:"slot"`
	LeapMode          string        `json:"leapMode"`
	Updates           int           `json:"updates"`
	MaximumNS         string        `json:"maximumNs"`
	DeclaredGrowthPPM string        `json:"declaredGrowthPpm"`
	MeasuredLease     RecordedLease `json:"measuredLease"`
}

type SelectionReceipt struct {
	Version          int             `json:"version"`
	Formula          string          `json:"formula"`
	RunID            string          `json:"runId"`
	FrontendSlot     string          `json:"frontendSlot"`
	ControlSlot      string          `json:"controlSlot"`
	CallerLeapMode   string          `json:"callerLeapMode"`
	StartUnixSeconds int64           `json:"startUnixSeconds"`
	EndUnixSeconds   int64           `json:"endUnixSeconds"`
	Policy           SelectionPolicy `json:"policy"`
	Hosts            []SelectedHost  `json:"hosts"`
	Bounds           ClockBounds     `json:"bounds"`
	EvidenceSHA256   string          `json:"evidenceSha256"`
}

type ClockBounds struct {
	FrontendControlBoundNS int64   `json:"frontendControlBoundNs"`
	FrontendWorkerBoundNS  int64   `json:"frontendWorkerBoundNs"`
	DriftPPM               float64 `json:"driftPpm"`
}

type RecordedLease struct {
	GrowthPPM   string `json:"growthPpm"`
	BoundNS     int64  `json:"boundNs"`
	RemainingNS int64  `json:"remainingNs"`
	Expired     bool   `json:"expired"`
}

type ReceiptBinding struct {
	ReceiptSHA256  string      `json:"receiptSha256"`
	EvidenceSHA256 string      `json:"evidenceSha256"`
	Bounds         ClockBounds `json:"bounds"`
}

// ClockBinding carries the canonical receipt proof, never raw host evidence.
type ClockBinding struct {
	Binding ReceiptBinding   `json:"binding"`
	Receipt SelectionReceipt `json:"receipt"`
}

var clockDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var clockNumberPattern = regexp.MustCompile(`^[0-9.eE+/-]+$`)
var clockRunPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func ClockDigestValid(s string) bool { return clockDigestPattern.MatchString(s) }
func clockNumber(s string) bool {
	if len(s) > 80 || !clockNumberPattern.MatchString(s) {
		return false
	}
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		n, err := strconv.Atoi(s[i+1:])
		if err != nil || n < -70 || n > 70 {
			return false
		}
	}
	r, ok := new(big.Rat).SetString(s)
	return ok && r.Sign() >= 0 && r.Num().BitLen() <= 256 && r.Denom().BitLen() <= 256
}
func (b ClockBinding) Valid(expected string) bool {
	r := b.Receipt
	if !coverageSlot(r.FrontendSlot) || !coverageSlot(r.ControlSlot) || !clockRunPattern.MatchString(r.RunID) || r.Formula != ClockSelectionFormula || (r.CallerLeapMode != "smeared" && r.CallerLeapMode != "unsmeared") || !ClockDigestValid(r.EvidenceSHA256) || len(r.Hosts) != 4 || !clockNumber(r.Policy.MaxDispersionGrowthPPM) {
		return false
	}
	seen := map[string]bool{}
	for _, h := range r.Hosts {
		if !coverageSlot(h.Slot) || seen[h.Slot] || h.LeapMode != r.CallerLeapMode || !clockNumber(h.MaximumNS) || !clockNumber(h.DeclaredGrowthPPM) || !clockNumber(h.MeasuredLease.GrowthPPM) {
			return false
		}
		seen[h.Slot] = true
	}

	raw, err := json.Marshal(b.Receipt)
	return err == nil && ClockDigestValid(expected) && b.Binding.ReceiptSHA256 == expected && fmt.Sprintf("%x", sha256.Sum256(raw)) == expected && b.Binding.Bounds == b.Receipt.Bounds && b.Binding.EvidenceSHA256 == b.Receipt.EvidenceSHA256 && b.Receipt.Version == 1 && b.Receipt.Bounds.DriftPPM == 100 && b.Receipt.Bounds.FrontendControlBoundNS >= 0 && b.Receipt.Bounds.FrontendControlBoundNS <= 1e9 && b.Receipt.Bounds.FrontendWorkerBoundNS >= 0 && b.Receipt.Bounds.FrontendWorkerBoundNS <= 1e9 && b.Receipt.Policy.SignalReserveNS >= 0 && b.Receipt.Policy.SignalReserveNS <= b.Receipt.Bounds.FrontendWorkerBoundNS
}
