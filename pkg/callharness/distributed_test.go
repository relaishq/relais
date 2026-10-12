package callharness

import (
	"errors"
	"github.com/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
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
	r.sentAudioUnits["good"] = contentUnit{at: 3 * time.Second, returnedAt: 3020 * time.Millisecond, written: true, duration: 20 * time.Millisecond}
	v := r.intervalContentLoss(kindAudio, 3*time.Second, 3100*time.Millisecond, 3200*time.Millisecond, 3300*time.Millisecond, 5*time.Second)
	require.True(t, v.Trusted)
	require.False(t, v.Failed)
	r.sentAudioUnits["margin"] = contentUnit{at: 2800 * time.Millisecond, written: true, duration: 20 * time.Millisecond}
	v = r.intervalContentLoss(kindAudio, 3*time.Second, 3100*time.Millisecond, 3200*time.Millisecond, 3300*time.Millisecond, 5*time.Second)
	require.False(t, v.Trusted)
	require.False(t, v.Failed)
	r.sentAudioUnits["certain"] = contentUnit{at: 3 * time.Second, written: true, duration: 20 * time.Millisecond}
	v = r.intervalContentLoss(kindAudio, 3*time.Second, 3100*time.Millisecond, 3200*time.Millisecond, 3300*time.Millisecond, 5*time.Second)
	require.True(t, v.Trusted)
	require.True(t, v.Failed)
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
	r.tracks = []*trackRecord{{kind: kindAudio, arrivals: arrivals}}
	r.sentAudioUnits["old"] = contentUnit{at: 3 * time.Second, returnedAt: 3020 * time.Millisecond, written: true}
	c := &Call{rec: r, remoteUfrag: "session"}
	require.NoError(t, c.RecordDistributedEvent(DistributedEvent{Identity: id, Kind: "move", Start: r.start.Add(3 * time.Second), End: r.start.Add(3200 * time.Millisecond)}))
	d := r.distributedMeasurement(r.moves[0], 10*time.Second)
	require.True(t, d.Mapped)
	require.Equal(t, "pass", d.Gaps[0].Outcome)
	require.False(t, d.AudioLoss.Trusted)
	require.Equal(t, "inconclusive", d.Outcome)
}
