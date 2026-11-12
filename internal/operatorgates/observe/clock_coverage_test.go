package observe

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"testing"
)

const coverageBase int64 = 1793930000

func coverageStamp(seconds float64) string {
	return fmt.Sprintf("%019d", coverageBase*coverageSecond+int64(seconds*1e9))
}
func coverageFixture() (map[string]CoverageTimeline, []CoverageWindow) {
	timelines := map[string]CoverageTimeline{}
	for _, slot := range []string{"shared", "relay", "w1", "w2"} {
		r := CoverageTimeline{ReadComplete: true}
		for n := int64(0); n <= 35; n++ {
			r.Rows = append(r.Rows, CoverageRow{UnixSeconds: coverageBase + n, MaxErrorSeconds: .001, LeapStatus: "N"})
		}
		timelines[slot] = r
	}
	return timelines, []CoverageWindow{{Phase: 0, Event: 0, Kind: "move", Worker: "w1", StartUnixNS: coverageStamp(10.25), EndUnixNS: coverageStamp(20.25)}}
}
func TestClockCoverage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]CoverageTimeline, []CoverageWindow)
		want   []string
	}{
		{"covered", func(map[string]CoverageTimeline, []CoverageWindow) {}, []string{}},
		{"bound-equality", func(map[string]CoverageTimeline, []CoverageWindow) {}, []string{}},
		{"max-over-span", func(m map[string]CoverageTimeline, _ []CoverageWindow) { m["shared"].Rows[15].MaxErrorSeconds = .0011 }, []string{"clock-coverage-bound"}},
		{"omit-frontend-error", func(m map[string]CoverageTimeline, _ []CoverageWindow) { m["relay"].Rows[15].MaxErrorSeconds = .0011 }, []string{"clock-coverage-bound"}},
		{"upward-rounding", func(m map[string]CoverageTimeline, _ []CoverageWindow) {
			m["relay"].Rows[15].MaxErrorSeconds = .00100000001
		}, []string{"clock-coverage-bound"}},
		{"equal-second", func(m map[string]CoverageTimeline, _ []CoverageWindow) {
			r := m["shared"]
			r.Rows = append(r.Rows, CoverageRow{UnixSeconds: coverageBase + 15, MaxErrorSeconds: .002, LeapStatus: "N"})
			m["shared"] = r
		}, []string{"clock-coverage-bound"}},
		{"gap", func(m map[string]CoverageTimeline, _ []CoverageWindow) {
			r := m["shared"]
			r.Rows = []CoverageRow{{UnixSeconds: coverageBase, LeapStatus: "N"}, {UnixSeconds: coverageBase + 100, LeapStatus: "N"}}
			m["shared"] = r
		}, []string{"clock-coverage-missing"}},
		{"omit-final-coverage", func(m map[string]CoverageTimeline, _ []CoverageWindow) {
			r := m["shared"]
			r.Rows = r.Rows[:21]
			m["shared"] = r
		}, []string{"clock-coverage-missing"}},
		{"span-interior-break", func(m map[string]CoverageTimeline, _ []CoverageWindow) {
			r := m["shared"]
			r.Breaks = []CoverageBreak{{FromUnixSeconds: coverageBase + 15, UnixSeconds: coverageBase + 15, Kind: "source-change"}}
			m["shared"] = r
		}, []string{"clock-coverage-break"}},
		{"break-margin", func(m map[string]CoverageTimeline, _ []CoverageWindow) {
			r := m["shared"]
			r.Breaks = []CoverageBreak{{FromUnixSeconds: coverageBase + 22, UnixSeconds: coverageBase + 22, Kind: "clock-step"}}
			m["shared"] = r
		}, []string{"clock-coverage-break"}},
		{"outside-break-margin", func(m map[string]CoverageTimeline, _ []CoverageWindow) {
			r := m["shared"]
			r.Breaks = []CoverageBreak{{FromUnixSeconds: coverageBase + 23, UnixSeconds: coverageBase + 23, Kind: "clock-step"}}
			m["shared"] = r
		}, []string{}},
		{"repair-gap-with-later-sample", func(m map[string]CoverageTimeline, _ []CoverageWindow) {
			r := m["shared"]
			r.Breaks = []CoverageBreak{{FromUnixSeconds: coverageBase + 15, UnixSeconds: coverageBase + 15, Kind: "cursor-gap"}}
			m["shared"] = r
		}, []string{"clock-coverage-missing"}},
		{"leap", func(m map[string]CoverageTimeline, _ []CoverageWindow) { m["shared"].Rows[15].LeapStatus = "+" }, []string{"clock-coverage-break"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, w := coverageFixture()
			tc.change(m, w)
			got := EvaluateClockCoverage(m, "relay", "shared", 2000000, 3000000, w, nil)
			if len(got) != 1 || !reflect.DeepEqual(got[0].Causes, tc.want) {
				t.Fatalf("MUTATION: clock-coverage-%s: %+v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
func TestClockCoverageKillBracket(t *testing.T) {
	for _, width := range []int64{2000000, 2000001} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m, w := coverageFixture()
			before := coverageBase*coverageSecond + 10250000000
			after := before + width
			mid := before + width/2
			w[0].Kind = "takeover"
			w[0].StartUnixNS = fmt.Sprint(mid)
			b := []CoverageBracket{{Worker: "w1", BeforeUnixNS: fmt.Sprint(before), AfterUnixNS: fmt.Sprint(after), MidpointUnixNS: fmt.Sprint(mid)}}
			got := EvaluateClockCoverage(m, "relay", "shared", 2000000, 3000000, w, b)
			want := []string{}
			if width > 2000000 {
				want = []string{"clock-coverage-kill-bracket"}
			}
			if !reflect.DeepEqual(got[0].Causes, want) {
				t.Fatal(got, want)
			}
		})
	}
}

// Only a well-formed bracket the gate marked wide gets the excluded cause.
func TestClockCoverageKillExcluded(t *testing.T) {
	for _, tc := range []struct{ name, want string }{{"well-formed", "clock-coverage-kill-excluded"}, {"reversed", "clock-coverage-kill-bracket"}, {"malformed", "clock-coverage-kill-bracket"}, {"midpoint", "clock-coverage-kill-bracket"}, {"duplicate", "clock-coverage-kill-bracket"}} {
		t.Run(tc.name, func(t *testing.T) {
			m, w := coverageFixture()
			before := coverageBase*coverageSecond + 10250000000
			after := before + 5000000
			mid := before + 2500000
			w[0].Kind = "takeover"
			w[0].StartUnixNS = fmt.Sprint(mid)
			b := []CoverageBracket{{LeaseExcluded: true, Worker: "w1", BeforeUnixNS: fmt.Sprint(before), AfterUnixNS: fmt.Sprint(after), MidpointUnixNS: fmt.Sprint(mid)}}
			switch tc.name {
			case "reversed":
				b[0].BeforeUnixNS, b[0].AfterUnixNS = b[0].AfterUnixNS, b[0].BeforeUnixNS
			case "malformed":
				b[0].AfterUnixNS = "bad"
			case "midpoint":
				b[0].BeforeUnixNS = fmt.Sprint(before + 2)
			case "duplicate":
				b = append(b, b[0])
				b[1].AfterUnixNS = fmt.Sprint(after + 2)
			}
			got := EvaluateClockCoverage(m, "relay", "shared", 2000000, 3000000, w, b)
			if !reflect.DeepEqual(got[0].Causes, []string{tc.want}) {
				t.Fatal("MUTATION: malformed-bracket-excluded", tc.name, got, tc.want)
			}
		})
	}
}

// The evaluator rechecks a gate-excluded bracket: it stays excluded only when
// the 2 ms clock error plus its half-width exceeds the 3 ms worker bound. A
// 4 ms bracket's 2 ms half-width alone fits; with the clock error it does not.
func TestClockCoverageKillExcludedRecheck(t *testing.T) {
	for _, tc := range []struct {
		width int64
		want  []string
	}{{2000000, []string{}}, {2000001, []string{"clock-coverage-kill-excluded"}}, {4000000, []string{"clock-coverage-kill-excluded"}}} {
		t.Run(fmt.Sprint(tc.width), func(t *testing.T) {
			m, w := coverageFixture()
			before := coverageBase*coverageSecond + 10250000000
			mid := before + tc.width/2
			w[0].Kind, w[0].StartUnixNS = "takeover", fmt.Sprint(mid)
			b := []CoverageBracket{{LeaseExcluded: true, Worker: "w1", BeforeUnixNS: fmt.Sprint(before), AfterUnixNS: fmt.Sprint(before + tc.width), MidpointUnixNS: fmt.Sprint(mid)}}
			got := EvaluateClockCoverage(m, "relay", "shared", 2000000, 3000000, w, b)
			if !reflect.DeepEqual(got[0].Causes, tc.want) {
				t.Fatal("MUTATION: excluded-bracket-trusts-gate", tc.width, got[0].Causes, tc.want)
			}
		})
	}
}

// An excluded bracket waives only its own width. Every timeline check of the
// takeover still runs, so an independent clock cause keeps the event undecided.
func TestClockCoverageKillExcludedKeepsTimelineChecks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]CoverageTimeline)
		want   []string
	}{
		{"clean", func(map[string]CoverageTimeline) {}, []string{"clock-coverage-kill-excluded"}},
		{"worker-step", func(m map[string]CoverageTimeline) {
			r := m["w1"]
			r.Breaks = []CoverageBreak{{FromUnixSeconds: coverageBase + 10, UnixSeconds: coverageBase + 10, Kind: "clock-step"}}
			m["w1"] = r
		}, []string{"clock-coverage-break", "clock-coverage-kill-excluded"}},
		{"worker-missing", func(m map[string]CoverageTimeline) {
			r := m["w1"]
			r.Rows = r.Rows[:5]
			m["w1"] = r
		}, []string{"clock-coverage-kill-excluded", "clock-coverage-missing"}},
		{"control-bound", func(m map[string]CoverageTimeline) { m["shared"].Rows[20].MaxErrorSeconds = .0011 }, []string{"clock-coverage-bound", "clock-coverage-kill-excluded"}},
		{"frontend-bound", func(m map[string]CoverageTimeline) { m["relay"].Rows[20].MaxErrorSeconds = .0011 }, []string{"clock-coverage-bound", "clock-coverage-kill-excluded"}},
		{"worker-bound", func(m map[string]CoverageTimeline) { m["w1"].Rows[10].MaxErrorSeconds = .0021 }, []string{"clock-coverage-bound", "clock-coverage-kill-excluded"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, w := coverageFixture()
			// 5 ms wide: its 2.5 ms half-width alone exceeds the 3 ms worker bound
			// less the 2 ms clock error, which only the excluded bracket waives.
			before := coverageBase*coverageSecond + 10250000000
			mid := before + 2500000
			w[0].Kind, w[0].StartUnixNS = "takeover", fmt.Sprint(mid)
			b := []CoverageBracket{{LeaseExcluded: true, Worker: "w1", BeforeUnixNS: fmt.Sprint(before), AfterUnixNS: fmt.Sprint(before + 5000000), MidpointUnixNS: fmt.Sprint(mid)}}
			tc.change(m)
			got := EvaluateClockCoverage(m, "relay", "shared", 2000000, 3000000, w, b)
			if !reflect.DeepEqual(got[0].Causes, tc.want) {
				t.Fatal("MUTATION: excluded-bracket-skips-timelines", tc.name, got[0].Causes, tc.want)
			}
		})
	}
}

// Drainage waits for an excluded takeover's tail, as its timelines are judged.
func TestCoverageTailWaitsForExcludedKill(t *testing.T) {
	m, w := coverageFixture()
	before := coverageBase*coverageSecond + 10250000000
	mid := before + 2500000
	w[0].Kind, w[0].StartUnixNS = "takeover", fmt.Sprint(mid)
	b := []CoverageBracket{{LeaseExcluded: true, Worker: "w1", BeforeUnixNS: fmt.Sprint(before), AfterUnixNS: fmt.Sprint(before + 5000000), MidpointUnixNS: fmt.Sprint(mid)}}
	r := m["shared"]
	r.Rows = r.Rows[:21] // control's tail ends before the window's end plus margin
	m["shared"] = r
	if CoverageTailReady(m, "relay", "shared", 2000000, 3000000, w, b) {
		t.Fatal("MUTATION: excluded-tail-skipped")
	}
}
func TestClockCoverageRelocationAndDeterminism(t *testing.T) {
	m, w := coverageFixture()
	m["relay"].Rows[15].MaxErrorSeconds = 1
	// Signal and control both on shared; relay is unrelated.
	baseline := EvaluateClockCoverage(m, "shared", "shared", 2000000, 3000000, w, nil)
	if len(baseline[0].Causes) != 0 {
		t.Fatal(baseline)
	}
	expected, _ := json.Marshal(baseline)
	for n := 0; n < 20; n++ {
		r := m["shared"]
		for i, j := 0, len(r.Rows)-1; i < j; i, j = i+1, j-1 {
			r.Rows[i], r.Rows[j] = r.Rows[j], r.Rows[i]
		}
		m["shared"] = r
		got, _ := json.Marshal(EvaluateClockCoverage(m, "shared", "shared", 2000000, 3000000, w, nil))
		if string(got) != string(expected) {
			t.Fatal("nondeterministic coverage", string(got))
		}
	}
	if got := EvaluateClockCoverage(m, "shared", "relay", 2000000, 3000000, w, nil); len(got[0].Causes) == 0 {
		t.Fatal("relocated control error omitted")
	}
}
func TestCoverageTailRequiresClosingRowAndCompleteRead(t *testing.T) {
	m, w := coverageFixture()
	if !CoverageTailReady(m, "relay", "shared", 2000000, 3000000, w, nil) {
		t.Fatal("ready timeline rejected")
	}
	r := m["relay"]
	r.Rows = r.Rows[:22]
	m["relay"] = r
	if CoverageTailReady(m, "relay", "shared", 2000000, 3000000, w, nil) {
		t.Fatal("fractional closing second omitted")
	}
	r.Rows = append(r.Rows, CoverageRow{UnixSeconds: coverageBase + 22, LeapStatus: "N"})
	r.ReadComplete = false
	m["relay"] = r
	if CoverageTailReady(m, "relay", "shared", 2000000, 3000000, w, nil) {
		t.Fatal("cut-short read accepted")
	}
	r.ReadComplete = true
	m["relay"] = r
	if !CoverageTailReady(m, "relay", "shared", 2000000, 3000000, w, nil) {
		t.Fatal("closing row rejected")
	}
}

func TestClockCoverageDecimalNanosecondEquality(t *testing.T) {
	for _, tc := range []struct {
		decimal string
		want    int64
	}{{"1.230e-04", 123000}, {"2.460e-04", 246000}, {"1.230000000001e-04", 123001}} {
		t.Run(tc.decimal, func(t *testing.T) {
			value, err := strconv.ParseFloat(tc.decimal, 64)
			if err != nil {
				t.Fatal(err)
			}
			timelines, windows := coverageFixture()
			for slot, timeline := range timelines {
				for i := range timeline.Rows {
					timeline.Rows[i].MaxErrorSeconds = value
				}
				timelines[slot] = timeline
			}
			results := EvaluateClockCoverage(timelines, "relay", "shared", 2*tc.want, 3000000, windows, nil)
			if len(results[0].Causes) != 0 {
				t.Fatal("decimal bound equality failed", results)
			}
			results = EvaluateClockCoverage(timelines, "relay", "shared", 2*tc.want-1, 3000000, windows, nil)
			if !slices.Contains(results[0].Causes, "clock-coverage-bound") {
				t.Fatal("outward rounding lost a nanosecond", results)
			}
		})
	}
}
func TestCoverageTailIgnoresUndrainableWindows(t *testing.T) {
	timelines, windows := coverageFixture()
	bad := []CoverageWindow{{Event: 1, Kind: "move"}, {Event: 2, Kind: "takeover", Worker: "w1", StartUnixNS: coverageStamp(11), EndUnixNS: coverageStamp(12)}}
	if !CoverageTailReady(timelines, "relay", "shared", 2000000, 3000000, append(windows, bad...), nil) {
		t.Fatal("undrainable window kept a complete tail open")
	}
	results := EvaluateClockCoverage(timelines, "relay", "shared", 2000000, 3000000, bad, nil)
	if len(results[0].Causes) == 0 || len(results[1].Causes) == 0 {
		t.Fatal("undrainable errors disappeared", results)
	}
	if !CoverageTailReady(nil, "relay", "shared", 2000000, 3000000, bad, nil) {
		t.Fatal("bad-only window caused unnecessary reads")
	}
}
