package observe

import (
	"math"
	"math/big"
	"sort"
	"strconv"
)

const coverageSecond int64 = 1e9

// Coverage types are numeric public diagnostics, independent of media policy.
// Absolute nanoseconds stay decimal strings; the evaluator never reads a clock.
type CoverageRow struct {
	UnixSeconds     int64   `json:"unixSeconds"`
	MaxErrorSeconds float64 `json:"maxErrorSeconds"`
	LeapStatus      string  `json:"leapStatus"`
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
type CoverageBracket struct {
	Worker         string `json:"worker"`
	MidpointUnixNS string `json:"midpointUnixNs"`
	BeforeUnixNS   string `json:"beforeUnixNs"`
	AfterUnixNS    string `json:"afterUnixNs"`
}
type CoverageResult struct {
	Phase  int      `json:"phase"`
	Event  int      `json:"event"`
	Causes []string `json:"causes"`
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
func coverageError(t CoverageTimeline, a, b int64) (int64, string) {
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
	return evaluateClockCoverage(timelines, frontend, control, cb, wb, windows, brackets, false, 0)
}

// EvaluateClockCoverageWithoutKillBrackets checks the available retrospective
// rules when historical evidence has no signal bracket. It does not prove the
// kill-bracket rule. timestampUncertaintyNS widens each endpoint to account for
// the bounded time-origin shift recovered from millisecond report timestamps.
func EvaluateClockCoverageWithoutKillBrackets(timelines map[string]CoverageTimeline, frontend, control string, cb, wb int64, windows []CoverageWindow, timestampUncertaintyNS int64) []CoverageResult {
	return evaluateClockCoverage(timelines, frontend, control, cb, wb, windows, nil, true, timestampUncertaintyNS)
}

func evaluateClockCoverage(timelines map[string]CoverageTimeline, frontend, control string, cb, wb int64, windows []CoverageWindow, brackets []CoverageBracket, allowMissingBracket bool, timestampUncertaintyNS int64) []CoverageResult {
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
			h, whyH := coverageError(timelines[s.slot], s.a, s.b)
			f, whyF := coverageError(timelines[frontend], s.a-s.bound, s.b+s.bound)
			add(whyH)
			add(whyF)
			if whyH != "" || whyF != "" {
				continue
			}
			if h > math.MaxInt64-f || h+f > s.bound {
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
