package callharness

import (
	"errors"
	"fmt"
	"github.com/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
	"slices"
	"testing"
	"time"
)

func steadyArrivals() []time.Duration {
	var a []time.Duration
	for d := time.Duration(0); d <= 12*time.Second; d += 20 * time.Millisecond {
		a = append(a, d)
	}
	return a
}
func TestDistributedGapIntervalJudgments(t *testing.T) {
	a := steadyArrivals()
	pass := judgeDistributedGap("audio", a, 3*time.Second, 3010*time.Millisecond, 3100*time.Millisecond, 3110*time.Millisecond, "move")
	require.Equal(t, "pass", pass.Outcome)
	require.Equal(t, 20*time.Millisecond, pass.EventGap.Upper)
	// The same 180ms gap may or may not intersect the mapped event.
	var sparse []time.Duration
	for _, d := range a {
		if d <= 3*time.Second || d >= 3180*time.Millisecond {
			sparse = append(sparse, d)
		}
	}
	unknown := judgeDistributedGap("audio", sparse, 2900*time.Millisecond, 3190*time.Millisecond, 3300*time.Millisecond, 3310*time.Millisecond, "move")
	require.Equal(t, "inconclusive", unknown.Outcome)
	failed := judgeDistributedGap("audio", sparse, 3050*time.Millisecond, 3060*time.Millisecond, 3100*time.Millisecond, 3110*time.Millisecond, "move")
	require.Equal(t, "fail", failed.Outcome)
}
func TestDistributedIdentityAndFailedReceipts(t *testing.T) {
	r := newRecorder()
	clock, _ := NewClockTracker(0)
	require.NoError(t, clock.Add(probeAt(r.start, 0, time.Millisecond)))
	r.distributed = &DistributedOptions{RunID: "run", Clock: clock}
	c := &Call{rec: r, remoteUfrag: "session"}
	id := OperationIdentity{RunID: "run", OperationID: "move-1", SessionID: "session", Worker: "generation-a", Epoch: 4}
	e := DistributedEvent{Identity: id, Kind: "move", Start: r.start, End: r.start.Add(time.Millisecond), Uncertain: true, Result: controlplane.MoveResult{ID: "session", From: "generation-a", Kind: "move", Error: "uncertain", Lost: true, LossCause: "ownership_expired"}}
	require.ErrorContains(t, c.RecordDistributedEvent(e), "not armed")
	require.NoError(t, c.ArmDistributedEvent(id))
	require.NoError(t, c.RecordDistributedEvent(e))
	require.NoError(t, c.RecordDistributedEvent(e))
	require.Len(t, r.moves, 1)
	require.True(t, r.moves[0].lost)
	require.Equal(t, "ownership_expired", r.moves[0].lossCause)
	require.Error(t, r.moves[0].err)
	changed := e
	changed.Result.Error = "other"
	require.ErrorContains(t, c.RecordDistributedEvent(changed), "conflicting")
	changed = e
	changed.Identity.Epoch++
	require.Error(t, c.RecordDistributedEvent(changed))
	d := r.distributedMeasurement(r.moves[0], time.Second)
	require.Equal(t, "fail", d.Outcome, "known loss survives invalid timing")
}
func TestDistributedRetention(t *testing.T) {
	r := newRecorder()
	r.recording = RecordingOptions{History: 180 * time.Second, DecodeEvery: 30}
	r.tracks = []*trackRecord{{kind: kindAudio, arrivals: steadyArrivals(), headers: make([]rtpMark, len(steadyArrivals()))}}
	r.compact(170 * time.Second)
	require.Zero(t, r.historyStart)
	require.Len(t, r.tracks[0].arrivals, len(steadyArrivals()))
	r.compact(200 * time.Second)
	require.Equal(t, 20*time.Second, r.historyStart)
}
func TestDistributedMinimumTrials(t *testing.T) {
	var moves []MoveReport
	for range 20 {
		moves = append(moves, MoveReport{Kind: "move", Distributed: &DistributedMeasurement{Outcome: "pass"}})
	}
	for range 10 {
		moves = append(moves, MoveReport{Kind: "takeover", Distributed: &DistributedMeasurement{Outcome: "pass"}})
	}
	outcome, causes := JudgeDistributedTrials(moves)
	require.Equal(t, "pass", outcome)
	require.Empty(t, causes)
	outcome, _ = JudgeDistributedTrials(moves[:29])
	require.Equal(t, "inconclusive", outcome)
	moves[0].Distributed.Outcome = "fail"
	outcome, _ = JudgeDistributedTrials(moves[:1])
	require.Equal(t, "fail", outcome)
}

func TestDistributedP95AllowsOneMoveOutlier(t *testing.T) {
	var moves []MoveReport
	baseline := make([]time.Duration, 50)
	for i := range baseline {
		baseline[i] = 20 * time.Millisecond
	}
	for i := range 20 {
		gap := 20 * time.Millisecond
		if i == 19 {
			gap = 180 * time.Millisecond
		}
		moves = append(moves, MoveReport{Kind: "move", Distributed: &DistributedMeasurement{Outcome: "fail", WindowOutcome: "pass", Gaps: []DistributedGap{{Kind: "audio", EventGap: DurationInterval{Lower: gap, Upper: gap}, BaselineCertain: baseline, BaselinePossible: baseline}}}})
	}
	for range 10 {
		moves = append(moves, MoveReport{Kind: "takeover", Distributed: &DistributedMeasurement{Outcome: "pass", WindowOutcome: "pass"}})
	}
	outcome, _ := JudgeDistributedTrials(moves)
	require.Equal(t, "pass", outcome, "the policy gates pooled p95, not every trial's 150ms gap")
	moves[18].Distributed.Gaps[0].EventGap = DurationInterval{Lower: 180 * time.Millisecond, Upper: 180 * time.Millisecond}
	outcome, _ = JudgeDistributedTrials(moves)
	require.Equal(t, "fail", outcome)
	moves[18].Distributed.Gaps[0].EventGap = DurationInterval{Lower: 20 * time.Millisecond, Upper: 180 * time.Millisecond}
	outcome, _ = JudgeDistributedTrials(moves)
	require.Equal(t, "inconclusive", outcome)
}

func TestDistributedContentLossOverInterval(t *testing.T) {
	r := newRecorder()
	r.start = time.Now().Add(-10 * time.Second)
	r.hungUpAt = 8 * time.Second
	clock, _ := NewClockTracker(0)
	require.NoError(t, clock.Add(probeAt(r.start, 0, 100*time.Millisecond)))
	r.distributed = &DistributedOptions{RunID: "run", Clock: clock}
	id := OperationIdentity{RunID: "run", OperationID: "loss", SessionID: "session", Worker: "0", Epoch: 1}
	e := DistributedEvent{Identity: id, Kind: "move", Start: r.start.Add(3050 * time.Millisecond), End: r.start.Add(3250 * time.Millisecond)}
	r.moves = []moveRecord{{kind: "move", start: e.Start, end: e.End, event: &e, armedAt: r.start.Add(2 * time.Second), receivedAt: r.start.Add(7 * time.Second)}}
	arrivals := steadyArrivals()
	r.tracks = []*trackRecord{{kind: kindAudio, arrivals: arrivals, headers: make([]rtpMark, len(arrivals))}}
	for i := 0; i < 400; i++ {
		at := time.Duration(i) * 20 * time.Millisecond
		r.sentAudioUnits[fmt.Sprint(i)] = contentUnit{at: at, returnedAt: at + time.Millisecond, written: true, duration: 20 * time.Millisecond}
	}
	verdict := func() MetricVerdict { return r.measureMoves(r.hungUpAt, 1)[0].Measurement.AudioLoss }
	v := verdict()
	require.True(t, v.Trusted, v.Reasons)
	require.False(t, v.Failed, v.Reasons)
	r.sentAudioUnits["margin"] = contentUnit{at: 2780 * time.Millisecond, written: true, duration: 20 * time.Millisecond}
	v = verdict()
	require.False(t, v.Trusted, v.Reasons)
	require.False(t, v.Trusted && v.Failed)
	r.sentAudioUnits["certain"] = contentUnit{at: 3 * time.Second, written: true, duration: 20 * time.Millisecond}
	v = verdict()
	require.True(t, v.Trusted, v.Reasons)
	require.True(t, v.Failed, v.Reasons)
}

func TestDistributedMissingReceiptsAreReported(t *testing.T) {
	r := newRecorder()
	clock, _ := NewClockTracker(0)
	require.NoError(t, clock.Add(probeAt(r.start, 0, time.Millisecond)))
	r.distributed = &DistributedOptions{RunID: "run", Clock: clock}
	c := &Call{rec: r, remoteUfrag: "session"}
	id := OperationIdentity{RunID: "run", OperationID: "missing", SessionID: "session", Worker: "0", Epoch: 1}
	require.NoError(t, c.ArmDistributedEvent(id))
	report := r.report()
	require.Equal(t, "inconclusive", report.Distributed.Outcome)
	require.Equal(t, []OperationIdentity{id}, report.Distributed.Missing)
	require.Contains(t, report.Distributed.Causes, "armed event receipt missing: missing")
}

func TestDistributedLateReceiptRetainsArmedWindow(t *testing.T) {
	r := newRecorder()
	r.start = time.Now().Add(-170 * time.Second)
	r.recording = RecordingOptions{History: 180 * time.Second, DecodeEvery: 30}
	clock, _ := NewClockTracker(0)
	require.NoError(t, clock.Add(probeAt(r.start, 0, time.Millisecond)))
	r.distributed = &DistributedOptions{RunID: "run", Clock: clock}
	c := &Call{rec: r, remoteUfrag: "session"}
	id := OperationIdentity{RunID: "run", OperationID: "late", SessionID: "session", Worker: "0", Epoch: 1}
	r.armed = map[OperationIdentity]time.Time{id: r.start.Add(2 * time.Second)}
	a := steadyArrivals()
	r.tracks = []*trackRecord{{kind: kindAudio, arrivals: a, headers: make([]rtpMark, len(a))}}
	r.compact(170 * time.Second)
	require.NoError(t, c.RecordDistributedEvent(DistributedEvent{Identity: id, Kind: "move", Start: r.start.Add(3 * time.Second), End: r.start.Add(3200 * time.Millisecond)}))
	d := r.distributedMeasurement(r.moves[0], 170*time.Second)
	require.True(t, d.Mapped)
	require.Len(t, d.Gaps, 1)
	require.Equal(t, "pass", d.Gaps[0].Outcome)
}

func TestDistributedClockJumpInvalidatesSavedSummary(t *testing.T) {
	r := newRecorder()
	clock, _ := NewClockTracker(0)
	require.NoError(t, clock.Add(probeAt(r.start, 0, time.Millisecond)))
	r.distributed = &DistributedOptions{RunID: "run", Clock: clock}
	id := OperationIdentity{RunID: "run", OperationID: "saved", SessionID: "session", Worker: "0", Epoch: 1}
	// A numeric outlier may fail one window but still fit the pooled p95 policy.
	// A later clock jump must invalidate its window trust independently.
	r.savedMoves = []MoveReport{{Identity: &id, Kind: "move", Distributed: &DistributedMeasurement{Mapped: true, Outcome: "fail", WindowOutcome: "pass"}}}
	clock.Invalidate(errors.New("clock jump"))
	report := r.report()
	require.False(t, report.Moves[0].Distributed.Mapped)
	require.Equal(t, "inconclusive", report.Moves[0].Distributed.WindowOutcome)
	require.Equal(t, "inconclusive", report.Distributed.Outcome)
	require.Equal(t, "clock jump", report.Distributed.ClockError)
}

func TestDistributedRTPDoesNotProveLiveContent(t *testing.T) {
	r := newRecorder()
	r.start = time.Now().Add(-10 * time.Second)
	clock, _ := NewClockTracker(0)
	require.NoError(t, clock.Add(probeAt(r.start, 0, time.Millisecond)))
	r.distributed = &DistributedOptions{RunID: "run", Clock: clock}
	id := OperationIdentity{RunID: "run", OperationID: "move", SessionID: "session", Worker: "0", Epoch: 1}
	r.armed = map[OperationIdentity]time.Time{id: r.start.Add(2 * time.Second)}
	// Continued RTP can include delayed old-owner packets. Only this earlier
	// identified send returned; none proves live media after completion.
	var arrivals []time.Duration
	for at := time.Duration(0); at <= 10*time.Second; at += 20 * time.Millisecond {
		arrivals = append(arrivals, at)
	}
	r.tracks = []*trackRecord{{kind: kindAudio, arrivals: arrivals, headers: make([]rtpMark, len(arrivals))}}
	r.hungUpAt = 10 * time.Second
	r.sentAudioUnits["old"] = contentUnit{at: 3 * time.Second, returnedAt: 3020 * time.Millisecond, written: true}
	c := &Call{rec: r, remoteUfrag: "session"}
	require.NoError(t, c.RecordDistributedEvent(DistributedEvent{Identity: id, Kind: "move", Start: r.start.Add(3 * time.Second), End: r.start.Add(3200 * time.Millisecond)}))
	d := r.measureMoves(10*time.Second, 1)[0].Distributed
	require.True(t, d.Mapped)
	require.Equal(t, "pass", d.Gaps[0].Outcome)
	require.False(t, d.AudioLoss.Trusted)
	require.Equal(t, "inconclusive", d.Outcome)
}

func trialsWithBaseline(certain, possible []time.Duration, gap time.Duration) []MoveReport {
	var moves []MoveReport
	for range 20 {
		moves = append(moves, MoveReport{Kind: "move", Distributed: &DistributedMeasurement{WindowOutcome: "pass", Outcome: "pass", Gaps: []DistributedGap{{Kind: kindAudio, BaselineCertain: certain, BaselinePossible: possible, EventGap: DurationInterval{Lower: gap, Upper: gap}}}}})
	}
	for range 10 {
		moves = append(moves, MoveReport{Kind: "takeover", Distributed: &DistributedMeasurement{WindowOutcome: "pass", Outcome: "pass"}})
	}
	return moves
}
func TestDistributedBaselinePolicyUsesQuantiles(t *testing.T) {
	certain := make([]time.Duration, 40)
	for i := range certain {
		certain[i] = 20 * time.Millisecond
	}
	possible := append(slices.Clone(certain), 90*time.Millisecond)
	outcome, causes := JudgeDistributedTrials(trialsWithBaseline(certain, possible, 60*time.Millisecond))
	require.Equal(t, "pass", outcome, causes) // one rare baseline outlier cannot make p95-median exceed 50ms
	outcome, causes = JudgeDistributedTrials(trialsWithBaseline(certain, possible, 80*time.Millisecond))
	require.Equal(t, "fail", outcome, causes) // p95 upper is 20ms even though max possible is 90ms
}
func TestDistributedCertainPossibleBaselineMustNotUseHighestForPass(t *testing.T) {
	certain := make([]time.Duration, 40)
	for i := range certain {
		certain[i] = 20 * time.Millisecond
	}
	possible := append(slices.Clone(certain), 60*time.Millisecond, 60*time.Millisecond, 60*time.Millisecond)
	bounds := baselineQuantileBounds(certain, possible, 95)
	require.Equal(t, DurationInterval{Lower: 20 * time.Millisecond, Upper: 60 * time.Millisecond}, bounds)
	outcome, causes := JudgeDistributedTrials(trialsWithBaseline(certain, possible, 75*time.Millisecond))
	require.Equal(t, "inconclusive", outcome, causes)
	outcome, causes = JudgeDistributedTrials(trialsWithBaseline(certain, possible, 65*time.Millisecond))
	require.Equal(t, "pass", outcome, causes)
}

func TestDistributedGapUsesCertainLowerBaseline(t *testing.T) {
	var arrivals []time.Duration
	for at := 1800 * time.Millisecond; at <= 3*time.Second; at += 20 * time.Millisecond {
		arrivals = append(arrivals, at)
	}
	arrivals = append(arrivals, 3060*time.Millisecond, 3120*time.Millisecond, 3180*time.Millisecond, 3240*time.Millisecond, 3260*time.Millisecond, 3280*time.Millisecond, 3355*time.Millisecond)
	for at := 3375 * time.Millisecond; at < 4*time.Second; at += 20 * time.Millisecond {
		arrivals = append(arrivals, at)
	}
	g := judgeDistributedGap(kindAudio, arrivals, 3*time.Second, 3250*time.Millisecond, 3380*time.Millisecond, 3400*time.Millisecond, "move")
	require.Equal(t, 20*time.Millisecond, g.BaselineP95.Lower)
	require.Equal(t, 60*time.Millisecond, g.BaselineP95.Upper)
	require.Equal(t, "inconclusive", g.Outcome, g.Causes)
}

func TestDistributedUnmappedEventWidensBaselineFloor(t *testing.T) {
	r, m := classificationFixture(t, 6*time.Millisecond)
	r.baselineFloor = time.Second
	m.event.Start = time.Time{}
	r.moves = []moveRecord{m}
	report := r.measureMoves(r.hungUpAt, 1)[0]
	require.False(t, report.Distributed.Mapped)
	require.Equal(t, DurationInterval{Lower: time.Second, Upper: r.hungUpAt}, report.Distributed.BaselineFloor)
}

func TestDistributedFreshnessClipsAtNextMappedStart(t *testing.T) {
	r, m := classificationFixture(t, 6*time.Millisecond)
	tracker, _ := NewClockTracker(0)
	require.NoError(t, tracker.Add(ClockProbe{CallerSend: r.start, CallerReceive: r.start.Add(6 * time.Millisecond), ServerReceive: r.start.Add(3003 * time.Millisecond), ServerSend: r.start.Add(3003 * time.Millisecond)}))
	r.distributed.Clock = tracker
	m.event.Start = m.event.Start.Add(3 * time.Second)
	m.event.End = m.event.End.Add(3 * time.Second)
	next := m
	e := *m.event
	e.Identity.OperationID = "next"
	e.Start = r.start.Add(8 * time.Second)
	e.End = r.start.Add(8200 * time.Millisecond)
	next.event = &e
	next.start = e.Start
	next.end = e.End
	next.armedAt = r.start.Add(4 * time.Second)
	next.receivedAt = r.start.Add(9 * time.Second)
	r.moves = []moveRecord{m, next}
	report := r.measureMoves(r.hungUpAt, 1)[0]
	require.Equal(t, 4997*time.Millisecond, report.Measurement.Audio.EvaluationEnd)
	require.False(t, report.Measurement.Audio.Verdict.Trusted, report.Measurement.Audio.Verdict.Reasons)
}

func TestBaselineQuantileBoundsCoverEveryOptionalSubset(t *testing.T) {
	certain := []time.Duration{10, 20, 30, 40}
	optional := []time.Duration{1, 50, 60}
	possible := append(slices.Clone(certain), optional...)
	for _, percentile := range []int{50, 95} {
		bounds := baselineQuantileBounds(certain, possible, percentile)
		highest := time.Duration(0)
		for mask := 0; mask < 1<<len(optional); mask++ {
			candidate := slices.Clone(certain)
			for i, value := range optional {
				if mask&(1<<i) != 0 {
					candidate = append(candidate, value)
				}
			}
			slices.Sort(candidate)
			q := candidate[(percentile*len(candidate)+99)/100-1]
			require.LessOrEqual(t, bounds.Lower, q)
			require.GreaterOrEqual(t, bounds.Upper, q)
			highest = max(highest, q)
		}
		require.Equal(t, highest, bounds.Upper)
	}
}
