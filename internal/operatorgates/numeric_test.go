package operatorgates

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/pkg/operator"
)

type numericFixtureJudge struct{ err error }

func (j numericFixtureJudge) Judge(context.Context, operatorevidence.Pool) (operatorevidence.Summary, error) {
	return operatorevidence.Summary{Status: operator.Inconclusive, UndecidedMetrics: []operatorevidence.UndecidedMetric{{Metric: "first_content", Causes: []string{"duration-bounds-uncertain"}}}}, j.err
}

type numericFailureWriter struct{}

func (numericFailureWriter) Write([]byte) (int, error) { return 0, errors.New("write-failed") }

func TestMediaRefreshTimingRecordsDurationAndFixedReason(t *testing.T) {
	var output bytes.Buffer
	r := &TimingRecorder{Writer: &output}
	r.RecordMediaRefresh(operator.Load, 250*time.Millisecond, true)
	want := "{\"type\":\"gate-media-refresh\",\"gate\":\"load\",\"durationNs\":250000000,\"reason\":\"media-refresh-unavailable\"}\n"
	if output.String() != want || r.Err() != nil {
		t.Fatal("MUTATION: gate-media-refresh-duration", output.String(), r.Err())
	}
}

func TestNumericTimingRecordingCannotChangeJudgeVerdictsOrReasons(t *testing.T) {
	for _, err := range []error{nil, errors.New("judge-incomplete")} {
		judge := numericFixtureJudge{err: err}
		want, wantErr := judge.Judge(context.Background(), operatorevidence.Pool{})
		recorder := &TimingRecorder{Writer: numericFailureWriter{}}
		got, gotErr := (timingJudge{MediaJudge: judge, recorder: recorder}).Judge(context.Background(), operatorevidence.Pool{})
		if got.Status != want.Status || got.UndecidedMetrics[0].Causes[0] != want.UndecidedMetrics[0].Causes[0] || gotErr != wantErr || recorder.Err() == nil {
			t.Fatal("diagnostic I/O altered judgment", got, gotErr, recorder.Err())
		}
	}
}

func TestNumericTimingRecorderCapsSnapshotsAndClockRefreshes(t *testing.T) {
	var output bytes.Buffer
	r := &TimingRecorder{Writer: &output}
	for i := range 20 {
		// A later snapshot has a different numeric witness, so retaining the
		// first sixteen snapshots cannot satisfy the final-state assertion.
		phases := make([]operatorevidence.MediaInput, i+1)
		for j := range phases {
			phases[j].CallerJSONL = "{"
		}
		r.RecordPool(operatorevidence.Pool{Phases: phases})
	}
	for range 260 {
		r.RecordClocks(clocks.Evidence{At: time.Unix(1, 0), Clocks: map[string]clocks.Sample{
			"relay":         {Tracking: "private-label", MeasuredUnixNS: "1767225600000000000", ErrorBoundNS: 1234, RateErrorPPM: 0.25},
			"private-label": {},
		}})
	}
	r.Finish()
	text := output.String()
	var finalSnapshot struct {
		Type     string                         `json:"type"`
		Snapshot int                            `json:"snapshot"`
		Final    bool                           `json:"final"`
		Evidence operatorevidence.NumericTiming `json:"evidence"`
	}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		var row struct {
			Type     string                         `json:"type"`
			Snapshot int                            `json:"snapshot"`
			Final    bool                           `json:"final"`
			Evidence operatorevidence.NumericTiming `json:"evidence"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row.Type == "numeric-timing" {
			finalSnapshot = row
		}
	}
	if !finalSnapshot.Final || finalSnapshot.Snapshot != 20 || finalSnapshot.Evidence.UnavailableStreams != 20 {
		t.Fatal("final cumulative snapshot was dropped", finalSnapshot)
	}
	if r.Err() != nil || strings.Count(text, `"type":"numeric-timing"`) != 16 || strings.Count(text, `"type":"host-clocks"`) != 256 || !strings.Contains(text, `"omitted_snapshots":4,"omitted_refreshes":4`) || strings.Contains(text, "private-label") || !strings.Contains(text, `"errorBoundNs":1234,"rateErrorPpm":0.25`) {
		t.Fatal("numeric retention or redaction failed", r.Err())
	}
}

func TestClockRecorderRetainsTrackingTimeline(t *testing.T) {
	var out bytes.Buffer
	r := TimingRecorder{Writer: &out}
	r.RecordClocks(clocks.Evidence{Version: clocks.EvidenceVersion, Clocks: map[string]clocks.Sample{"relay": {TrackingLog: &clocks.TrackingLog{
		Cursor: json.RawMessage(`{"position":123,"anchor":"abc"}`),
		Rows:   []clocks.TrackingRow{{UnixSeconds: 1, Reference: "private-label", MaxErrorSeconds: 0.000009, LeapStatus: "N"}},
		Breaks: []clocks.TrackingBreak{{Kind: "self-consistency", UnixSeconds: 2}},
	}}}})
	var got struct {
		Evidence clocks.Evidence `json:"evidence"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	log := got.Evidence.Clocks["relay"].TrackingLog
	if r.Err() != nil || log == nil || len(log.Rows) != 1 || len(log.Breaks) != 1 || strings.Contains(out.String(), "private-label") || got.Evidence.Version != 2 {
		t.Fatal("lost recorded timeline", out.String(), r.Err())
	}
}

func TestLeaseObservationDiagnostics(t *testing.T) {
	var output bytes.Buffer
	recorder := TimingRecorder{Writer: &output}
	o := clocks.LeaseObservation{Available: true, ErrorBoundNS: 1000, MeasuredUnixNS: "1793930000100000000", ReferenceUnixNS: "1793930000000000000", ReferenceAgeNS: 100000000, PollSeconds: "1", ResidualPPM: "0", SkewPPM: "0", DispersionStartSeconds: "0.000001000", DispersionEndSeconds: "0.000001001", IntervalNS: 20000000, HostElapsedNS: 50000000}
	read := LeaseRead{Evidence: clocks.Evidence{Version: 2, Clocks: map[string]clocks.Sample{"relay": {Lease: &o, MeasuredUnixNS: o.MeasuredUnixNS, Tracking: "private-reference", TrackingLog: &clocks.TrackingLog{ReadComplete: true, Rows: []clocks.TrackingRow{{Reference: "private-reference", LeapStatus: "N"}}}}}}, Sent: map[string]time.Duration{"relay": time.Second, "private-reference": 2 * time.Second}}
	recorder.RecordLeaseRead(read, 2300*time.Millisecond)
	var got struct {
		Type       string           `json:"type"`
		SentNS     map[string]int64 `json:"sentNs"`
		ReceivedNS int64            `json:"receivedNs"`
		Evidence   clocks.Evidence  `json:"evidence"`
	}
	if json.Unmarshal(output.Bytes(), &got) != nil || got.Type != "clock-lease-observation" || got.SentNS["relay"] != int64(time.Second) || len(got.SentNS) != 1 || got.ReceivedNS != int64(2300*time.Millisecond) || got.Evidence.Clocks["relay"].Lease == nil || got.Evidence.Clocks["relay"].Lease.ReferenceAgeNS != 100000000 || strings.Contains(output.String(), "private-reference") {
		t.Fatal("MUTATION: lease-observation-diagnostic", output.String())
	}
}

func TestLeaseObservationLimit(t *testing.T) {
	var output bytes.Buffer
	recorder := TimingRecorder{Writer: &output}
	for i := 0; i < 257; i++ {
		recorder.RecordLeaseRead(LeaseRead{}, 0)
	}
	recorder.RecordClocks(clocks.Evidence{})
	recorder.Finish()
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	var final struct {
		Omitted int `json:"omitted_refreshes"`
	}
	if len(lines) != 257 || json.Unmarshal([]byte(lines[len(lines)-1]), &final) != nil || final.Omitted != 2 {
		t.Fatal("MUTATION: lease-observation-limit", len(lines), final.Omitted)
	}
}
