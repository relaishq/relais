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
