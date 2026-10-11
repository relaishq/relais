package callharness

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func TestRecorderKeepsMidRunGapsAfterHistoryExpires(t *testing.T) {
	r := newRecorder()
	r.recording.History = longRunHistory
	track := r.addTrack(kindAudio, 1, 111)
	at := time.Second
	for i := 0; i < 5000; i++ {
		at += 20 * time.Millisecond
		if i == 1000 {
			at += 500 * time.Millisecond
		}
		if i == 3000 {
			at += 2 * time.Second
		}
		r.packet(track, &rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i), Timestamp: uint32(i * 960)}}, r.start.Add(at))
		r.mu.Lock()
		r.compact(at)
		r.mu.Unlock()
	}
	report := r.report()
	require.Less(t, len(report.Tracks[0].Arrivals), 5000)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	var saved struct {
		Tracks []struct {
			MediaGaps []struct{ EndedAt, Duration time.Duration }
		}
	}
	require.NoError(t, json.Unmarshal(encoded, &saved))
	require.Len(t, saved.Tracks[0].MediaGaps, 2, "mid-run gaps must survive the rolling recorder history")
}

func TestRecorderGapInventoryIsBoundedAndCountsOverflow(t *testing.T) {
	r := newRecorder()
	track := r.addTrack(kindAudio, 1, 111)
	at := time.Second
	for i := 0; i < MaxRetainedMediaGaps+4; i++ {
		at += 200 * time.Millisecond
		r.packet(track, &rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i)}}, r.start.Add(at))
	}
	report := r.report().Tracks[0]
	require.True(t, report.MediaGapsRecorded)
	require.Len(t, report.MediaGaps, MaxRetainedMediaGaps)
	require.Equal(t, 3, report.MediaGapOverflow)
}
