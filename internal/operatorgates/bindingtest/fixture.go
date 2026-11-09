// Package bindingtest supplies synthetic independent host evidence to gate tests.
package bindingtest

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/pkg/operator"
)

type Fixture struct {
	Input     clocks.SelectionEvidence
	Policy    clocks.SelectionPolicy
	Selection clocks.ClockSelection
	Artifacts operator.ClockBindingArtifacts
}

func New(run string, now time.Time) (Fixture, error) {
	start := now.Unix() - 125
	f := Fixture{Policy: clocks.SelectionPolicy{WindowSeconds: 120, FloorNS: 50000, ScaleNumerator: 2, ScaleDenominator: 1, AllowanceNS: 10000, SignalReserveNS: 50000, ControlCeilingNS: 250000, WorkerCeilingNS: 350000, MaxDispersionGrowthPPM: "5"}, Input: clocks.SelectionEvidence{RunID: run, FrontendSlot: "shared", ControlSlot: "shared", CallerLeapMode: "unsmeared", StartUnixSeconds: start, EndUnixSeconds: start + 120}}
	for _, slot := range []string{"relay", "w1", "w2", "shared"} {
		h := clocks.SelectionHost{Slot: slot, LeapMode: "unsmeared", Lease: Observation(time.Unix(start+122, 0)), Tracking: clocks.TrackingLog{ReadComplete: true}}
		for at := start - 2; at <= start+121; at++ {
			h.Tracking.Rows = append(h.Tracking.Rows, clocks.TrackingRow{BeforeRun: true, UnixSeconds: at, Reference: "test-source", MaxErrorSeconds: 1e-6, MaxErrorDecimal: "1.000e-06", LeapStatus: "N"})
		}
		f.Input.Hosts = append(f.Input.Hosts, h)
	}
	f.Input.Falsification = clocks.Falsification{FrontendSlot: "shared", CountsByStatus: map[string]int{}, CountsByReason: map[string]int{}}
	for at := start; at < start+120; at++ {
		for _, target := range []string{"control", "w1", "w2"} {
			n := at * 1e9
			f.Input.Falsification.Records = append(f.Input.Falsification.Records, clocks.ProbeRecord{Target: target, FrontendSend: fmt.Sprint(n), HostReceive: fmt.Sprint(n + 1000), HostSend: fmt.Sprint(n + 1000), FrontendReceive: fmt.Sprint(n + 2000), Status: "not-falsified"})
		}
	}
	var err error
	f.Selection, err = clocks.Select(f.Input, f.Policy)
	if err != nil {
		return f, err
	}
	r, _ := json.Marshal(f.Selection.Receipt)
	e, _ := json.Marshal(f.Input)
	f.Artifacts, err = operator.PackClockBindingArtifacts(r, e)
	return f, err
}
func Observation(now time.Time) clocks.LeaseObservation {
	return clocks.LeaseObservation{Available: true, ErrorBoundNS: 1000, MeasuredUnixNS: fmt.Sprint(now.UnixNano()), ReferenceUnixNS: fmt.Sprint(now.Add(-100 * time.Millisecond).UnixNano()), ReferenceAgeNS: 100000000, PollSeconds: "1", ResidualPPM: "0", SkewPPM: "0", DispersionStartSeconds: "0.000001000", DispersionEndSeconds: "0.000001001", IntervalNS: 20000000, HostElapsedNS: 20000000}
}

var historical = regexp.MustCompile(`historical_tracking\(json.loads\(base64.b64decode\('[^']*'\)\), ([0-9]+), ([0-9]+), ([0-9]+)\)`)

func (f Fixture) Wire(script string, now time.Time) string {
	log := f.Input.Hosts[0].Tracking
	if match := historical.FindStringSubmatch(script); match != nil {
		first, last := clocks.FrozenTrackingRange(log.Rows, f.Input.StartUnixSeconds, f.Input.EndUnixSeconds)
		if match[2] != fmt.Sprint(first) || match[3] != fmt.Sprint(last) {
			return Pack(map[string]string{"cause": "clock-history-incomplete"})
		}
		log.RunStartUnixMS, _ = strconv.ParseInt(match[1], 10, 64)
		return Pack(log)
	}
	log.Rows = nil
	latest := f.Input.Hosts[0].Tracking.Rows[len(f.Input.Hosts[0].Tracking.Rows)-1]
	latest.UnixSeconds = now.Add(-2 * time.Second).Unix()
	log.Cursor, _ = json.Marshal(map[string]any{"lastRow": latest, "position": 1})
	o := Observation(now)
	return Pack(clocks.Sample{MeasuredUnixNS: o.MeasuredUnixNS, Lease: &o, TrackingLog: &log})
}
func Pack(v any) string {
	raw, _ := json.Marshal(v)
	var out bytes.Buffer
	z := gzip.NewWriter(&out)
	_, _ = z.Write(raw)
	_ = z.Close()
	return base64.StdEncoding.EncodeToString(out.Bytes())
}
