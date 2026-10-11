package callharness

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func lossEvidenceRecorder() *recorder {
	r := newRecorder()
	r.hungUpAt = 10 * time.Second
	for i := 0; i < 500; i++ {
		at := time.Duration(i) * 20 * time.Millisecond
		r.sentAudioUnits[strconv.Itoa(i)] = contentUnit{at: at, duration: 20 * time.Millisecond, returnedAt: at + time.Millisecond, written: true}
		if i%2 == 0 {
			r.sentVideoFrames[uint16(i)] = sentVideoFrame{at: at, duration: 40 * time.Millisecond, returnedAt: at + time.Millisecond, written: true}
		}
	}
	r.sentVideo.Frames = 250
	return r
}

func TestContentLossEvidenceCountsWrittenIdentitiesAndEqualCleanWindows(t *testing.T) {
	r := lossEvidenceRecorder()
	u := r.sentAudioUnits["250"]
	u.returnedAt, u.duration = 0, 6500*time.Microsecond
	r.sentAudioUnits["250"] = u
	v := r.sentVideoFrames[250]
	v.returnedAt = 0
	r.sentVideoFrames[250] = v
	r.sentAudioUnits["failed write"] = contentUnit{at: 5 * time.Second}
	// A repeated return is not a new successful send or a repaired missing unit.
	r.repeatedAudioReturns = []contentUnit{{at: 5 * time.Second, returnedAt: 6 * time.Second, written: true, replayed: true}}
	move := MoveReport{Kind: "takeover", Start: 5 * time.Second, End: 5400 * time.Millisecond, Recovery: VideoRecovery{MediaResumedAt: 5400 * time.Millisecond}}
	m := r.eventMeasurement(move, r.hungUpAt, 0)
	e := m.ContentLoss
	require.Equal(t, 6500*time.Microsecond, m.LostAudio)
	require.Equal(t, 1, e.Event.Audio.Lost, "count identities, not audio duration / 20ms")
	require.Equal(t, 1, e.Event.Video.Lost)
	require.Equal(t, 45, e.Event.Audio.Sent)
	require.True(t, e.Event.Audio.Trusted)
	require.True(t, e.Event.Video.Trusted)
	require.Len(t, e.Baselines, 3)
	lastStart := e.Event.Start - contentSettleWindow
	for _, w := range e.Baselines {
		require.Equal(t, e.Event.End-e.Event.Start, w.End-w.Start)
		require.LessOrEqual(t, w.End, lastStart)
		lastStart = w.Start
		require.Equal(t, 45, w.Audio.Sent)
		require.Zero(t, w.Audio.Lost)
		require.Zero(t, w.Video.Lost)
		require.True(t, w.Audio.Trusted)
		require.True(t, w.Video.Trusted)
	}
}

func TestContentLossEvidenceRejectsExpiredUnsettledAndPriorEventWindows(t *testing.T) {
	move := MoveReport{Kind: "takeover", Start: 5 * time.Second, End: 5400 * time.Millisecond, Recovery: VideoRecovery{MediaResumedAt: 5400 * time.Millisecond}}
	t.Run("expired baseline", func(t *testing.T) {
		r := lossEvidenceRecorder()
		r.historyStart = 4 * time.Second
		m := r.eventMeasurement(move, r.hungUpAt, 0)
		require.Empty(t, m.ContentLoss.Baselines)
	})
	t.Run("unsettled", func(t *testing.T) {
		r := lossEvidenceRecorder()
		r.hungUpAt = 6 * time.Second
		m := r.eventMeasurement(move, r.hungUpAt, 0)
		require.False(t, m.ContentLoss.Event.Audio.Trusted)
		require.False(t, m.ContentLoss.Event.Video.Trusted)
	})
	t.Run("prior move", func(t *testing.T) {
		r := lossEvidenceRecorder()
		r.moves = []moveRecord{{start: r.start.Add(4 * time.Second), end: r.start.Add(4100 * time.Millisecond)}}
		m := r.eventMeasurement(move, r.hungUpAt, 0)
		require.Empty(t, m.ContentLoss.Baselines)
	})
	t.Run("saved prior move", func(t *testing.T) {
		r := lossEvidenceRecorder()
		r.savedMoves = []MoveReport{{Start: 4 * time.Second, End: 4100 * time.Millisecond, Measurement: EventMeasurement{WindowEnd: 4350 * time.Millisecond}}}
		m := r.eventMeasurement(move, r.hungUpAt, 0)
		require.Empty(t, m.ContentLoss.Baselines)
	})
	t.Run("identity conflict", func(t *testing.T) {
		r := lossEvidenceRecorder()
		r.contentIdentityErrors = 1
		m := r.eventMeasurement(move, r.hungUpAt, 0)
		require.False(t, m.ContentLoss.Event.Video.Trusted)
		for _, w := range m.ContentLoss.Baselines {
			require.False(t, w.Video.Trusted)
		}
	})
}
