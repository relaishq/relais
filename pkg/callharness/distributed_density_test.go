package callharness

import (
	"cmp"
	"fmt"
	"github.com/stretchr/testify/require"
	"slices"
	"testing"
	"time"

	"github.com/relais/pkg/controlplane"
)

// jitter is a deterministic 0..4ms pseudo-random offset.
func densityJitter(i int) time.Duration { return time.Duration((i*7919)%5) * time.Millisecond }

// A realistic-density call: 50 Hz audio and (optionally) 30 fps video, each
// returned ~30ms later. Takeover outage 10.0..10.6s; outage sends are
// delivered late after resume (no loss). A move at 5s with a 40ms gap.
func denseDistributedCall(t *testing.T, video bool, rtt, bound time.Duration) (*recorder, *Call) {
	t.Helper()
	total := densityTotal
	r := newRecorder()
	r.start = time.Now().Add(-total)
	r.hungUpAt = total
	r.recording = RecordingOptions{History: 180 * time.Second}
	audio := &trackRecord{kind: kindAudio}
	seq := uint16(0)
	i := 0
	for at := time.Duration(0); at < total; at += 20 * time.Millisecond {
		ret := at + 30*time.Millisecond + densityJitter(i)
		if at >= 10*time.Second && at < 10600*time.Millisecond {
			ret = 10600*time.Millisecond + (at-10*time.Second)/10
		}
		if at >= 5*time.Second && at < 5040*time.Millisecond {
			ret = 5040*time.Millisecond + 30*time.Millisecond
		}
		r.sentAudioUnits[fmt.Sprint("a", i)] = contentUnit{at: at, returnedAt: ret, duration: 20 * time.Millisecond, written: true}
		i++
	}
	for at := 30 * time.Millisecond; at < total; at += 20 * time.Millisecond {
		if at > 10*time.Second && at < 10600*time.Millisecond || at > 5*time.Second && at < 5040*time.Millisecond {
			continue
		}
		if len(audio.arrivals) > 0 && audio.arrivals[len(audio.arrivals)-1] < 10*time.Second && at >= 10600*time.Millisecond {
			seq += 8192
		}
		audio.arrivals = append(audio.arrivals, at)
		audio.headers = append(audio.headers, rtpMark{seq: seq})
		seq++
	}
	r.tracks = []*trackRecord{audio}
	if video {
		r.sentVideo.Frames = int(total / (time.Second / 30))
		r.frameInterval = time.Second / 30
		vt := &trackRecord{kind: kindVideo, video: &videoRecord{}}
		vseq := uint16(0)
		for f := 0; f < int(total/(time.Second/30)); f++ {
			at := time.Duration(f) * time.Second / 30
			ret := at + 35*time.Millisecond + densityJitter(f)
			if at >= 10*time.Second && at < 10600*time.Millisecond {
				ret = 10600*time.Millisecond + (at-10*time.Second)/10
			}
			src := sentVideoFrame{identity: uint64(f), at: at, returnedAt: ret, duration: time.Second / 30, written: true}
			r.sentVideoFrames[uint16(f)] = src
			vt.video.frames = append(vt.video.frames, frameMark{at: ret, receivedAt: ret, firstArrival: ret - time.Millisecond, decodable: true, source: src, pictureID: uint16(f), restartsDecodeChain: f%60 == 0})
			for p := 0; p < 3; p++ {
				if len(vt.arrivals) > 0 && vt.arrivals[len(vt.arrivals)-1] < 10*time.Second && ret >= 10600*time.Millisecond {
					vseq += 8192
				}
				vt.arrivals = append(vt.arrivals, ret-time.Duration(2-p)*time.Millisecond/2)
				vt.headers = append(vt.headers, rtpMark{seq: vseq})
				vseq++
			}
		}
		r.tracks = append(r.tracks, vt)
	}
	clock, err := NewClockTracker(0)
	if err != nil {
		t.Fatal(err)
	}
	mid := r.start.Round(0).Add(rtt / 2)
	if err := clock.Add(ClockProbe{CallerSend: r.start, CallerReceive: r.start.Add(rtt), ServerReceive: mid, ServerSend: mid}); err != nil {
		t.Fatal(err)
	}
	b := OffsetInterval{Lower: -bound, Upper: bound}
	r.distributed = &DistributedOptions{RunID: "run", Clock: clock, ControlOffset: b, WorkerOffset: b}
	return r, &Call{rec: r, remoteUfrag: "session"}
}

func recordDenseEvent(t *testing.T, r *recorder, c *Call, op, kind string, start, end time.Duration) {
	id := OperationIdentity{RunID: "run", OperationID: op, SessionID: "session", Worker: "0", Epoch: 1}
	if r.armed == nil {
		r.armed = map[OperationIdentity]time.Time{}
	}
	r.armed[id] = r.start.Add(start - 200*time.Millisecond)
	e := DistributedEvent{Identity: id, Kind: kind, Start: r.start.Round(0).Add(start), End: r.start.Round(0).Add(end), Result: controlplane.MoveResult{ID: "session", From: "0", To: "1", Kind: kind}}
	if err := c.RecordDistributedEvent(e); err != nil {
		t.Fatal(err)
	}
}

const densityTotal = 185 * time.Second

func TestDistributedLongHistoryDoesNotHoldCallerLock(t *testing.T) {
	r, c := denseDistributedCall(t, true, 50*time.Millisecond, time.Millisecond)
	recordDenseEvent(t, r, c, "move-1", "move", 5*time.Second, 5030*time.Millisecond)
	recordDenseEvent(t, r, c, "kill-1", "takeover", 10*time.Second, 10500*time.Millisecond)
	// Install the production time indexes before timing. Fixture setup itself
	// bypasses the sending/receiving methods that append these indexes.
	for key, u := range r.sentAudioUnits {
		r.audioSends = append(r.audioSends, evidenceRef{at: u.at, audio: key})
		r.audioReturns = append(r.audioReturns, evidenceRef{at: u.returnedAt, audio: key})
	}
	for key, f := range r.sentVideoFrames {
		r.videoSends = append(r.videoSends, evidenceRef{at: f.at, video: key, generation: f.identity})
		r.videoReturns = append(r.videoReturns, evidenceRef{at: f.returnedAt, video: key, generation: f.identity})
	}
	for _, refs := range [][]evidenceRef{r.audioSends, r.audioReturns, r.videoSends, r.videoReturns} {
		slices.SortFunc(refs, func(a, b evidenceRef) int { return cmp.Compare(a.at, b.at) })
	}
	// The real maintenance path snapshots an expiring move and takeover while
	// sender/receiver goroutines would continue using r.mu during classification.
	ready, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	began := time.Now()
	go func() {
		r.compactRecordingBeforeEvaluation(densityTotal+5*time.Second, func() { close(ready); <-release })
		close(done)
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("snapshot handoff did not finish")
	}
	// A paused classification handoff must leave the sender/receiver lock
	// available. This proof is deterministic, including under race overhead.
	available := r.mu.TryLock()
	if available {
		r.mu.Unlock()
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("interval evaluation did not finish")
	}
	require.True(t, available, "interval evaluation must not retain the caller recorder lock")
	require.Len(t, r.savedMoves, 2)
	held := r.measurementLockTime
	t.Logf("185s, audio 50Hz/video 30fps, RTT50ms: maximum lock=%s (maximum phase %s), evaluation=%s, cells=%d/%d", held, r.measurementLockTime, time.Since(began), r.savedMoves[0].Distributed.Evaluations, r.savedMoves[1].Distributed.Evaluations)
	// Race instrumentation changes copying costs. The native performance gate
	// stays at 5ms; race still checks the actual lock handoff above.
	if !densityRaceDetector {
		require.Less(t, held, 5*time.Millisecond)
	}
}

func TestDistributedPruningKeepsRetriedPictureIdentity(t *testing.T) {
	r := newRecorder()
	r.recording.History = 180 * time.Second
	r.distributed = &DistributedOptions{RunID: "run"}
	// Failed writes do not advance the identity counter. A confirmed retry can
	// reuse the same identity, so expiry must also match the original send time.
	r.sentVideoFrames[0] = sentVideoFrame{identity: 0, at: 25 * time.Second, written: true}
	r.videoSends = []evidenceRef{{at: time.Second, video: 0, generation: 0}, {at: 25 * time.Second, video: 0, generation: 0}}
	r.compactRecording(200 * time.Second)
	frame, ok := r.sentVideoFrames[0]
	require.True(t, ok, "expiry of the failed send must not erase its later confirmed retry")
	require.Equal(t, 25*time.Second, frame.at)
}
