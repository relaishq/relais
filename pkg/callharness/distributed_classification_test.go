package callharness

import (
	"fmt"
	"github.com/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
	"slices"
	"strings"
	"testing"
	"time"
)

func classificationFixture(t *testing.T, width time.Duration) (*recorder, moveRecord) {
	t.Helper()
	r := testMeasurementRecorder(t)
	r.start = time.Now().Add(-10 * time.Second)
	var arrivals []time.Duration
	for at := 500 * time.Millisecond; at < 1500*time.Millisecond; at += 20 * time.Millisecond {
		arrivals = append(arrivals, at)
	}
	for at := 1849 * time.Millisecond; at < 7*time.Second; at += 20 * time.Millisecond {
		arrivals = append(arrivals, at)
	}
	r.tracks[0].arrivals = arrivals
	r.tracks[0].headers = make([]rtpMark, len(arrivals))
	for i, at := range arrivals {
		seq := uint16(i)
		if at >= 1849*time.Millisecond {
			seq += 8192
		}
		r.tracks[0].headers[i] = rtpMark{seq: seq}
	}
	clock, err := NewClockTracker(0)
	require.NoError(t, err)
	require.NoError(t, clock.Add(ClockProbe{CallerSend: r.start, CallerReceive: r.start.Add(width), ServerReceive: r.start.Add(width / 2), ServerSend: r.start.Add(width / 2)}))
	r.distributed = &DistributedOptions{RunID: "run", Clock: clock}
	id := OperationIdentity{RunID: "run", OperationID: "event", SessionID: "session", Worker: "0", Epoch: 1}
	e := DistributedEvent{Identity: id, Kind: "takeover", Start: r.start.Add(1500 * time.Millisecond), End: r.start.Add(1800 * time.Millisecond), Result: controlplane.MoveResult{ID: "session", From: "0", To: "1", Kind: "takeover"}}
	m := moveRecord{event: &e, kind: e.Kind, start: e.Start, end: e.End, armedAt: r.start.Add(time.Second), receivedAt: r.start.Add(8 * time.Second)}
	r.moves = []moveRecord{m}
	return r, m
}

func TestDistributedSmallIntervalTrustsContentClassifications(t *testing.T) {
	r, _ := classificationFixture(t, 6*time.Millisecond)
	m := r.measureMoves(r.hungUpAt, 1)[0].Measurement
	for _, v := range []MetricVerdict{m.AudioLoss, m.VideoLoss, m.Audio.Verdict, m.Video.Verdict, m.ResumeVerdict, m.FirstContentVerdict, m.FirstNewContentVerdict, m.FirstLiveVerdict} {
		require.True(t, v.Trusted, v.Reasons)
		require.False(t, v.Failed, v.Reasons)
	}
	require.True(t, m.Conclusive(), m.Summary())
}

func delayFreshness(r *recorder, firstReturn time.Duration) {
	// Remove the earlier live run and supply three unique, on-time live units
	// whose first return sits on the chosen caller-local instant.
	for key, u := range r.sentAudioUnits {
		if u.at >= 1849*time.Millisecond {
			delete(r.sentAudioUnits, key)
		}
	}
	for key, f := range r.sentVideoFrames {
		if f.at >= 1849*time.Millisecond {
			delete(r.sentVideoFrames, key)
		}
	}
	for i := 0; i < 8; i++ {
		returned := firstReturn + time.Duration(i)*20*time.Millisecond
		at := returned - time.Millisecond
		r.sentAudioUnits[fmt.Sprint("late-run-", i)] = contentUnit{at: at, returnedAt: returned, written: true, identity: fmt.Sprint(i)}
		r.sentVideoFrames[uint16(100+i)] = sentVideoFrame{identity: uint64(100 + i), at: at, returnedAt: returned, written: true}
	}
}
func TestDistributedFreshnessDurationInterval(t *testing.T) {
	for _, tc := range []struct {
		name            string
		first           time.Duration
		trusted, failed bool
	}{
		{"clear pass", 3400 * time.Millisecond, true, false},
		{"clear fail", 3600 * time.Millisecond, true, true},
		{"straddles 2s", 3500 * time.Millisecond, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := classificationFixture(t, 6*time.Millisecond)
			delayFreshness(r, tc.first)
			rep := r.measureMoves(r.hungUpAt, 1)[0]
			for _, v := range []MetricVerdict{rep.Measurement.Audio.Verdict, rep.Measurement.Video.Verdict} {
				require.Equal(t, tc.trusted, v.Trusted, v.Reasons)
				require.Equal(t, tc.failed, v.Trusted && v.Failed, v.Reasons)
				if !tc.trusted {
					require.Contains(t, strings.Join(v.Reasons, ";"), "width=6ms")
					require.Contains(t, strings.Join(v.Reasons, ";"), "duration=[1.997s,2.003s]")
				}
			}
			require.Equal(t, DurationInterval{Lower: tc.first - 1503*time.Millisecond, Upper: tc.first - 1497*time.Millisecond}, rep.Distributed.Durations["audio_freshness"])
		})
	}
}
func TestDistributedZeroIntervalEqualsLocal(t *testing.T) {
	for _, failed := range []bool{false, true} {
		r, m := classificationFixture(t, 0)
		if failed {
			delayFreshness(r, 3600*time.Millisecond)
		}
		distributed := r.measureMoves(r.hungUpAt, 1)[0]
		m.event = nil
		r.moves = []moveRecord{m}
		r.distributed = nil
		local := r.measureMoves(r.hungUpAt, 1)[0]
		require.Equal(t, local.Measurement, distributed.Measurement)
		require.Equal(t, local.Tracks, distributed.Tracks)
		require.Equal(t, local.Recovery, distributed.Recovery)
	}
}

func TestDistributedInteriorNonmonotonicClassification(t *testing.T) {
	r, _ := classificationFixture(t, 40*time.Millisecond)
	for key, u := range r.sentAudioUnits {
		if u.at < 1849*time.Millisecond {
			delete(r.sentAudioUnits, key)
		}
	}
	for i := 0; i < 10; i++ {
		for _, group := range []struct {
			name     string
			returned time.Duration
		}{{"old", 485 * time.Millisecond}, {"new", 1500 * time.Millisecond}} {
			returned := group.returned + time.Duration(i)*time.Millisecond
			r.sentAudioUnits[fmt.Sprint(group.name, i)] = contentUnit{at: returned - time.Millisecond, returnedAt: returned, written: true}
		}
	}
	// Both endpoints have ten good baseline samples. Inside the interval the
	// old group has expired but the new group is still post-event, so coverage
	// is unknown. An endpoint-only check would falsely trust freshness.
	point := func(start time.Duration) MetricVerdict {
		p := MoveReport{Kind: "takeover", Start: start, End: 1800 * time.Millisecond, Recovery: r.videoRecovery(start, r.hungUpAt)}
		return r.eventMeasurement(p, r.hungUpAt, 0).Audio.Verdict
	}
	require.True(t, point(1480*time.Millisecond).Trusted)
	require.True(t, point(1520*time.Millisecond).Trusted)
	require.False(t, point(1495*time.Millisecond).Trusted)
	rep := r.measureMoves(r.hungUpAt, 1)[0]
	require.False(t, rep.Measurement.Audio.Verdict.Trusted)
	require.Contains(t, strings.Join(rep.Measurement.Audio.Verdict.Reasons, ";"), "classification changes")
	require.Greater(t, rep.Distributed.Evaluations, 2)
}

func TestDistributedWANIntervalWithDenseRTP(t *testing.T) {
	r, m := classificationFixture(t, 60*time.Millisecond)
	m.kind = "move"
	m.event.Kind = "move"
	m.event.Result.Kind = "move"
	r.moves = []moveRecord{m}
	var burst []time.Duration
	for i := 0; i < 2000; i++ {
		burst = append(burst, 1800*time.Millisecond+time.Duration(i))
	}
	r.tracks[0].arrivals = append(r.tracks[0].arrivals, burst...)
	slices.Sort(r.tracks[0].arrivals)
	r.tracks[0].headers = make([]rtpMark, len(r.tracks[0].arrivals))
	rep := r.measureMoves(r.hungUpAt, 1)[0]
	require.True(t, rep.Measurement.Conclusive(), rep.Measurement.Summary())
	require.Less(t, rep.Distributed.Evaluations, maxClassificationEvaluations)
	require.Less(t, rep.Distributed.Evaluations, 1000, "RTP fragments sharing classification cells must not exhaust the work budget")
}

func TestClassificationMeasurementMatchesFullHistory(t *testing.T) {
	r, _ := classificationFixture(t, 60*time.Millisecond)
	for i := 0; i < 9000; i++ {
		key := fmt.Sprint("old-", i)
		r.sentAudioUnits[key] = contentUnit{at: time.Duration(i + 1), returnedAt: time.Millisecond + time.Duration(i+1), written: true}
	}
	r.repeatedAudioReturns = append(r.repeatedAudioReturns, contentUnit{at: time.Nanosecond, returnedAt: 1950 * time.Millisecond, written: true, replayed: true, identity: "old-0"})
	for _, start := range []time.Duration{1470 * time.Millisecond, 1500 * time.Millisecond, 1530 * time.Millisecond} {
		p := MoveReport{Kind: "takeover", Start: start, End: 1800 * time.Millisecond, Recovery: r.videoRecovery(start, r.hungUpAt)}
		full := r.eventMeasurement(p, r.hungUpAt, 0)
		cells := r.classificationMeasurement(p, r.hungUpAt, 0)
		require.Equal(t, contentVerdicts(&full), contentVerdicts(&cells))
		require.Equal(t, full.Audio, cells.Audio)
		require.Equal(t, full.Video, cells.Video)
		require.Equal(t, full.FirstContent, cells.FirstContent)
		require.Equal(t, full.FirstNewContent, cells.FirstNewContent)
		require.Equal(t, full.FirstLiveFrame, cells.FirstLiveFrame)
	}
}

func TestDistributedShortSettleDoesNotHideKnownResume(t *testing.T) {
	r, _ := classificationFixture(t, 6*time.Millisecond)
	r.hungUpAt = 2200 * time.Millisecond
	rep := r.measureMoves(r.hungUpAt, 1)[0]
	require.True(t, rep.Distributed.Mapped)
	require.False(t, rep.Measurement.AudioLoss.Trusted)
	require.False(t, rep.Measurement.VideoLoss.Trusted)
	require.True(t, rep.Measurement.ResumeVerdict.Trusted, rep.Measurement.ResumeVerdict.Reasons)
	require.False(t, rep.Measurement.ResumeVerdict.Failed)
	require.True(t, rep.Measurement.FirstContentVerdict.Trusted, rep.Measurement.FirstContentVerdict.Reasons)
}
