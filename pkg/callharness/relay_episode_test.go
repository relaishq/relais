package callharness

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/relayloss"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func relayEpisodeCall() *Call {
	r := newRecorder()
	r.relayLossEvidence = true
	// The 2.1s packet was in flight at the synthetic 2s fault. Restart starts
	// at 4s, after stall detection; L must include that in-flight packet.
	r.tracks = []*trackRecord{
		{kind: kindAudio, arrivals: []time.Duration{time.Second, 2100 * time.Millisecond, 6100 * time.Millisecond}},
		{kind: kindVideo, arrivals: []time.Duration{1500 * time.Millisecond, 6 * time.Second}, video: &videoRecord{frames: []frameMark{{at: 6 * time.Second, decodable: true}}}},
	}
	return &Call{rec: r}
}

func TestRelayLossEpisodeAnchorsAndArrivalCounts(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(map[bool]string{false: "isolated", true: "early media refuses ordering"}[early], func(t *testing.T) {
			c := relayEpisodeCall()
			r := c.rec
			index := r.beginRelayRestart(r.start.Add(4 * time.Second))
			r.appliedRelayRestart(index, r.start.Add(5*time.Second), 1, "192.0.2.2:3478")
			if early {
				// A receiver may hold a pre-application timestamp before it
				// acquires the recorder lock. Count it when observation settles.
				r.tracks[0].arrivals = []time.Duration{time.Second, 2100 * time.Millisecond, 4500 * time.Millisecond, 6100 * time.Millisecond}
			}
			r.observeRelayRestarts(r.start.Add(7 * time.Second))
			rows := c.RelayLossEpisodes()
			require.Len(t, rows, 1)
			got := rows[0]
			require.Equal(t, 4*time.Second, got.RestartStartNS)
			require.Equal(t, 5*time.Second, got.AppliedNS)
			require.Equal(t, uint64(1), got.Generation)
			require.Equal(t, "192.0.2.2:3478", got.AnswerCandidate)
			require.Equal(t, 2100*time.Millisecond, got.LastBeforeNS)
			require.Equal(t, 6*time.Second, got.FirstVideoNS)
			require.Equal(t, 6100*time.Millisecond, got.FirstAudioNS)
			require.Len(t, got.ArrivalsBeforeApplied, 2)
			want := 0
			if early {
				want = 1 // The R judge must reject this row; a numeric gap cannot pass it.
			}
			require.Equal(t, want, got.ArrivalsBeforeApplied[0].Count)
			require.Zero(t, got.ArrivalsBeforeApplied[1].Count)
			// Returned evidence cannot mutate the recorder or a later snapshot.
			rows[0].ArrivalsBeforeApplied[0].Count = 42
			require.Equal(t, want, c.RelayLossEpisodes()[0].ArrivalsBeforeApplied[0].Count)
		})
	}
}

func TestRelayLossEpisodeObservationIsBounded(t *testing.T) {
	c := relayEpisodeCall()
	r := c.rec
	index := r.beginRelayRestart(r.start.Add(4 * time.Second))
	require.Equal(t, index, r.beginRelayRestart(r.start.Add(4500*time.Millisecond)), "a retry before an answer keeps the first anchor")
	r.appliedRelayRestart(index, r.start.Add(5*time.Second), 1, "192.0.2.2:3478")
	r.tracks[0].arrivals[2] = 4*time.Second + 2*relayloss.Budget + time.Nanosecond
	r.tracks[1].video.frames[0].at = r.tracks[0].arrivals[2]
	r.observeRelayRestarts(r.start.Add(4*time.Second + 2*relayloss.Budget))
	before := c.RelayLossEpisodes()
	require.Zero(t, before[0].FirstAudioNS)
	require.Zero(t, before[0].FirstVideoNS)
	r.observeRelayRestarts(r.start.Add(time.Minute))
	require.Equal(t, before, c.RelayLossEpisodes(), "late recovery cannot repair the observation")
}

func TestRelayLossCountIncludesAnswerBoundary(t *testing.T) {
	c := relayEpisodeCall()
	r := c.rec
	index := r.beginRelayRestart(r.start.Add(4 * time.Second))
	r.appliedRelayRestart(index, r.start.Add(5*time.Second), 1, "192.0.2.2:3478")
	r.tracks[0].arrivals[2] = 5 * time.Second
	r.observeRelayRestarts(r.start.Add(7 * time.Second))
	row := c.RelayLossEpisodes()[0]
	require.Equal(t, row.AppliedNS, row.FirstAudioNS)
	require.Equal(t, 1, row.ArrivalsBeforeApplied[0].Count, "(L, applied] includes equality; the R judge must refuse it")
}

func TestRelayLossEvidenceIsOptIn(t *testing.T) {
	c := recoveryPointCall()
	require.Equal(t, -1, c.rec.beginRelayRestart(c.rec.start.Add(3*time.Second)))
	require.Empty(t, c.RelayLossEpisodes())
}

func TestRelayLossEpisodeRecordsAppliedAnswer(t *testing.T) {
	p, _, h, relays, _ := relayChangeTopology(t, sessionstore.NewMemory(), false, false)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	call, err := h.Dial(ctx, CallOptions{Video: true, RelayChange: true, RelayLossEvidence: true, Recording: RecordingOptions{DecodeEvery: 30}})
	require.NoError(t, err)
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 15*time.Second) }()
	defer func() { cancel(); <-sent }()
	require.Eventually(t, func() bool {
		return len(call.RelaySnapshot().Tracks) == 2
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, p.ChangeRelay(ctx, call.SessionID(), "r2"))
	require.Eventually(t, func() bool {
		rows := call.RelayLossEpisodes()
		return len(rows) == 1 && rows[0].FirstVideoNS > 0 && rows[0].FirstAudioNS > 0
	}, 5*time.Second, 10*time.Millisecond)
	row := call.RelayLossEpisodes()[0]
	require.Less(t, row.LastBeforeNS, row.RestartStartNS)
	require.LessOrEqual(t, row.RestartStartNS, row.AppliedNS)
	require.LessOrEqual(t, row.AppliedNS, row.FirstVideoNS)
	require.LessOrEqual(t, row.AppliedNS, row.FirstAudioNS)
	require.EqualValues(t, 1, row.Generation)
	require.Equal(t, relays[1].PublicAddr().String(), row.AnswerCandidate)
	// This planned change validates hook wiring, not the unplanned-loss order.
	require.Len(t, row.ArrivalsBeforeApplied, 2)
}
