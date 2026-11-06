package clocks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/clockprobe"
	"github.com/stretchr/testify/require"
)

const probeSecond int64 = 1793930000

func probeSamples() map[string]Sample {
	samples := map[string]Sample{}
	for _, slot := range []string{"relay", "shared", "w1", "w2"} {
		log := &TrackingLog{}
		for n := int64(-3); n <= 3; n++ {
			log.Rows = append(log.Rows, TrackingRow{UnixSeconds: probeSecond + n, MaxErrorSeconds: 0.0005, LeapStatus: "N"})
		}
		samples[slot] = Sample{TrackingLog: log, ErrorBoundNS: 1, RateErrorPPM: 0.1, MeasuredUnixNS: "1793930000000000000"}
	}
	return samples
}
func probeRecord(offset int64) clockprobe.Record {
	base := probeSecond * secondNS
	return clockprobe.Record{Target: clockprobe.WorkerOne, FrontendSend: base + 100000000, FrontendReceive: base + 102000000, HostReceive: base + 101000000 + offset, HostSend: base + 101000000 + offset, Result: clockprobe.Success}
}
func probeBatch(t *testing.T, records ...clockprobe.Record) json.RawMessage {
	t.Helper()
	var lines strings.Builder
	for _, r := range records {
		line, err := r.MarshalLine()
		require.NoError(t, err)
		lines.Write(line)
	}
	raw, err := json.Marshal(ProbeBatch{Lines: lines.String(), Cursor: json.RawMessage(`{"file":"1:2","position":84,"anchor":"` + strings.Repeat("a", 64) + `","skipLine":false}`)})
	require.NoError(t, err)
	return raw
}

var probeTargets = map[string]string{"control": "shared", "w1": "w1", "w2": "w2"}

func TestFalsificationDisjointAndTouchingIntervals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset int64
		status string
	}{
		{"positive-disjoint", 10000000, "falsified"}, {"negative-disjoint", -10000000, "falsified"},
		{"positive-touch", 2000000, "not-falsified"}, {"negative-touch", -2000000, "not-falsified"},
		{"one-ns-outside", 2000001, "falsified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h ProbeHistory
			h.Collect(probeSamples(), "shared", probeBatch(t, probeRecord(tc.offset)))
			e := h.Snapshot("shared", probeTargets)
			require.Equal(t, tc.status, e.Records[0].Status)
			require.Equal(t, int64(1000000), *e.Records[0].PairErrorNS)
			require.Equal(t, 1, e.RTTHistogram.CountsByTarget["w1"][4])
		})
	}
}

func TestFalsificationLateClosingRowsAndCumulativeCounts(t *testing.T) {
	samples := probeSamples()
	for slot, s := range samples {
		s.TrackingLog.Rows = s.TrackingLog.Rows[:4]
		samples[slot] = s
	}
	var h ProbeHistory
	h.Collect(samples, "shared", probeBatch(t, probeRecord(10000000)))
	e := h.Snapshot("shared", probeTargets)
	require.Equal(t, "no-coverage", e.Records[0].Reason)
	require.Equal(t, 1, e.CountsByStatus["unchecked"])
	require.Equal(t, 1, h.Snapshot("shared", probeTargets).CountsByStatus["unchecked"])
	late := probeSamples()
	for slot, s := range late {
		s.TrackingLog.Rows = s.TrackingLog.Rows[4:]
		late[slot] = s
	}
	h.Collect(late, "shared", probeBatch(t))
	e = h.Snapshot("shared", probeTargets)
	require.Len(t, e.Records, 1)
	require.Equal(t, 1, e.CountsByStatus["falsified"])
	require.Zero(t, e.CountsByStatus["unchecked"])
}

func TestFalsificationUsesCoveringRowsIncludingEqualSeconds(t *testing.T) {
	samples := probeSamples()
	rows := samples["w1"].TrackingLog.Rows
	// Unrelated bounds must not widen this probe's bound.
	rows[0].MaxErrorSeconds, rows[len(rows)-1].MaxErrorSeconds = 100, 100
	samples["w1"].TrackingLog.Rows = append(rows, TrackingRow{UnixSeconds: probeSecond + 1, MaxErrorSeconds: 0.0020000001, LeapStatus: "N"})
	var h ProbeHistory
	h.Collect(samples, "shared", probeBatch(t, probeRecord(0)))
	e := h.Snapshot("shared", probeTargets)
	require.Equal(t, int64(2500001), *e.Records[0].PairErrorNS)
	// A zero-width span at the widened boundary still contributes its row.
	r := probeRecord(0)
	r.FrontendSend = probeSecond * secondNS
	r.FrontendReceive = r.FrontendSend
	r.HostReceive, r.HostSend = r.FrontendSend, r.FrontendSend
	samples = probeSamples()
	samples["w1"].TrackingLog.Rows = append(samples["w1"].TrackingLog.Rows, TrackingRow{UnixSeconds: probeSecond - 1, MaxErrorSeconds: 0.003, LeapStatus: "N"})
	h = ProbeHistory{}
	h.Collect(samples, "shared", probeBatch(t, r))
	require.Equal(t, int64(3500000), *h.Snapshot("shared", probeTargets).Records[0].PairErrorNS)
}

func TestFalsificationBreaksLeapsGapsAndMissingPredecessor(t *testing.T) {
	for _, cause := range []string{"break", "leap", "gap", "predecessor", "invalid-row"} {
		t.Run(cause, func(t *testing.T) {
			samples := probeSamples()
			log := samples["w1"].TrackingLog
			switch cause {
			case "break":
				log.Breaks = []TrackingBreak{{FromUnixSeconds: probeSecond, UnixSeconds: probeSecond, Kind: "clock-step"}}
			case "leap":
				log.Rows[4].LeapStatus = "+"
			case "gap":
				log.Rows = []TrackingRow{{UnixSeconds: probeSecond - 100, LeapStatus: "N"}, {UnixSeconds: probeSecond + 100, LeapStatus: "N"}}
			case "predecessor":
				log.Rows = log.Rows[4:]
			case "invalid-row":
				log.Rows[3].MaxErrorSeconds = -1
			}
			var h ProbeHistory
			h.Collect(samples, "shared", probeBatch(t, probeRecord(10000000)))
			r := h.Snapshot("shared", probeTargets).Records[0]
			require.Equal(t, "unchecked", r.Status)
			if cause == "break" || cause == "leap" {
				require.Equal(t, "tracking-break", r.Reason)
			} else {
				require.Equal(t, "no-coverage", r.Reason)
			}
		})
	}
}

func TestFalsificationInvalidAttemptsAndInventory(t *testing.T) {
	for _, cause := range []string{"request-failed", "zero", "invalid", "negative-rtt", "inventory"} {
		t.Run(cause, func(t *testing.T) {
			r := probeRecord(0)
			front := "shared"
			reason := "invalid-sample"
			switch cause {
			case "request-failed":
				r.Result = clockprobe.RequestFailed
				r.FrontendSend = 0
				reason = "request-failed"
			case "zero":
				r.HostSend = 0
			case "invalid":
				r.Result = clockprobe.InvalidSample
			case "negative-rtt":
				r.HostSend += 100000000
			case "inventory":
				front = ""
				reason = "inventory-unavailable"
			}
			var h ProbeHistory
			h.Collect(probeSamples(), "shared", probeBatch(t, r))
			e := h.Snapshot(front, probeTargets)
			require.Equal(t, "unchecked", e.Records[0].Status)
			require.Equal(t, reason, e.Records[0].Reason)
			if r.FrontendSend == 0 {
				require.Equal(t, strings.Repeat("0", 19), e.Records[0].FrontendSend)
			}
		})
	}
}

func TestFalsificationUsesTheMappedFrontendAndControl(t *testing.T) {
	samples := probeSamples()
	for i := range samples["w2"].TrackingLog.Rows {
		samples["w2"].TrackingLog.Rows[i].MaxErrorSeconds = 0.002
	}
	for i := range samples["relay"].TrackingLog.Rows {
		samples["relay"].TrackingLog.Rows[i].MaxErrorSeconds = 0.003
	}
	r := probeRecord(10000000)
	r.Target = clockprobe.Control
	var h ProbeHistory
	h.Collect(samples, "w2", probeBatch(t, r))
	e := h.Snapshot("w2", map[string]string{"control": "relay"})
	require.Equal(t, int64(5000000), *e.Records[0].PairErrorNS)
}

func TestProbeLossesQuotaAndOptionalDecodeIsolation(t *testing.T) {
	var h ProbeHistory
	for _, raw := range []json.RawMessage{nil, []byte(`null`), []byte(`{"lines":42}`), []byte(`{"lines":"x","lines":"y"}`)} {
		h.Collect(probeSamples(), "shared", raw)
	}
	require.Equal(t, 4, h.Snapshot("shared", probeTargets).LossCounts.ReadFailures)
	marker, err := clockprobe.ParseLine([]byte("3 " + strings.Repeat("0", 19) + " " + strings.Repeat("0", 19) + " " + strings.Repeat("0", 19) + " " + strings.Repeat("0", 19) + " 3\n"))
	require.NoError(t, err)
	h.Collect(nil, "shared", probeBatch(t, marker))
	require.Equal(t, 1, h.Snapshot("shared", probeTargets).LossCounts.CapacityEvents)
	for i := 0; i < ProbeRecordLimit+1; i += 500 {
		rows := make([]clockprobe.Record, min(500, ProbeRecordLimit+1-i))
		for n := range rows {
			rows[n] = probeRecord(int64(i + n))
		}
		h.Collect(nil, "shared", probeBatch(t, rows...))
	}
	e := h.Snapshot("shared", probeTargets)
	require.Len(t, e.Records, ProbeRecordLimit)
	require.Equal(t, 1, e.LossCounts.OmittedRecords)
	// Malformed optional members cannot alter the strict legacy fields.
	for _, extension := range []string{`"probeLog":false`, `"probeLog":{"lines":{},"lines":1}`, `"ProbeLog":{"private":"secret"}`} {
		data := []byte(`{"errorBoundNs":1,"rateErrorPpm":0.1,"measuredUnixNs":"1793930000000000000",` + extension + `}`)
		s, err := Parse(data)
		require.NoError(t, err)
		require.Equal(t, int64(1), s.ErrorBoundNS)
	}
	_, err = Parse([]byte(`{"errorBoundNs":1,"errorBoundNs":2,"rateErrorPpm":0.1,"measuredUnixNs":"1793930000000000000","probeLog":null}`))
	require.Error(t, err)
}

func TestFalsificationPublicProjectionDropsPrivateLabelsAndCursors(t *testing.T) {
	e := Falsification{FrontendSlot: "/private/source", Records: []ProbeRecord{{Target: "10.0.0.7", Reason: "/private/probes", Status: "secret", FrontendSend: "secret"}}, CountsByStatus: map[string]int{"secret": 1}, CountsByReason: map[string]int{"10.0.0.7": 1}, RTTHistogram: ProbeHistogram{CountsByTarget: map[string][]int{"secret": {1}}}}
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	for _, secret := range []string{"private", "secret", "10.0.0.7", `"cursor":`, `"anchor":`} {
		require.NotContains(t, string(raw), secret)
	}
	s := PublicSample(Sample{ProbeLog: json.RawMessage(`{"cursor":{"path":"secret"}}`)})
	raw, err = json.Marshal(s)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "secret")
}

func TestProbeRecordEmissionQuotaIncludesUpdates(t *testing.T) {
	var h ProbeHistory
	h.Collect(probeSamples(), "shared", probeBatch(t, probeRecord(10000000)))
	first := h.Snapshot("shared", probeTargets)
	require.Len(t, first.Records, 1)
	unchanged := h.Snapshot("shared", probeTargets)
	require.Empty(t, unchanged.Records)
	require.Equal(t, first.CountsByStatus, unchanged.CountsByStatus)
	h.emitted = ProbeRecordLimit
	h.breaks["w1"] = []TrackingBreak{{FromUnixSeconds: probeSecond, UnixSeconds: probeSecond, Kind: "clock-step"}}
	changed := h.Snapshot("shared", probeTargets)
	require.Empty(t, changed.Records)
	require.Equal(t, 1, changed.CountsByStatus["unchecked"])
	require.Equal(t, 1, changed.LossCounts.OmittedRecords)
	require.Equal(t, 1, h.Snapshot("shared", probeTargets).LossCounts.OmittedRecords)
	h.Collect(nil, "shared", probeBatch(t, probeRecord(123)))
	require.Equal(t, 2, h.Snapshot("shared", probeTargets).LossCounts.OmittedRecords)
}
