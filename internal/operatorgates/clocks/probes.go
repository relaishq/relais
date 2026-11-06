package clocks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/relaishq/relais/internal/clockprobe"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

const ProbeRecordLimit = 32768
const secondNS int64 = 1e9

type ProbeLossCounts struct {
	ReadFailures   int `json:"readFailures"`
	MalformedLines int `json:"malformedLines"`
	CursorGaps     int `json:"cursorGaps"`
	CapacityEvents int `json:"capacityEvents"`
	OmittedRecords int `json:"omittedRecords"`
}

type ProbeBatch struct {
	Lines      string          `json:"lines"`
	Cursor     json.RawMessage `json:"cursor"`
	LossCounts ProbeLossCounts `json:"lossCounts"`
}

type ProbeRecord struct {
	Target          string `json:"target"`
	FrontendSend    string `json:"fSendUnixNs"`
	HostReceive     string `json:"hRecvUnixNs"`
	HostSend        string `json:"hSendUnixNs"`
	FrontendReceive string `json:"fRecvUnixNs"`
	Status          string `json:"status"`
	Reason          string `json:"reason"`
	LowerNS         *int64 `json:"lowerNs,omitempty"`
	UpperNS         *int64 `json:"upperNs,omitempty"`
	PairErrorNS     *int64 `json:"pairErrorNs,omitempty"`
	RTTNS           *int64 `json:"rttNs,omitempty"`
}

var rttUpperNS = []int64{100000, 250000, 500000, 1000000, 2000000, 5000000, 10000000, 50000000}

type ProbeHistogram struct {
	UpperBoundsNS  []int64          `json:"upperBoundsNs"`
	CountsByTarget map[string][]int `json:"countsByTarget"`
}

// Falsification has cumulative counters and new or revised record details.
// Neither the counters nor the details are policy inputs.
type Falsification struct {
	FrontendSlot   string          `json:"frontendSlot"`
	Records        []ProbeRecord   `json:"records"`
	CountsByStatus map[string]int  `json:"countsByStatus"`
	CountsByReason map[string]int  `json:"countsByReason"`
	RTTHistogram   ProbeHistogram  `json:"rttHistogram"`
	LossCounts     ProbeLossCounts `json:"lossCounts"`
}

// ProbeHistory owns the session quota, including unresolved records. Tracking
// history uses the existing bounded clock refreshes; probes never change it.
type ProbeHistory struct {
	records  []clockprobe.Record
	versions []probeVersion
	emitted  int
	rows     map[string][]TrackingRow
	breaks   map[string][]TrackingBreak
	loss     ProbeLossCounts
}

type probeVersion struct {
	status, reason string
	bound          int64
	boundKnown     bool
}

func versionOf(r ProbeRecord) probeVersion {
	v := probeVersion{status: r.Status, reason: r.Reason, boundKnown: r.PairErrorNS != nil}
	if r.PairErrorNS != nil {
		v.bound = *r.PairErrorNS
	}
	return v
}

func (h *ProbeHistory) Collect(samples map[string]Sample, frontend string, raw json.RawMessage) json.RawMessage {
	if h.rows == nil {
		h.rows = map[string][]TrackingRow{}
		h.breaks = map[string][]TrackingBreak{}
	}
	for slot, sample := range samples {
		if sample.TrackingLog != nil {
			h.rows[slot] = append(h.rows[slot], sample.TrackingLog.Rows...)
			h.breaks[slot] = append(h.breaks[slot], sample.TrackingLog.Breaks...)
		}
	}
	// No frontend means no probe read was attempted. Keep tracking history,
	// but do not report an unknown inventory or a skipped refresh as I/O loss.
	if frontend == "" {
		return nil
	}
	var batch ProbeBatch
	if len(raw) == 0 || observe.Decode(raw, &batch, observe.Contract{"lines", "cursor?", "lossCounts", "lossCounts.readFailures", "lossCounts.malformedLines", "lossCounts.cursorGaps", "lossCounts.capacityEvents", "lossCounts.omittedRecords"}) != nil || len(batch.Lines) > 65536 || (batch.Lines != "" && !strings.HasSuffix(batch.Lines, "\n")) || !batch.LossCounts.valid() || !validProbeCursor(batch.Cursor, batch.Lines != "") {
		h.loss.ReadFailures++
		return nil
	}
	h.loss.add(batch.LossCounts)
	for _, line := range strings.SplitAfter(batch.Lines, "\n") {
		if line == "" {
			continue
		}
		r, err := clockprobe.ParseLine([]byte(line))
		if err != nil {
			h.loss.MalformedLines++
			continue
		}
		if r.Result == clockprobe.CapacityReached {
			h.loss.CapacityEvents++
			continue
		}
		if h.emitted+len(h.records)-len(h.versions) >= ProbeRecordLimit {
			h.loss.OmittedRecords++
			continue
		}
		h.records = append(h.records, r)
	}
	return batch.Cursor
}

func validProbeCursor(raw json.RawMessage, required bool) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return !required
	}
	var c struct {
		File     string `json:"file"`
		Position int64  `json:"position"`
		Anchor   string `json:"anchor"`
		SkipLine bool   `json:"skipLine"`
	}
	return json.Unmarshal(raw, &c) == nil && c.File != "" && c.Position >= 0 && c.Position <= clockprobe.MaxFileBytes && len(c.Anchor) == 64
}

func (l ProbeLossCounts) valid() bool {
	return l.ReadFailures >= 0 && l.MalformedLines >= 0 && l.CursorGaps >= 0 && l.CapacityEvents >= 0 && l.OmittedRecords >= 0 && l.ReadFailures <= 65536 && l.MalformedLines <= 65536 && l.CursorGaps <= 65536 && l.CapacityEvents <= 65536 && l.OmittedRecords <= 65536
}
func (l *ProbeLossCounts) add(v ProbeLossCounts) {
	l.ReadFailures += v.ReadFailures
	l.MalformedLines += v.MalformedLines
	l.CursorGaps += v.CursorGaps
	l.CapacityEvents += v.CapacityEvents
	l.OmittedRecords += v.OmittedRecords
}

type probeTimeline struct {
	rows   []TrackingRow
	breaks []TrackingBreak
	valid  bool
}

func (h *ProbeHistory) Snapshot(frontend string, targets map[string]string) *Falsification {
	e := &Falsification{FrontendSlot: frontend, Records: []ProbeRecord{}, CountsByStatus: map[string]int{}, CountsByReason: map[string]int{}, LossCounts: h.loss,
		RTTHistogram: ProbeHistogram{UpperBoundsNS: append([]int64{}, rttUpperNS...), CountsByTarget: map[string][]int{}}}
	if frontend == "" {
		e.FrontendSlot = "unavailable"
	}
	timelines := map[string]probeTimeline{}
	for slot, rows := range h.rows {
		t := probeTimeline{rows: append([]TrackingRow{}, rows...), breaks: h.breaks[slot], valid: true}
		for _, r := range rows {
			if r.UnixSeconds < 0 || r.UnixSeconds > math.MaxInt64/secondNS-2 || !finite(r.MaxErrorSeconds) || r.MaxErrorSeconds < 0 {
				t.valid = false
			}
		}
		// Backward updates carry a tracking break. Keep all equal-second rows.
		sort.SliceStable(t.rows, func(i, j int) bool { return t.rows[i].UnixSeconds < t.rows[j].UnixSeconds })
		timelines[slot] = t
	}
	for _, target := range []string{"control", "w1", "w2"} {
		e.RTTHistogram.CountsByTarget[target] = make([]int, len(rttUpperNS)+1)
	}
	for i, r := range h.records {
		row := checkProbe(r, frontend, targets[r.Target.String()], timelines)
		version := versionOf(row)
		changed := i >= len(h.versions) || h.versions[i] != version
		if i >= len(h.versions) {
			h.versions = append(h.versions, version)
		} else {
			h.versions[i] = version
		}
		if changed {
			if h.emitted < ProbeRecordLimit {
				e.Records = append(e.Records, row)
				h.emitted++
			} else {
				h.loss.OmittedRecords++
			}
		}
		e.CountsByStatus[row.Status]++
		e.CountsByReason[row.Reason]++
		if row.RTTNS != nil {
			i := sort.Search(len(rttUpperNS), func(i int) bool { return *row.RTTNS <= rttUpperNS[i] })
			e.RTTHistogram.CountsByTarget[row.Target][i]++
		}
	}
	e.LossCounts = h.loss
	return e
}

func checkProbe(r clockprobe.Record, frontend, target string, timelines map[string]probeTimeline) ProbeRecord {
	e := ProbeRecord{Target: r.Target.String(), FrontendSend: fmtNS(r.FrontendSend), HostReceive: fmtNS(r.HostReceive), HostSend: fmtNS(r.HostSend), FrontendReceive: fmtNS(r.FrontendReceive), Status: "unchecked", Reason: "invalid-sample"}
	if r.Result == clockprobe.RequestFailed {
		e.Reason = "request-failed"
		return e
	}
	if r.Result != clockprobe.Success || r.FrontendSend <= 0 || r.HostReceive <= 0 || r.HostSend <= 0 || r.FrontendReceive <= 0 || r.FrontendSend > r.FrontendReceive || r.HostReceive > r.HostSend {
		return e
	}
	rtt := (r.FrontendReceive - r.FrontendSend) - (r.HostSend - r.HostReceive)
	if rtt < 0 {
		return e
	}
	e.RTTNS = &rtt
	lower, upper := r.HostSend-r.FrontendReceive, r.HostReceive-r.FrontendSend
	e.LowerNS, e.UpperNS = &lower, &upper
	if frontend == "" || target == "" {
		e.Reason = "inventory-unavailable"
		return e
	}
	f, whyF := timelines[frontend].cover(r.FrontendSend, r.FrontendReceive)
	t, whyT := timelines[target].cover(r.HostReceive, r.HostSend)
	if whyF != "" || whyT != "" {
		e.Reason = "no-coverage"
		if whyF == "tracking-break" || whyT == "tracking-break" {
			e.Reason = "tracking-break"
		}
		return e
	}
	if f > math.MaxInt64-t {
		return e
	}
	bound := f + t
	e.PairErrorNS = &bound
	e.Status, e.Reason = "not-falsified", "intersects"
	if lower > bound || upper < -bound {
		e.Status, e.Reason = "falsified", "disjoint"
	}
	return e
}

func fmtNS(n int64) string { return fmt.Sprintf("%019d", n) }

func (t probeTimeline) cover(a, b int64) (int64, string) {
	if !t.valid || len(t.rows) < 2 || a < secondNS || b > math.MaxInt64-secondNS {
		return 0, "no-coverage"
	}
	a, b = a-secondNS, b+secondNS
	if t.rows[0].UnixSeconds*secondNS > a || t.rows[len(t.rows)-1].UnixSeconds*secondNS < b {
		return 0, "no-coverage"
	}
	for _, br := range t.breaks {
		// Compare in seconds to avoid overflow on an untrusted break timestamp.
		if br.FromUnixSeconds > br.UnixSeconds || (br.FromUnixSeconds <= b/secondNS+1 && br.UnixSeconds >= (a+secondNS-1)/secondNS-1) {
			return 0, "tracking-break"
		}
	}
	maximum := float64(0)
	start := max(1, sort.Search(len(t.rows), func(i int) bool { return t.rows[i].UnixSeconds*secondNS >= a }))
	if t.rows[0].UnixSeconds*secondNS == a {
		maximum = t.rows[0].MaxErrorSeconds
	}
	for k := start; k < len(t.rows) && t.rows[k-1].UnixSeconds*secondNS <= b; k++ {
		prev, row := t.rows[k-1], t.rows[k]
		if row.UnixSeconds-prev.UnixSeconds > 60 {
			return 0, "no-coverage"
		}
		if row.LeapStatus != "N" || prev.LeapStatus != "N" {
			return 0, "tracking-break"
		}
		maximum = max(maximum, row.MaxErrorSeconds)
	}
	ns := math.Ceil(maximum * 1e9)
	if ns >= float64(math.MaxInt64) {
		return 0, "no-coverage"
	}
	return int64(ns), ""
}

// MarshalJSON is the shared public projection for reports and JSONL. It cannot
// serialize raw host payloads, private cursor members or unbounded labels.
func (e Falsification) MarshalJSON() ([]byte, error) {
	type wire Falsification
	allowed := func(s string, values ...string) bool {
		for _, value := range values {
			if s == value {
				return true
			}
		}
		return false
	}
	count := func(n int) int { return max(0, min(n, int(maxSafeInteger))) }
	out := wire{FrontendSlot: e.FrontendSlot, Records: []ProbeRecord{}, CountsByStatus: map[string]int{}, CountsByReason: map[string]int{}, RTTHistogram: ProbeHistogram{UpperBoundsNS: append([]int64{}, rttUpperNS...), CountsByTarget: map[string][]int{}}, LossCounts: e.LossCounts}
	if !allowed(out.FrontendSlot, "relay", "shared", "w1", "w2") {
		out.FrontendSlot = "unavailable"
	}
	for _, r := range e.Records[:min(len(e.Records), ProbeRecordLimit)] {
		if !allowed(r.Target, "control", "w1", "w2") {
			r.Target = "unavailable"
		}
		if !allowed(r.Status, "falsified", "not-falsified", "unchecked") {
			r.Status = "unchecked"
		}
		if !allowed(r.Reason, "disjoint", "intersects", "no-coverage", "tracking-break", "inventory-unavailable", "request-failed", "invalid-sample") {
			r.Reason = "invalid-sample"
		}
		for _, s := range []*string{&r.FrontendSend, &r.FrontendReceive, &r.HostReceive, &r.HostSend} {
			if !nsPattern.MatchString(*s) {
				*s = "unavailable"
			}
		}
		out.Records = append(out.Records, r)
	}
	for _, k := range []string{"falsified", "not-falsified", "unchecked"} {
		out.CountsByStatus[k] = count(e.CountsByStatus[k])
	}
	for _, k := range []string{"disjoint", "intersects", "no-coverage", "tracking-break", "inventory-unavailable", "request-failed", "invalid-sample"} {
		out.CountsByReason[k] = count(e.CountsByReason[k])
	}
	for _, k := range []string{"control", "w1", "w2"} {
		v := make([]int, len(rttUpperNS)+1)
		for i, n := range e.RTTHistogram.CountsByTarget[k][:min(len(e.RTTHistogram.CountsByTarget[k]), len(v))] {
			v[i] = count(n)
		}
		out.RTTHistogram.CountsByTarget[k] = v
	}
	l := &out.LossCounts
	l.ReadFailures, l.MalformedLines, l.CursorGaps, l.CapacityEvents, l.OmittedRecords = count(l.ReadFailures), count(l.MalformedLines), count(l.CursorGaps), count(l.CapacityEvents), count(l.OmittedRecords)
	return json.Marshal(out)
}

// stripProbeLog isolates optional decoding before the strict policy decoder.
// Non-probe members retain their original raw values and duplicate members.
func stripProbeLog(data []byte) ([]byte, json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return data, nil, nil
	}
	var baseline bytes.Buffer
	baseline.WriteByte('{')
	var probe json.RawMessage
	count, seen := 0, 0
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, nil, err
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return nil, nil, err
		}
		name := key.(string)
		if strings.EqualFold(name, "probeLog") {
			seen++
			probe = raw
			if name != "probeLog" {
				seen++
			}
			continue
		}
		if count > 0 {
			baseline.WriteByte(',')
		}
		count++
		encoded, _ := json.Marshal(name)
		baseline.Write(encoded)
		baseline.WriteByte(':')
		baseline.Write(raw)
	}
	if _, err := d.Token(); err != nil {
		return nil, nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return data, nil, nil
	}
	baseline.WriteByte('}')
	if seen == 0 {
		return data, nil, nil
	}
	if seen > 1 {
		probe = json.RawMessage(`null`)
	}
	return baseline.Bytes(), probe, nil
}
