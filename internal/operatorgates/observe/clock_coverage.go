package observe

import (
	"encoding/json"
	"math"
	"math/big"
	"sort"
	"strconv"
)

const coverageSecond int64 = 1e9

// Coverage types are numeric public evidence for clock qualification.
// Absolute nanoseconds stay decimal strings; the evaluator never reads a clock.
type CoverageRow struct {
	UnixSeconds     int64   `json:"unixSeconds"`
	MaxErrorSeconds float64 `json:"maxErrorSeconds"`
	LeapStatus      string  `json:"leapStatus"`
	MaxErrorDecimal string  `json:"maxErrorDecimal,omitempty"`
}
type CoverageBreak struct {
	FromUnixSeconds int64  `json:"fromUnixSeconds"`
	UnixSeconds     int64  `json:"unixSeconds"`
	Kind            string `json:"kind"`
}
type CoverageTimeline struct {
	Rows         []CoverageRow   `json:"rows"`
	Breaks       []CoverageBreak `json:"breaks"`
	ReadComplete bool            `json:"readComplete"`
}
type CoverageWindow struct {
	Phase       int    `json:"phase"`
	Event       int    `json:"event"`
	Kind        string `json:"kind"`
	Worker      string `json:"worker"`
	StartUnixNS string `json:"startUnixNs"`
	EndUnixNS   string `json:"endUnixNs"`
}

// SignalDiagnostics is optional host scheduling evidence, never qualification.
// Usage deltas include the adjacent getrusage calls; CPU IDs are sampled outside.
type SignalDiagnostics struct {
	ThreadCPUTimeNS     *int64 `json:"threadCPUTimeNs,omitempty"`
	Priority            string `json:"priority"`
	InvoluntarySwitches *int64 `json:"involuntarySwitches,omitempty"`
	VoluntarySwitches   *int64 `json:"voluntarySwitches,omitempty"`
	CPUBefore           *int64 `json:"cpuBefore,omitempty"`
	CPUAfter            *int64 `json:"cpuAfter,omitempty"`
}

type CoverageBracket struct {
	Diagnostics    *SignalDiagnostics `json:"signalDiagnostics,omitempty"`
	Worker         string             `json:"worker"`
	MidpointUnixNS string             `json:"midpointUnixNs"`
	BeforeUnixNS   string             `json:"beforeUnixNs"`
	AfterUnixNS    string             `json:"afterUnixNs"`
}

// CoverageQualification is the bounded, evaluated input to pinned judges.
// The historical replay opt-in belongs to the pool, not these measured results.
type CoverageQualification struct {
	Results            []CoverageResult `json:"results"`
	Falsified          bool             `json:"falsified"`
	ClockReceiptSHA256 string           `json:"clockReceiptSha256,omitempty"`
	Binding            *ClockBinding    `json:"binding,omitempty"`
}

type CoverageResult struct {
	Phase  int      `json:"phase"`
	Event  int      `json:"event"`
	Causes []string `json:"causes"`
}

func (q CoverageQualification) MarshalJSON() ([]byte, error) {
	if IsBound(q.ClockReceiptSHA256, q.Binding) && (q.Binding == nil || !q.Binding.Valid(q.ClockReceiptSHA256)) {
		q.Binding = nil
		q.ClockReceiptSHA256 = "unavailable"
	}
	type wire CoverageQualification
	return json.Marshal(wire(q))
}

type coverageSpan struct {
	slot              string
	a, b, bound, half int64
	signal            bool
}

func CoverageNS(s string) (int64, bool) {
	if len(s) != 19 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n >= 2*coverageSecond && n <= math.MaxInt64-2*coverageSecond
}
func coverageSlot(s string) bool { return s == "relay" || s == "shared" || s == "w1" || s == "w2" }

func coverageSpans(w CoverageWindow, control string, cb, wb int64, brackets []CoverageBracket, allowMissingBracket bool) ([]coverageSpan, string) {
	a, okA := CoverageNS(w.StartUnixNS)
	b, okB := CoverageNS(w.EndUnixNS)
	if !okA || !okB || b < a || !coverageSlot(control) || cb < 0 || wb < 0 || cb > coverageSecond || wb > coverageSecond {
		return nil, "clock-coverage-missing"
	}
	if w.Kind == "move" {
		return []coverageSpan{{slot: control, a: a, b: b, bound: cb}}, ""
	}
	if w.Kind != "takeover" && w.Kind != "kill" {
		return nil, "clock-coverage-missing"
	}
	if w.Worker != "w1" && w.Worker != "w2" {
		return nil, "clock-coverage-missing"
	}
	var found *CoverageBracket
	for i := range brackets {
		r := &brackets[i]
		if r.Worker == w.Worker && r.MidpointUnixNS == w.StartUnixNS {
			if found != nil && *found != *r {
				return nil, "clock-coverage-kill-bracket"
			}
			found = r
		}
	}
	if found == nil {
		if allowMissingBracket {
			return []coverageSpan{{slot: w.Worker, a: a, b: a, bound: wb}, {slot: control, a: b, b: b, bound: cb}}, ""
		}
		return nil, "clock-coverage-kill-bracket"
	}
	before, x := CoverageNS(found.BeforeUnixNS)
	after, y := CoverageNS(found.AfterUnixNS)
	if !x || !y || after < before || before+(after-before)/2 != a {
		return nil, "clock-coverage-kill-bracket"
	}
	return []coverageSpan{{slot: w.Worker, a: before, b: after, bound: wb, half: (after - before + 1) / 2, signal: true}, {slot: control, a: b, b: b, bound: cb}}, ""
}

// coverageError takes the maximum retrospective envelope over every intersecting
// interval, including touching boundaries and updates in the same log second.
func coverageError(t CoverageTimeline, a, b int64, padded bool) (int64, string) {
	rows := append([]CoverageRow(nil), t.Rows...)
	for _, r := range rows {
		if r.UnixSeconds < 0 || r.UnixSeconds > math.MaxInt64/coverageSecond-2 || math.IsNaN(r.MaxErrorSeconds) || math.IsInf(r.MaxErrorSeconds, 0) || r.MaxErrorSeconds < 0 {
			return 0, "clock-coverage-missing"
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].UnixSeconds < rows[j].UnixSeconds })
	a -= coverageSecond
	b += coverageSecond
	if len(rows) < 2 || rows[0].UnixSeconds*coverageSecond > a || rows[len(rows)-1].UnixSeconds*coverageSecond < b {
		return 0, "clock-coverage-missing"
	}
	for _, br := range t.Breaks {
		if br.FromUnixSeconds < 0 || br.UnixSeconds < br.FromUnixSeconds || br.UnixSeconds > math.MaxInt64/coverageSecond-2 {
			return 0, "clock-coverage-missing"
		}
		// The span already has ±1 s padding; padding the break by ±1 s
		// intentionally makes the maximum separation that overlaps ±2 s.
		if br.FromUnixSeconds*coverageSecond-coverageSecond <= b && br.UnixSeconds*coverageSecond+coverageSecond >= a {
			switch br.Kind {
			case "cursor-gap", "log-rotation", "log-loss":
				return 0, "clock-coverage-missing"
			default:
				return 0, "clock-coverage-break"
			}
		}
	}
	maximum := float64(0)
	paddedMaximum := new(big.Rat)
	if padded {
		for i := range rows {
			r := &rows[i]
			// Only intervals that touch the padded span contribute.
			if (i == 0 && r.UnixSeconds*coverageSecond != a) || (i > 0 && (rows[i-1].UnixSeconds*coverageSecond > b || r.UnixSeconds*coverageSecond < a)) {
				continue
			}
			value, err := ClockMaximumNS(r.MaxErrorDecimal)
			parsed, parseErr := strconv.ParseFloat(r.MaxErrorDecimal, 64)
			if err != nil || parseErr != nil || parsed != r.MaxErrorSeconds {
				return 0, "clock-coverage-missing"
			}
			if value.Cmp(paddedMaximum) > 0 {
				paddedMaximum.Set(value)
			}
		}
	}
	if rows[0].UnixSeconds*coverageSecond == a {
		maximum = rows[0].MaxErrorSeconds
	}
	for k := 1; k < len(rows); k++ {
		prev, row := rows[k-1], rows[k]
		if prev.UnixSeconds*coverageSecond > b || row.UnixSeconds*coverageSecond < a {
			continue
		}
		if row.UnixSeconds-prev.UnixSeconds > 60 {
			return 0, "clock-coverage-missing"
		}
		if prev.LeapStatus != "N" || row.LeapStatus != "N" {
			return 0, "clock-coverage-break"
		}
		maximum = max(maximum, row.MaxErrorSeconds)
	}
	// Recover the shortest decimal representation of the recorded chrony
	// value before scaling. Binary multiplication can turn 1.230e-04 into
	// 123000.00000000001 and incorrectly ceil an exact-ns envelope to 123001.
	decimal, ok := new(big.Rat).SetString(strconv.FormatFloat(maximum, 'f', -1, 64))
	if !ok {
		return 0, "clock-coverage-missing"
	}
	decimal.Mul(decimal, big.NewRat(coverageSecond, 1))
	if padded {
		decimal = paddedMaximum
	}
	ns, remainder := new(big.Int), new(big.Int)
	ns.QuoRem(decimal.Num(), decimal.Denom(), remainder)
	if remainder.Sign() > 0 {
		ns.Add(ns, big.NewInt(1))
	}
	if !ns.IsInt64() {
		return 0, "clock-coverage-missing"
	}
	return ns.Int64(), ""
}

// EvaluateClockCoverage reports would-be causes. It never changes verdicts or
// latches. Multiple receipts for one event contribute to the same sorted row.
func EvaluateClockCoverage(timelines map[string]CoverageTimeline, frontend, control string, cb, wb int64, windows []CoverageWindow, brackets []CoverageBracket) []CoverageResult {
	return evaluateClockCoverage(timelines, frontend, control, cb, wb, windows, brackets, false, 0, 0, false)
}

// EvaluateClockCoverageWithoutKillBrackets checks the available retrospective
// rules when historical evidence has no signal bracket. It does not prove the
// kill-bracket rule. timestampUncertaintyNS widens each endpoint to account for
// the bounded time-origin shift recovered from millisecond report timestamps.
func EvaluateClockCoverageWithoutKillBrackets(timelines map[string]CoverageTimeline, frontend, control string, cb, wb int64, windows []CoverageWindow, timestampUncertaintyNS int64) []CoverageResult {
	return evaluateClockCoverage(timelines, frontend, control, cb, wb, windows, nil, true, timestampUncertaintyNS, 0, false)
}

func EvaluateBoundClockCoverage(t map[string]CoverageTimeline, f, c string, cb, wb, k int64, w []CoverageWindow, b []CoverageBracket) []CoverageResult {
	return evaluateClockCoverage(t, f, c, cb, wb, w, b, false, 0, k, true)
}
func evaluateClockCoverage(timelines map[string]CoverageTimeline, frontend, control string, cb, wb int64, windows []CoverageWindow, brackets []CoverageBracket, allowMissingBracket bool, timestampUncertaintyNS, reserveNS int64, padded bool) []CoverageResult {
	type key struct{ p, e int }
	groups := map[key]map[string]bool{}
	for _, w := range windows {
		k := key{w.Phase, w.Event}
		if groups[k] == nil {
			groups[k] = map[string]bool{}
		}
		add := func(s string) {
			if s != "" {
				groups[k][s] = true
			}
		}
		spans, why := coverageSpans(w, control, cb, wb, brackets, allowMissingBracket)
		add(why)
		if timestampUncertaintyNS < 0 || timestampUncertaintyNS >= 1000000 {
			add("clock-coverage-missing")
			continue
		}
		if !coverageSlot(frontend) {
			add("clock-coverage-missing")
			continue
		}
		for _, s := range spans {
			if s.b > math.MaxInt64-coverageSecond-s.bound-timestampUncertaintyNS {
				add("clock-coverage-missing")
				continue
			}
			s.b += timestampUncertaintyNS
			h, whyH := coverageError(timelines[s.slot], s.a, s.b, padded)
			f, whyF := coverageError(timelines[frontend], s.a-s.bound, s.b+s.bound, padded)
			add(whyH)
			add(whyF)
			if whyH != "" || whyF != "" {
				continue
			}
			reserve := int64(0)
			if s.signal {
				reserve = reserveNS
			}
			if h > math.MaxInt64-f || reserve < 0 || reserve > s.bound || h+f > s.bound-reserve {
				add("clock-coverage-bound")
			}
			if s.signal && (h > math.MaxInt64-f || s.half > s.bound || h+f > s.bound-s.half) {
				add("clock-coverage-kill-bracket")
			}
		}
	}
	out := make([]CoverageResult, 0, len(groups))
	for k, causes := range groups {
		r := CoverageResult{Phase: k.p, Event: k.e, Causes: []string{}}
		for s := range causes {
			r.Causes = append(r.Causes, s)
		}
		sort.Strings(r.Causes)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Phase < out[j].Phase || out[i].Phase == out[j].Phase && out[i].Event < out[j].Event
	})
	return out
}

// CoverageTailReady concerns collection only: an observed break is permanent,
// but does not require waiting forever for healthy replacement evidence.
func CoverageTailReady(timelines map[string]CoverageTimeline, frontend, control string, cb, wb int64, windows []CoverageWindow, brackets []CoverageBracket) bool {
	if len(windows) == 0 {
		return true
	}
	if !coverageSlot(frontend) {
		return true
	} // Inventory cannot be repaired by a tracking read.
	required := map[string]int64{}
	for _, w := range windows {
		spans, why := coverageSpans(w, control, cb, wb, brackets, false)
		if why != "" {
			continue
		} // Missing receipts/brackets cannot be repaired by draining.
		for _, s := range spans {
			ceilSecond := func(n int64) int64 {
				q := n / coverageSecond
				if n%coverageSecond != 0 {
					q++
				}
				return q
			}
			required[s.slot] = max(required[s.slot], ceilSecond(s.b)+1)
			required[frontend] = max(required[frontend], ceilSecond(s.b+s.bound)+1)
		}
	}
	for slot, through := range required {
		t := timelines[slot]
		if !t.ReadComplete {
			return false
		}
		last := int64(0)
		for _, r := range t.Rows {
			last = max(last, r.UnixSeconds)
		}
		if last < through {
			return false
		}
	}
	return true
}

// IsCoverageCause accepts only the clock qualification's fixed public codes.
func IsCoverageCause(s string) bool {
	switch s {
	case "clock-coverage-missing", "clock-coverage-bound", "clock-coverage-break", "clock-coverage-kill-bracket", "clock-coverage-falsified", "clock-coverage-binding":
		return true
	}
	return false
}

// WindowCauses returns the clock qualification for one event. Nonempty causes
// make its windows undecided, while independent media failures remain failures.
func (q *CoverageQualification) WindowCauses(phase, event int, preCoverage bool) []string {
	expected := ""
	if q != nil {
		expected = q.ClockReceiptSHA256
	}
	return q.WindowCausesForReceipt(phase, event, preCoverage, expected)
}
func (q *CoverageQualification) WindowCausesForReceipt(phase, event int, preCoverage bool, expected string) []string {
	if expected != "" || q != nil && IsBound(q.ClockReceiptSHA256, q.Binding) {
		if preCoverage || q == nil || q.Binding == nil || q.ClockReceiptSHA256 != expected || !q.Binding.Valid(expected) {
			return []string{"clock-coverage-binding"}
		}
	}
	causes := []string{"clock-coverage-missing"}
	if q == nil {
		if preCoverage {
			return nil
		}
		return causes
	}
	for _, row := range q.Results {
		if row.Phase == phase && row.Event == event {
			causes = row.Causes
			break
		}
	}
	if q.Falsified {
		causes = append(append([]string{}, causes...), "clock-coverage-falsified")
	}
	return causes
}
