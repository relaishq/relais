package clocks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"slices"
	"sort"
	"strconv"

	"github.com/relaishq/relais/internal/operatorgates/observe"
)

// SelectionPolicy is launcher policy. There are no built-in paid-run numbers.
type SelectionPolicy = observe.SelectionPolicy

type SelectionHost struct {
	Slot     string           `json:"slot"`
	LeapMode string           `json:"leapMode"`
	Tracking TrackingLog      `json:"tracking"`
	Lease    LeaseObservation `json:"lease"`
}

// SelectionEvidence retains the launcher's complete admission snapshot.
// 5b will cross-check it with the session's own pre-run rows before binding it.
type SelectionEvidence struct {
	RunID            string          `json:"runId"`
	FrontendSlot     string          `json:"frontendSlot"`
	ControlSlot      string          `json:"controlSlot"`
	CallerLeapMode   string          `json:"callerLeapMode"`
	StartUnixSeconds int64           `json:"startUnixSeconds"`
	EndUnixSeconds   int64           `json:"endUnixSeconds"`
	Hosts            []SelectionHost `json:"hosts"`
	Falsification    Falsification   `json:"falsification"`
}

type SelectedHost = observe.SelectedHost

type SelectionReceipt = observe.SelectionReceipt

type ClockSelection struct {
	Receipt SelectionReceipt `json:"receipt"`
	SHA256  string           `json:"sha256"`
}

var selectionSlots = []string{"relay", "shared", "w1", "w2"}
var column14Pattern = regexp.MustCompile(`^[0-9]\.[0-9]{3}e([+-][0-9]{2,3})$`)
var selectionRunPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Column14MaximumNS includes half the last printed unit of chrony's %.3e
// column 14. It returns an exact rational, before any policy scaling/rounding.
func Column14MaximumNS(raw string) (*big.Rat, error) { return observe.ClockMaximumNS(raw) }

// Select only records a decision. It neither changes fixed bounds nor invokes
// a judge, caller, process, clock, or transport.
func Select(input SelectionEvidence, policy SelectionPolicy) (ClockSelection, error) {
	var out ClockSelection
	declared, err := exact(policy.MaxDispersionGrowthPPM)
	if err != nil || declared.Sign() < 0 || policy.WindowSeconds < 120 || policy.FloorNS < 0 || policy.ScaleNumerator <= 0 || policy.ScaleDenominator <= 0 || policy.AllowanceNS < 0 || policy.SignalReserveNS < 0 || policy.ControlCeilingNS < 0 || policy.WorkerCeilingNS < 0 {
		return out, fmt.Errorf("clock-select-policy")
	}
	if !selectionRunPattern.MatchString(input.RunID) || !slices.Contains(selectionSlots, input.FrontendSlot) || !slices.Contains(selectionSlots, input.ControlSlot) || len(input.Hosts) != 4 || input.StartUnixSeconds < 3 || input.EndUnixSeconds <= input.StartUnixSeconds || input.EndUnixSeconds > math.MaxInt64/1000000000-3 || input.EndUnixSeconds-input.StartUnixSeconds != policy.WindowSeconds {
		return out, fmt.Errorf("clock-select-admission")
	}
	if input.CallerLeapMode != "smeared" && input.CallerLeapMode != "unsmeared" {
		return out, fmt.Errorf("clock-select-leap")
	}
	// Normalize copies, preserving caller-owned input and equal-second ordering.
	input.Hosts = slices.Clone(input.Hosts)
	sort.Slice(input.Hosts, func(i, j int) bool { return input.Hosts[i].Slot < input.Hosts[j].Slot })
	maximum := map[string]*big.Rat{}
	hosts := map[string]SelectionHost{}
	latest := int64(maxSafeInteger)
	r := SelectionReceipt{Version: 1, Formula: observe.ClockSelectionFormula, RunID: input.RunID, FrontendSlot: input.FrontendSlot, ControlSlot: input.ControlSlot, CallerLeapMode: input.CallerLeapMode, StartUnixSeconds: input.StartUnixSeconds, EndUnixSeconds: input.EndUnixSeconds, Policy: policy, Hosts: []SelectedHost{}}
	for i, h := range input.Hosts {
		if !slices.Contains(selectionSlots, h.Slot) || hosts[h.Slot].Slot != "" || len(h.Tracking.Rows) > 16384 || len(h.Tracking.Breaks) > 16384 || !h.Tracking.ReadComplete {
			return out, fmt.Errorf("clock-select-admission")
		}
		if h.LeapMode != input.CallerLeapMode {
			return out, fmt.Errorf("clock-select-leap")
		}
		h.Tracking.Rows = slices.Clone(h.Tracking.Rows)
		for j := 1; j < len(h.Tracking.Rows); j++ {
			if h.Tracking.Rows[j].UnixSeconds < h.Tracking.Rows[j-1].UnixSeconds {
				return out, fmt.Errorf("clock-select-break")
			}
		}
		input.Hosts[i] = h
		hosts[h.Slot] = h
		m, count, e := selectionMaximum(h.Tracking, input.StartUnixSeconds, input.EndUnixSeconds)
		if e != nil {
			return out, e
		}
		if count < 10 {
			return out, fmt.Errorf("clock-select-updates")
		}
		latest = min(latest, h.Tracking.Rows[len(h.Tracking.Rows)-1].UnixSeconds)
		maximum[h.Slot] = m
		lease, e := RecordLease(h.Lease, h.Lease.HostElapsedNS)
		if e != nil {
			return out, e
		}
		if lease.Expired {
			return out, fmt.Errorf("clock-lease-expired")
		}
		measuredNS, _ := strconv.ParseInt(h.Lease.MeasuredUnixNS, 10, 64)
		lastRowNS := h.Tracking.Rows[len(h.Tracking.Rows)-1].UnixSeconds * 1000000000
		// Tracking rows have one-second resolution; measurement freshness is bidirectional.
		measurementOffsetNS := measuredNS - lastRowNS
		if measurementOffsetNS < -1000000000 || measurementOffsetNS > 60000000000 {
			return out, fmt.Errorf("clock-select-stale")
		}
		// RecordLease produced this rational; it need not fit the input text limit.
		measured, _ := new(big.Rat).SetString(lease.GrowthPPM)
		if measured.Cmp(declared) > 0 {
			return out, fmt.Errorf("clock-select-growth")
		}
		r.Hosts = append(r.Hosts, SelectedHost{Slot: h.Slot, LeapMode: h.LeapMode, Updates: count, MaximumNS: m.RatString(), DeclaredGrowthPPM: declared.RatString(), MeasuredLease: lease})
	}
	// Only the trailing common window can be selected, never an older clean slice.
	if input.EndUnixSeconds != latest-1 {
		return out, fmt.Errorf("clock-select-trailing-window")
	}
	if err := selectionProbes(input, hosts); err != nil {
		return out, err
	}
	front, err := selectionHostBound(maximum[input.FrontendSlot], policy)
	if err != nil {
		return out, err
	}
	control, err := selectionHostBound(maximum[input.ControlSlot], policy)
	if err != nil {
		return out, err
	}
	workerM := maximum["w1"]
	if maximum["w2"].Cmp(workerM) > 0 {
		workerM = maximum["w2"]
	}
	worker, err := selectionHostBound(workerM, policy)
	if err != nil {
		return out, err
	}
	cb := new(big.Int).Add(big.NewInt(front), big.NewInt(control))
	wb := new(big.Int).Add(big.NewInt(front), big.NewInt(worker))
	wb.Add(wb, big.NewInt(policy.SignalReserveNS))
	if !cb.IsInt64() || !wb.IsInt64() || cb.Cmp(big.NewInt(policy.ControlCeilingNS)) > 0 || wb.Cmp(big.NewInt(policy.WorkerCeilingNS)) > 0 {
		return out, fmt.Errorf("clock-select-ceiling")
	}
	r.Bounds = Bounds{FrontendControlBoundNS: cb.Int64(), FrontendWorkerBoundNS: wb.Int64(), DriftPPM: 100}
	// Cursors are reader-local, not admission evidence. Source labels never leave
	// this function: the receipt only carries a digest of the numeric snapshot.
	for i := range input.Hosts {
		input.Hosts[i].Tracking.Cursor = nil
	}
	r.EvidenceSHA256, err = selectionDigest(input)
	if err != nil {
		return out, err
	}
	out.Receipt = r
	out.SHA256, err = selectionDigest(r)
	return out, err
}

func selectionDigest(v any) (string, error) {
	b, err := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), err
}

func selectionHostBound(m *big.Rat, p SelectionPolicy) (int64, error) {
	scaled := new(big.Rat).Mul(m, big.NewRat(p.ScaleNumerator, p.ScaleDenominator))
	if scaled.Cmp(big.NewRat(p.FloorNS, 1)) < 0 {
		scaled.SetInt64(p.FloorNS)
	}
	scaled.Add(scaled, big.NewRat(p.AllowanceNS, 1))
	return ceilNS(scaled)
}

func selectionMaximum(log TrackingLog, start, end int64) (*big.Rat, int, error) {
	if len(log.Rows) < 2 || start < 2 || end < start || log.Rows[0].UnixSeconds > start-1 || log.Rows[len(log.Rows)-1].UnixSeconds < end+1 {
		return nil, 0, fmt.Errorf("clock-select-coverage")
	}
	for _, b := range log.Breaks {
		if b.FromUnixSeconds < 0 || b.UnixSeconds < b.FromUnixSeconds || b.UnixSeconds >= start-2 {
			return nil, 0, fmt.Errorf("clock-select-break")
		}
	}
	m := new(big.Rat)
	updates := map[TrackingRow]bool{}
	for k, row := range log.Rows {
		if row.UnixSeconds < 0 || row.UnixSeconds > math.MaxInt64/1000000000-3 {
			return nil, 0, fmt.Errorf("clock-select-coverage")
		}
		if row.UnixSeconds >= start && row.UnixSeconds <= end {
			identity := row
			identity.BeforeRun = false
			updates[identity] = true
		}
		if row.UnixSeconds < start-1 {
			continue
		}
		prev := row
		if k > 0 {
			prev = log.Rows[k-1]
		}
		if row.UnixSeconds-prev.UnixSeconds > 60 {
			return nil, 0, fmt.Errorf("clock-select-coverage")
		}
		if prev.LeapStatus != "N" || row.LeapStatus != "N" {
			return nil, 0, fmt.Errorf("clock-select-leap")
		}
		if !finite(row.OffsetSeconds) || math.Abs(row.OffsetSeconds) > row.MaxErrorSeconds || prev.Reference != row.Reference {
			return nil, 0, fmt.Errorf("clock-select-break")
		}
		// Admission maxima stop at the closing interval. Trust checks above
		// continue through each host's latest row, including faster hosts.
		if k == 0 && row.UnixSeconds != start-1 || k > 0 && prev.UnixSeconds > end+1 {
			continue
		}
		value, err := Column14MaximumNS(row.MaxErrorDecimal)
		parsed, parseErr := strconv.ParseFloat(row.MaxErrorDecimal, 64)
		if err != nil || parseErr != nil || parsed != row.MaxErrorSeconds {
			return nil, 0, fmt.Errorf("clock-select-column14")
		}
		if value.Cmp(m) > 0 {
			m.Set(value)
		}
	}
	return m, len(updates), nil
}

func selectionProbes(input SelectionEvidence, hosts map[string]SelectionHost) error {
	f := input.Falsification
	if f.FrontendSlot != input.FrontendSlot || !f.LossCounts.valid() || f.LossCounts != (ProbeLossCounts{}) || f.CountsByStatus["falsified"] != 0 || len(f.Records) == 0 || len(f.Records) > ProbeRecordLimit {
		return fmt.Errorf("clock-select-probes")
	}
	byTarget := map[string][]int64{}
	for _, r := range f.Records {
		fs, ok1 := observe.CoverageNS(r.FrontendSend)
		fr, ok2 := observe.CoverageNS(r.FrontendReceive)
		if r.Status == "falsified" {
			return fmt.Errorf("clock-select-probes")
		}
		// An unchecked warm-up request with a known receive time is outside
		// admission. It cannot poison a later clean trailing window. Unknown
		// times still refuse; a falsification above is sticky at every time.
		if ok2 && fr < input.StartUnixSeconds*1e9 || ok1 && fs > input.EndUnixSeconds*1e9 {
			continue
		}
		hr, ok3 := observe.CoverageNS(r.HostReceive)
		hs, ok4 := observe.CoverageNS(r.HostSend)
		if !ok1 || !ok2 || !ok3 || !ok4 || fr < fs || hs < hr || fr-fs < hs-hr {
			return fmt.Errorf("clock-select-probes")
		}
		if fs < input.StartUnixSeconds*1e9 || fr > input.EndUnixSeconds*1e9 {
			continue
		}
		target := r.Target
		if !slices.Contains([]string{"control", "w1", "w2"}, target) || r.Status != "not-falsified" {
			return fmt.Errorf("clock-select-probes")
		}
		slot := target
		if target == "control" {
			slot = input.ControlSlot
		}
		fm, _, e1 := selectionMaximum(hosts[input.FrontendSlot].Tracking, fs/1e9, (fr+1e9-1)/1e9)
		hm, _, e2 := selectionMaximum(hosts[slot].Tracking, hr/1e9, (hs+1e9-1)/1e9)
		if e1 != nil || e2 != nil {
			return fmt.Errorf("clock-select-probes")
		}
		bound, err := ceilNS(new(big.Rat).Add(fm, hm))
		if err != nil || hs-fr > bound || hr-fs < -bound {
			return fmt.Errorf("clock-select-probes")
		}
		byTarget[target] = append(byTarget[target], fs)
	}
	for _, target := range []string{"control", "w1", "w2"} {
		times := byTarget[target]
		slices.Sort(times)
		if len(times) < 2 || times[0] > input.StartUnixSeconds*1e9+2e9 || times[len(times)-1] < input.EndUnixSeconds*1e9-2e9 {
			return fmt.Errorf("clock-select-probes")
		}
		for i := 1; i < len(times); i++ {
			if times[i]-times[i-1] > 2e9 {
				return fmt.Errorf("clock-select-probes")
			}
		}
	}
	return nil
}
