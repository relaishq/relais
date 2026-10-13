package callharness

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/relais/pkg/controlplane"
)

// OperationIdentity is supplied by the operator feed, on arming and on the
// durable server receipt. Worker is the unique registration/generation name;
// Epoch identifies the ownership tenure affected, not its successor.
type OperationIdentity struct {
	RunID       string `json:"run_id"`
	OperationID string `json:"operation_id"`
	SessionID   string `json:"session_id"`
	Worker      string `json:"worker"`
	Epoch       uint64 `json:"epoch"`
}

func (i OperationIdentity) Valid() bool {
	return i.RunID != "" && i.OperationID != "" && i.SessionID != "" && i.Worker != "" && i.Epoch > 0
}

type DistributedOptions struct {
	RunID string
	Clock *ClockTracker
	// Host minus frontend offsets. These are measured input bounds, not a
	// claim that /clock observes control or worker time.
	ControlOffset OffsetInterval
	WorkerOffset  OffsetInterval
}

// DistributedEvent is a durable server receipt, never a command submission.
// Start is control-host move start or worker-host actual SIGKILL time. End is
// control-host completion. Uncertain preserves unknown operation outcomes.
type DistributedEvent struct {
	Identity  OperationIdentity       `json:"identity"`
	Kind      string                  `json:"kind"`
	Start     time.Time               `json:"start"`
	End       time.Time               `json:"end"`
	Result    controlplane.MoveResult `json:"result"`
	Uncertain bool                    `json:"uncertain,omitempty"`
}

type DurationInterval struct {
	Lower time.Duration `json:"lower_ns"`
	Upper time.Duration `json:"upper_ns"`
}

// DistributedMeasurement keeps raw caller gaps independent of event clocks.
// Union windows prove passes; intersection windows can prove failures. A
// failure seen only in the uncertainty margin is inconclusive.
type DistributedMeasurement struct {
	Durations      map[string]DurationInterval `json:"durations,omitempty"`
	Evaluations    int                         `json:"evaluations"`
	FirstBaselines []FirstBaselines            `json:"first_baselines,omitempty"`
	BaselineFloor  DurationInterval            `json:"baseline_floor"`

	Mapped bool `json:"mapped"`

	AudioLoss MetricVerdict `json:"audio_loss"`
	VideoLoss MetricVerdict `json:"video_loss"`

	WindowOutcome string `json:"window_outcome"`

	Start      TimeInterval      `json:"start"`
	End        TimeInterval      `json:"end"`
	KillToLive *DurationInterval `json:"kill_to_live,omitempty"`
	Gaps       []DistributedGap  `json:"gaps"`
	Outcome    string            `json:"outcome"`
	Causes     []string          `json:"causes,omitempty"`
}
type DistributedGap struct {
	BaselinePossible []time.Duration `json:"baseline_possible_ns"`
	BaselineCertain  []time.Duration `json:"baseline_certain_ns"`

	Kind        string           `json:"kind"`
	BaselineP95 DurationInterval `json:"baseline_p95"`
	EventGap    DurationInterval `json:"event_gap"`
	Outcome     string           `json:"outcome"`
	Causes      []string         `json:"causes,omitempty"`
}

func (c *Call) ArmDistributedEvent(id OperationIdentity) error {
	r := c.rec
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.distributed == nil || !id.Valid() || id.RunID != r.distributed.RunID || id.SessionID != c.SessionID() {
		return errors.New("invalid distributed arm identity")
	}
	if r.armed == nil {
		r.armed = map[OperationIdentity]time.Time{}
	}
	if _, ok := r.armed[id]; ok {
		return nil
	}
	if len(r.armed) >= 4096 {
		return errors.New("armed event capacity exceeded")
	}
	r.armed[id] = time.Now()
	return nil
}

func (c *Call) RecordDistributedEvent(e DistributedEvent) error {
	r := c.rec
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.distributed == nil || !e.Identity.Valid() || e.Identity.RunID != r.distributed.RunID || e.Identity.SessionID != c.SessionID() || (e.Kind != "move" && e.Kind != "takeover") {
		return errors.New("invalid distributed event identity or kind")
	}
	armed, ok := r.armed[e.Identity]
	if !ok {
		return errors.New("distributed event was not armed")
	}
	if e.Result.ID != "" && e.Result.ID != e.Identity.SessionID || e.Result.From != "" && e.Result.From != e.Identity.Worker || e.Result.Kind != "" && e.Result.Kind != e.Kind {
		return errors.New("server result differs from armed identity")
	}
	for _, m := range r.moves {
		if m.event != nil && m.event.Identity == e.Identity {
			if !reflect.DeepEqual(*m.event, e) {
				return errors.New("conflicting distributed receipt")
			}
			return nil
		}
	}
	for _, m := range r.savedMoves {
		if m.Identity != nil && *m.Identity == e.Identity {
			return errors.New("distributed receipt already finalized")
		}
	}
	from, err := strconv.Atoi(e.Identity.Worker)
	if err != nil {
		from = -1
	}
	to, err := strconv.Atoi(e.Result.To)
	if err != nil {
		to = -1
	}
	var moveErr error
	if e.Result.Error != "" {
		moveErr = errors.New(e.Result.Error)
	}
	// Clock validation happens again when the event is measured, so later
	// probes can invalidate earlier measurements too.
	r.moves = append(r.moves, moveRecord{event: &e, armedAt: armed, receivedAt: time.Now(), kind: e.Kind, start: e.Start, end: e.End, from: from, to: to, lost: e.Result.Lost, lossCause: e.Result.LossCause, result: e.Result.Result, err: moveErr})
	return nil
}

func distributedEventOrderingValid(move moveRecord, start, end TimeInterval) bool {
	e := move.event
	return !start.Earliest.Before(move.armedAt) && !start.Latest.After(move.receivedAt) && !end.Latest.After(move.receivedAt) &&
		(e.Kind != "takeover" || !end.Earliest.Before(start.Latest)) && (e.Kind != "move" || !e.End.Before(e.Start))
}

func (r *recorder) distributedMeasurement(move moveRecord, until time.Duration) *DistributedMeasurement {
	e := move.event
	d := &DistributedMeasurement{Outcome: "pass"}
	defer func() {
		if d.WindowOutcome == "" {
			d.WindowOutcome = d.Outcome
		}
	}()
	unknown := func(cause string) {
		if d.Outcome != "fail" {
			d.Outcome = "inconclusive"
		}
		d.Causes = append(d.Causes, cause)
	}
	fail := func(cause string) { d.Outcome = "fail"; d.Causes = append(d.Causes, cause) }
	if e.Result.Lost {
		fail("server reported call loss: " + e.Result.LossCause)
	}
	if e.Result.Error != "" || e.Uncertain {
		unknown("server operation failed or outcome uncertain")
	}
	host := r.distributed.ControlOffset
	if e.Kind == "takeover" {
		host = r.distributed.WorkerOffset
	}
	var err error
	d.Start, err = r.distributed.Clock.Map(e.Start, host)
	if err != nil {
		unknown(err.Error())
		return d
	}
	d.End, err = r.distributed.Clock.Map(e.End, r.distributed.ControlOffset)
	if err != nil {
		unknown(err.Error())
		return d
	}
	if !distributedEventOrderingValid(move, d.Start, d.End) {
		unknown("mapped event violates arm/receipt ordering or host clock bound")
		return d
	}
	lo, hi := r.since(d.Start.Earliest), r.since(d.Start.Latest)
	endLo, endHi := r.since(d.End.Earliest), r.since(d.End.Latest)
	if lo-baselineWindow < r.historyStart || endHi+contentWindowMargin+contentSettleWindow > until {
		unknown("mapped event history or settle window unavailable")
	}
	if len(r.tracks) == 0 {
		unknown("no caller media observations")
	}
	d.Mapped = true
	d.WindowOutcome = d.Outcome
	for _, track := range r.tracks {
		g := judgeDistributedGap(track.kind, track.arrivals, lo, hi, endLo, endHi, e.Kind)
		d.Gaps = append(d.Gaps, g)
		if g.Outcome == "fail" {
			fail(track.kind + ": " + fmt.Sprint(g.Causes))
		} else if g.Outcome != "pass" {
			if len(g.BaselineCertain) < baselineMinSamples {
				d.WindowOutcome = "inconclusive"
			}
			unknown(track.kind + ": " + fmt.Sprint(g.Causes))
		}
	}
	// A union of candidate content windows proves no loss. A missing send
	// inside their intersection proves loss independent of event attribution.
	resumeUpper := endHi
	kinds := map[string]bool{}
	for _, track := range r.tracks {
		kinds[track.kind] = true
	}
	for kind := range kinds {
		first := time.Duration(0)
		observe := func(u contentUnit) {
			if u.written && u.at >= endHi && u.returnedAt >= u.at && u.returnedAt <= until && (first == 0 || u.returnedAt < first) {
				first = u.returnedAt
			}
		}
		if kind == kindAudio {
			for _, u := range r.sentAudioUnits {
				observe(u)
			}
		} else {
			for _, f := range r.sentVideoFrames {
				observe(f.unit())
			}
		}
		if first == 0 {
			// A delayed packet from the old owner does not prove new media
			// resumed. Without identified post-completion content, its upper
			// bound is the observation horizon and loss cannot falsely pass.
			resumeUpper = until
		} else {
			resumeUpper = max(resumeUpper, first)
		}
	}
	d.AudioLoss = r.intervalContentLoss(kindAudio, lo, hi, endLo, resumeUpper, until)
	d.VideoLoss = r.intervalContentLoss(kindVideo, lo, hi, endLo, resumeUpper, until)
	for _, pair := range []struct {
		kind string
		v    MetricVerdict
	}{{kindAudio, d.AudioLoss}, {kindVideo, d.VideoLoss}} {
		if pair.v.Trusted && pair.v.Failed {
			fail(pair.kind + ": caller content loss across every event window")
			d.WindowOutcome = "fail"
		} else if !pair.v.Trusted {
			unknown(pair.kind + ": content loss not proven across clock interval")
			if d.WindowOutcome != "fail" {
				d.WindowOutcome = "inconclusive"
			}
		}
	}
	if e.Kind == "takeover" {
		firstLo, firstHi := time.Duration(0), time.Duration(0)
		observe := func(sent, returned time.Duration) {
			if returned <= 0 || returned > until {
				return
			}
			if sent >= lo && (firstLo == 0 || returned < firstLo) {
				firstLo = returned
			}
			if sent >= resumeUpper && (firstHi == 0 || returned < firstHi) {
				firstHi = returned
			}
		}
		if r.sentVideo.Frames > 0 || r.sentVideo.SSRC != 0 {
			for _, track := range r.tracks {
				if track.video != nil {
					for _, frame := range track.video.frames {
						if frame.decodable && !frame.repeated && frame.source.written {
							observe(frame.source.at, frame.at)
						}
					}
				}
			}
		} else {
			for _, unit := range r.sentAudioUnits {
				if unit.written {
					observe(unit.at, unit.returnedAt)
				}
			}
		}
		if firstLo == 0 || firstHi == 0 {
			unknown("first identified post-recovery live content unavailable")
			if d.WindowOutcome != "fail" {
				d.WindowOutcome = "inconclusive"
			}
		} else {
			d.KillToLive = &DurationInterval{Lower: max(0, firstLo-hi), Upper: firstHi - lo}
		}
	}
	return d
}

// judgeDistributedGap bounds every candidate window, including offsets between
// the endpoints. Possible baseline samples bound its p95; guaranteed samples
// and zero padding bound it from below. Raw gaps are never clock-adjusted.
func judgeDistributedGap(kind string, arrivals []time.Duration, startLo, startHi, endLo, endHi time.Duration, scenario string) DistributedGap {
	g := DistributedGap{Kind: kind, Outcome: "pass"}
	var basePossible, baseCertain []time.Duration
	minEvent, maxEvent := time.Duration(0), time.Duration(0)
	covered := len(arrivals) > 1 && arrivals[0] < startLo-baselineWindow && arrivals[len(arrivals)-1] > endHi
	for i := 1; i < len(arrivals); i++ {
		a, b := arrivals[i-1], arrivals[i]
		gap := b - a
		if a >= startLo-baselineWindow && b < startHi {
			basePossible = append(basePossible, gap)
		}
		if a >= startHi-baselineWindow && b < startLo {
			baseCertain = append(baseCertain, gap)
		}
		if b > startLo && a <= endHi {
			maxEvent = max(maxEvent, gap)
		}
		if b > startHi && a <= endLo {
			minEvent = max(minEvent, gap)
		}
	}
	if !covered || len(baseCertain) < baselineMinSamples || maxEvent == 0 {
		g.Outcome = "inconclusive"
		g.Causes = []string{"insufficient caller window coverage"}
		return g
	}
	slices.Sort(basePossible)
	slices.Sort(baseCertain)
	g.BaselinePossible = basePossible
	g.BaselineCertain = baseCertain
	// At most len(basePossible) samples can belong to any baseline. Pad the
	// guaranteed set with zeros for a lower percentile bound.
	rank := (95*len(basePossible)+99)/100 - 1 - (len(basePossible) - len(baseCertain))
	lower := time.Duration(0)
	if rank >= 0 {
		lower = baseCertain[rank]
	}
	upper := baselineQuantileBounds(baseCertain, basePossible, 95).Upper
	g.BaselineP95 = DurationInterval{Lower: lower, Upper: upper}
	g.EventGap = DurationInterval{Lower: minEvent, Upper: maxEvent}
	pass := maxEvent < 2*time.Second
	failed := minEvent >= 2*time.Second
	if scenario == "move" {
		pass = pass && maxEvent <= 150*time.Millisecond && maxEvent <= lower+50*time.Millisecond
		failed = failed || minEvent > 150*time.Millisecond || minEvent > upper+50*time.Millisecond
	}
	if failed {
		g.Outcome = "fail"
		g.Causes = []string{"gap ceiling fails for every mapped window"}
	} else if !pass {
		g.Outcome = "inconclusive"
		g.Causes = []string{"gap judgment varies within clock interval"}
	}
	return g
}

// JudgeDistributedTrials preserves the existing count policy. It does not
// invent a network-loss allowance for a real unshaped run.
func JudgeDistributedTrials(moves []MoveReport) (string, []string) {
	outcome := "pass"
	var causes []string
	unknown := func(cause string) {
		if outcome != "fail" {
			outcome = "inconclusive"
		}
		causes = append(causes, cause)
	}
	fail := func(cause string) { outcome = "fail"; causes = append(causes, cause) }
	moveCount, killCount := 0, 0
	type pool struct{ lower, upper, certain, possible []time.Duration }
	pools := map[string]*pool{}
	for _, m := range moves {
		d := m.Distributed
		if d == nil {
			continue
		}
		switch m.Kind {
		case "move":
			moveCount++
		case "takeover":
			killCount++
		}
		window := d.WindowOutcome
		if window == "" {
			window = d.Outcome
		}
		if window == "fail" {
			fail("server loss or caller content failure")
		} else if window != "pass" {
			unknown("event window evidence incomplete")
		}
		if window != "pass" {
			causes = append(causes, d.Causes...)
		}
		for _, g := range d.Gaps {
			if len(g.BaselineCertain) < baselineMinSamples {
				unknown(g.Kind + " gap baseline has fewer than 10 guaranteed intervals")
				continue
			}
			if g.EventGap.Lower >= 2*time.Second {
				fail(g.Kind + " pause reaches 2s for every mapped window")
			} else if g.EventGap.Upper >= 2*time.Second {
				unknown(g.Kind + " pause may reach 2s within clock interval")
			}
			if baselineQuantileBounds(g.BaselineCertain, g.BaselinePossible, 95).Upper-baselineQuantileBounds(g.BaselineCertain, g.BaselinePossible, 50).Lower > 50*time.Millisecond {
				unknown(g.Kind + " baseline spread not proven within 50ms")
			}
			if m.Kind != "move" {
				continue
			}
			p := pools[g.Kind]
			if p == nil {
				p = &pool{}
				pools[g.Kind] = p
			}
			p.lower = append(p.lower, g.EventGap.Lower)
			p.upper = append(p.upper, g.EventGap.Upper)
			p.certain = append(p.certain, g.BaselineCertain...)
			p.possible = append(p.possible, g.BaselinePossible...)
		}
	}
	for kind, p := range pools {
		slices.Sort(p.certain)
		slices.Sort(p.possible)
		baseline := baselineQuantileBounds(p.certain, p.possible, 95)
		baselineLo, baselineHi := baseline.Lower, baseline.Upper
		lower, upper := durationP95(p.lower), durationP95(p.upper)
		if lower > 150*time.Millisecond || lower > baselineHi+50*time.Millisecond {
			fail(kind + " move p95 exceeds policy across clock interval")
		} else if upper > 150*time.Millisecond || upper > baselineLo+50*time.Millisecond {
			unknown(kind + " move p95 policy not proven across clock interval")
		}
	}
	if moveCount < 20 || killCount < 10 {
		unknown(fmt.Sprintf("too few distributed trials: moves %d/20, takeovers %d/10", moveCount, killCount))
	}
	return outcome, causes
}
func durationP95(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	values = slices.Clone(values)
	slices.Sort(values)
	return values[(95*len(values)+99)/100-1]
}

func (c *Call) ClockProbes() []ClockProbe {
	if c.rec.distributed == nil {
		return nil
	}
	return c.rec.distributed.Clock.Probes()
}

// DistributedRecording makes clock evidence reviewable in the final artifact.
type DistributedRecording struct {
	ObservedRate *ClockRateInterval `json:"observed_rate_ppm,omitempty"`
	DriftPPM     float64            `json:"drift_ppm"`

	Armed   []OperationIdentity `json:"armed"`
	Missing []OperationIdentity `json:"missing_receipts,omitempty"`

	RunID         string         `json:"run_id"`
	ControlOffset OffsetInterval `json:"control_offset"`
	WorkerOffset  OffsetInterval `json:"worker_offset"`
	Probes        []ClockProbe   `json:"probes"`
	ClockError    string         `json:"clock_error,omitempty"`
	Outcome       string         `json:"outcome"`
	Causes        []string       `json:"causes,omitempty"`
}

func (r *recorder) intervalContentLoss(kind string, startLo, startHi, endLo, endHi, until time.Duration) MetricVerdict {
	v := MetricVerdict{Trusted: true}
	if kind == kindVideo && r.sentVideo.Frames == 0 && r.sentVideo.SSRC == 0 {
		return v
	}
	if until < endHi+contentWindowMargin+contentSettleWindow {
		v.untrust("content settle window unavailable across interval")
		return v
	}
	possible, certain, units := 0, 0, 0
	observe := func(u contentUnit) {
		if !u.written || u.at < max(0, startLo-contentWindowMargin) || u.at >= endHi+contentWindowMargin {
			return
		}
		units++
		if u.returnedAt >= u.at && u.returnedAt > 0 && u.returnedAt <= until {
			return
		}
		possible++
		if u.at >= max(0, startHi-contentWindowMargin) && u.at < endLo+contentWindowMargin {
			certain++
		}
	}
	if kind == kindAudio {
		for _, u := range r.sentAudioUnits {
			observe(u)
		}
	} else {
		for _, f := range r.sentVideoFrames {
			observe(f.unit())
		}
	}
	if units == 0 {
		v.untrust("no identified sends across event interval")
	} else if certain > 0 {
		v.fail("sent content never returned in every mapped window")
	} else if possible > 0 {
		v.untrust("missing content lies in clock uncertainty margin")
	}
	if kind == kindVideo && r.contentIdentityErrors > 0 {
		v.Trusted = true
		v.fail("conflicting video content identities")
	}
	return v
}

// Every candidate baseline contains the certain set and a subset of possible.
// Zero padding gives a conservative lower quantile. The upper quantile uses
// the guaranteed count, padding with the largest optional samples; merely
// taking p95 of the union would underestimate a candidate with fewer samples.
func baselineQuantileBounds(certain, possible []time.Duration, percentile int) DurationInterval {
	if len(certain) == 0 || len(possible) < len(certain) {
		return DurationInterval{}
	}
	certain, possible = slices.Clone(certain), slices.Clone(possible)
	slices.Sort(certain)
	slices.Sort(possible)
	rank := (percentile*len(possible)+99)/100 - 1 - (len(possible) - len(certain))
	lower := time.Duration(0)
	if rank >= 0 {
		lower = certain[rank]
	}
	// Remove the guaranteed multiset to obtain optional samples.
	optional := make([]time.Duration, 0, len(possible)-len(certain))
	j := 0
	for _, value := range possible {
		if j < len(certain) && value == certain[j] {
			j++
			continue
		}
		optional = append(optional, value)
	}
	upper := certain[(percentile*len(certain)+99)/100-1]
	// A quantile can reach value v exactly when the guaranteed samples below
	// v fit below its rank after adding all optional samples >=v. Sweep the
	// sorted sets once instead of sorting one candidate for every subset size.
	ci, oi := len(certain), len(optional)
	for i := len(possible) - 1; i >= 0; i-- {
		value := possible[i]
		for ci > 0 && certain[ci-1] >= value {
			ci--
		}
		for oi > 0 && optional[oi-1] >= value {
			oi--
		}
		count := len(certain) + len(optional) - oi
		if ci <= (percentile*count+99)/100-1 {
			upper = value
			break
		}
	}
	return DurationInterval{Lower: lower, Upper: upper}
}

// mappedMoveTimes is also used by content baselines and online decode sampling;
// raw server times must never enter those caller-local windows.
func (r *recorder) mappedMoveTimes(move moveRecord) (DurationInterval, DurationInterval, error) {
	if move.event == nil {
		return DurationInterval{}, DurationInterval{}, errors.New("event identity unavailable")
	}
	host := r.distributed.ControlOffset
	if move.kind == "takeover" {
		host = r.distributed.WorkerOffset
	}
	start, err := r.distributed.Clock.Map(move.event.Start, host)
	if err != nil {
		return DurationInterval{}, DurationInterval{}, err
	}
	end, err := r.distributed.Clock.Map(move.event.End, r.distributed.ControlOffset)
	return DurationInterval{Lower: r.since(start.Earliest), Upper: r.since(start.Latest)}, DurationInterval{Lower: r.since(end.Earliest), Upper: r.since(end.Latest)}, err
}
