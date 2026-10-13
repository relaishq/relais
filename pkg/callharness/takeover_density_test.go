package callharness

import (
	"cmp"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Source rate is 50 Hz Opus and 30 fps VP8 (every tenth frame has two RTP
// packets). A shared relay drain interleaves held audio/video packets over 25 ms,
// followed by gated live media. Returns and decode completions have µs jitter.
// The previous takeover has the same replay pattern; its recovery-floor interval
// covers the first fresh audio/video returns after draining the replay queue.
func replayDensityCall(t *testing.T, kind string, width time.Duration) (*recorder, MoveReport, DurationInterval) {
	t.Helper()
	r := newRecorder()
	r.start = time.Now().Add(-densityTotal)
	r.hungUpAt = densityTotal
	r.recording = RecordingOptions{History: 180 * time.Second}
	r.frameInterval = time.Second / 30
	audio := &trackRecord{kind: kindAudio}
	video := &trackRecord{kind: kindVideo, video: &videoRecord{}}
	r.tracks = []*trackRecord{audio, video}
	type packet struct {
		at    time.Duration
		video bool
		key   int
		last  bool
	}
	var packets []packet
	for i := 0; time.Duration(i)*20*time.Millisecond < densityTotal; i++ {
		at := time.Duration(i) * 20 * time.Millisecond
		u := contentUnit{at: at, returnedAt: at + 30*time.Millisecond + time.Duration((i*137)%997)*time.Microsecond, duration: 20 * time.Millisecond, written: true}
		r.sentAudioUnits[fmt.Sprint("a", i)] = u
		packets = append(packets, packet{at: at, key: i, last: true})
	}
	for i := 0; time.Duration(i)*time.Second/30 < densityTotal; i++ {
		at := time.Duration(i) * time.Second / 30
		f := sentVideoFrame{identity: uint64(i), at: at, returnedAt: at + 35*time.Millisecond + time.Duration((i*193)%991)*time.Microsecond, duration: time.Second / 30, written: true}
		r.sentVideoFrames[uint16(i)] = f
		n := 1
		if i%10 == 0 {
			n = 2
		}
		for j := 0; j < n; j++ {
			packets = append(packets, packet{at: at + time.Duration(j)*100*time.Microsecond, video: true, key: i, last: j == n-1})
		}
	}
	r.sentVideo.Frames = len(r.sentVideoFrames)
	slices.SortFunc(packets, func(a, b packet) int { return cmp.Compare(a.at, b.at) })
	var aseq, vseq uint16
	for _, p := range packets {
		ret := p.at + 30*time.Millisecond + time.Duration((p.key*137)%997)*time.Microsecond
		if p.video {
			ret = p.at + 35*time.Millisecond + time.Duration((p.key*193)%991)*time.Microsecond
		}
		held := false
		for _, kill := range []time.Duration{4 * time.Second, 10 * time.Second} {
			resume := kill + 480*time.Millisecond
			if p.at >= kill-40*time.Millisecond && p.at < resume {
				// About 41 packets are held over the 480 ms outage, plus overlap with
				// the previous checkpoint. Drain at roughly 1.6 packets/ms, no instant dump.
				offset := int((p.at - (kill - 40*time.Millisecond)) * 83 / time.Second)
				ret = resume + time.Duration(offset)*610*time.Microsecond + time.Duration((p.key*73)%251)*time.Microsecond
				held = true
			} else if p.at >= resume && p.at < resume+25*time.Millisecond {
				ret = max(ret, resume+25*time.Millisecond+time.Duration((p.key*79)%499)*time.Microsecond)
			}
		}
		track := audio
		seq := &aseq
		if p.video {
			track = video
			seq = &vseq
		}
		track.arrivals = append(track.arrivals, ret)
		track.headers = append(track.headers, rtpMark{seq: *seq})
		*seq++
		if !p.last {
			continue
		}
		if p.video {
			f := r.sentVideoFrames[uint16(p.key)]
			repeated := held && p.at < f.at+40*time.Millisecond && (f.at >= 3960*time.Millisecond && f.at < 4*time.Second || f.at >= 9960*time.Millisecond && f.at < 10*time.Second)
			if repeated {
				video.arrivals = append(video.arrivals, f.returnedAt)
				video.headers = append(video.headers, rtpMark{})
				video.video.frames = append(video.video.frames, frameMark{at: f.returnedAt, receivedAt: f.returnedAt, firstArrival: f.returnedAt, decodable: true, source: f, pictureID: uint16(p.key)})
			}
			if !repeated {
				f.returnedAt = ret
				r.sentVideoFrames[uint16(p.key)] = f
			}
			mark := frameMark{at: ret + time.Duration((p.key*31)%311)*time.Microsecond, receivedAt: ret, firstArrival: ret - 100*time.Microsecond, decodable: true, source: f, pictureID: uint16(p.key), repeated: repeated, restartsDecodeChain: held && (p.key == 119 || p.key == 299)}
			video.video.frames = append(video.video.frames, mark)
			if repeated {
				u := f.unit()
				u.returnedAt = ret
				u.replayed = true
				u.identity = strconv.Itoa(p.key) + ":" + strconv.Itoa(p.key)
				r.repeatedVideoReturns = append(r.repeatedVideoReturns, u)
			}
		} else {
			key := fmt.Sprint("a", p.key)
			u := r.sentAudioUnits[key]
			repeated := held && (p.at >= 3960*time.Millisecond && p.at < 4*time.Second || p.at >= 9960*time.Millisecond && p.at < 10*time.Second)
			if repeated {
				audio.arrivals = append(audio.arrivals, u.returnedAt)
				audio.headers = append(audio.headers, rtpMark{})
				u.identity = key
				u.returnedAt = ret
				u.replayed = true
				r.repeatedAudioReturns = append(r.repeatedAudioReturns, u)
			} else {
				u.returnedAt = ret
				r.sentAudioUnits[key] = u
			}
		}
	}
	// Sort paired observations after shared drain/jitter, retaining the worker's
	// sequence margin at the first packet of each tenure.
	for _, track := range r.tracks {
		type observed struct {
			at time.Duration
			h  rtpMark
		}
		pairs := make([]observed, len(track.arrivals))
		for i, at := range track.arrivals {
			pairs[i] = observed{at, track.headers[i]}
		}
		slices.SortFunc(pairs, func(a, b observed) int { return cmp.Compare(a.at, b.at) })
		var margin uint16
		for i, p := range pairs {
			if i > 0 && p.at-pairs[i-1].at > 100*time.Millisecond {
				margin += 8192
			}
			track.arrivals[i] = p.at
			track.headers[i].seq = uint16(i) + margin
		}
	}
	slices.SortFunc(video.video.frames, func(a, b frameMark) int { return cmp.Compare(a.at, b.at) })
	clock, err := NewClockTracker(0)
	require.NoError(t, err)
	// caller RTT 6 ms; worker ±3 ms gives 12 ms; use symmetric widths here
	// to compare identical start/end axes at 10, 12 and 15 ms.
	rtt := 6 * time.Millisecond
	mid := r.start.Round(0).Add(rtt / 2)
	require.NoError(t, clock.Add(ClockProbe{CallerSend: r.start, CallerReceive: r.start.Add(rtt), ServerReceive: mid, ServerSend: mid}))
	bound := (width - rtt) / 2
	r.distributed = &DistributedOptions{RunID: "run", Clock: clock, ControlOffset: OffsetInterval{Lower: -bound, Upper: bound}, WorkerOffset: OffsetInterval{Lower: -bound, Upper: bound}}
	c := &Call{rec: r, remoteUfrag: "session"}
	end := 10492 * time.Millisecond
	if kind == "move" {
		end = 10030 * time.Millisecond
	}
	recordDenseEvent(t, r, c, "event", kind, 10*time.Second, end)
	r.prepareMeasurementIndex()
	floor := DurationInterval{Lower: 4530*time.Millisecond - width/2, Upper: 4530*time.Millisecond + width/2}
	if kind == "move" {
		floor = DurationInterval{}
	}
	return r, budgetPoint(r, 0, r.hungUpAt, floor.Lower), floor
}

func TestTakeoverDensityMeasurement(t *testing.T) {
	if os.Getenv("RELAIS_MEASURE_CALLER_COST") != "1" {
		t.Skip("opt-in uncapped CPU measurement")
	}
	prev := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(prev)
	for _, width := range []time.Duration{10 * time.Millisecond, 12 * time.Millisecond, 15 * time.Millisecond} {
		for _, kind := range []string{"move", "takeover"} {
			r, point, floor := replayDensityCall(t, kind, width)
			ref := []FirstBaselines{firstBaselinesOf(point.Measurement)}
			cpu := processCPUSeconds()
			began := time.Now()
			r.pre123Classify(&point, r.hungUpAt, floor, ref, false, 1<<30)
			t.Logf("before kind=%s width=%s cells=%d cpu=%.6fs wall=%s", kind, width, point.Distributed.Evaluations, processCPUSeconds()-cpu, time.Since(began))
			after := budgetPoint(r, 0, r.hungUpAt, floor.Lower)
			cpu = processCPUSeconds()
			began = time.Now()
			r.classifyDistributedEventWithBudget(&after, r.hungUpAt, floor, ref, false, 1<<30)
			t.Logf("after kind=%s width=%s cells=%d cpu=%.6fs wall=%s", kind, width, after.Distributed.Evaluations, processCPUSeconds()-cpu, time.Since(began))
			requireGridEvidence(t, point, after)
		}
	}
}

func requireGridEvidence(t *testing.T, before, after MoveReport) {
	t.Helper()
	require.Equal(t, contentVerdicts(&before.Measurement), contentVerdicts(&after.Measurement))
	require.Equal(t, before.Distributed.Durations, after.Distributed.Durations)
	require.Equal(t, before.Distributed.BaselineFloor, after.Distributed.BaselineFloor)
	require.Equal(t, before.Distributed.FirstBaselines, after.Distributed.FirstBaselines)
	require.Equal(t, before.Distributed.WindowOutcome, after.Distributed.WindowOutcome)
}

func TestTakeoverGridEvidenceEquivalent(t *testing.T) {
	for _, fixture := range []string{"dense", "jittered", "replay", "recent-floor"} {
		for _, kind := range []string{"move", "takeover"} {
			for _, width := range []time.Duration{10 * time.Millisecond, 12 * time.Millisecond, 15 * time.Millisecond} {
				t.Run(fmt.Sprint(fixture, "/", kind, "/", width), func(t *testing.T) {
					var r *recorder
					var point MoveReport
					floor := DurationInterval{}
					if fixture == "replay" || fixture == "recent-floor" {
						r, point, floor = replayDensityCall(t, kind, width)
						if fixture == "recent-floor" {
							floor = DurationInterval{Lower: 9930 * time.Millisecond, Upper: 9930*time.Millisecond + width}
						}
					} else {
						var c *Call
						r, c = denseDistributedCall(t, true, 6*time.Millisecond, (width-6*time.Millisecond)/2)
						if fixture == "jittered" {
							jitterBudgetFixture(r)
						}
						end := 5030 * time.Millisecond
						if kind == "takeover" {
							end = 5500 * time.Millisecond
						}
						recordDenseEvent(t, r, c, "event", kind, 5*time.Second, end)
						r.prepareMeasurementIndex()
						point = budgetPoint(r, 0, r.hungUpAt, 0)
					}
					refs := []FirstBaselines{firstBaselinesOf(point.Measurement)}
					before := budgetPoint(r, 0, r.hungUpAt, floor.Lower)
					after := budgetPoint(r, 0, r.hungUpAt, floor.Lower)
					r.pre123Classify(&before, r.hungUpAt, floor, refs, false, 1<<30)
					r.classifyDistributedEventWithBudget(&after, r.hungUpAt, floor, refs, false, 1<<30)
					requireGridEvidence(t, before, after)
				})
			}
		}
	}
}

func TestTakeoverBudgetCrossover(t *testing.T) {
	r, point, floor := replayDensityCall(t, "takeover", 15*time.Millisecond)
	refs := []FirstBaselines{firstBaselinesOf(point.Measurement)}
	old := budgetPoint(r, 0, r.hungUpAt, floor.Lower)
	r.pre123Classify(&old, r.hungUpAt, floor, refs, false, 4096)
	require.Equal(t, 4096, old.Distributed.Evaluations)
	require.Nil(t, old.Distributed.FirstBaselines)
	oracle := budgetPoint(r, 0, r.hungUpAt, floor.Lower)
	r.pre123Classify(&oracle, r.hungUpAt, floor, refs, false, 1<<30)
	actual := budgetPoint(r, 0, r.hungUpAt, floor.Lower)
	r.classifyDistributedEvent(&actual, r.hungUpAt, floor, refs, false)
	require.NotNil(t, actual.Distributed.FirstBaselines, "real replay event must complete within production cap")
	requireGridEvidence(t, oracle, actual)
}

func TestTakeoverReplayFixtureGeometry(t *testing.T) {
	r, _, _ := replayDensityCall(t, "takeover", 15*time.Millisecond)
	require.Len(t, r.sentAudioUnits, 9250)
	require.Len(t, r.sentVideoFrames, 5550)
	for _, resume := range []time.Duration{4480 * time.Millisecond, 10480 * time.Millisecond} {
		n := 0
		for _, track := range r.tracks {
			for _, at := range track.arrivals {
				if at >= resume && at < resume+25*time.Millisecond {
					n++
				}
			}
		}
		require.GreaterOrEqual(t, n, 40)
		require.LessOrEqual(t, n, 45)
		t.Logf("shared replay: %d packets in 25ms at %s", n, resume)
	}
	require.NotEmpty(t, r.repeatedAudioReturns)
	require.NotEmpty(t, r.repeatedVideoReturns)
	require.GreaterOrEqual(t, len(r.tracks[1].arrivals), 6105) // 1.1 RTP packets/frame
}

func TestClassificationTwoPointRegionCoverage(t *testing.T) {
	// Independent exhaustive integer-domain proof of constant branch signatures.
	// Includes isolated equality points, adjacent boundaries and clipped endpoints.
	for mask := 0; mask < 1<<9; mask++ {
		var boundaries []time.Duration
		for at := 0; at < 9; at++ {
			if mask&(1<<at) != 0 {
				boundaries = append(boundaries, time.Duration(at))
			}
		}
		signature := func(at time.Duration) string {
			s := ""
			for _, b := range boundaries {
				s += fmt.Sprint(cmp.Compare(at, b))
			}
			return s
		}
		represented := map[string]bool{}
		for _, at := range classificationPoints(0, 8, boundaries) {
			represented[signature(at)] = true
		}
		for at := time.Duration(0); at <= 8; at++ {
			require.True(t, represented[signature(at)], "mask=%d at=%d", mask, at)
		}
	}
	// Start has affine elapsed durations after a branch jump, so both open-region
	// extrema remain necessary even when their verdict signatures are identical.
	require.NotContains(t, classificationPoints(0, 10, []time.Duration{5, 9}), time.Duration(6))
	require.Contains(t, classificationStartPoints(0, 10, []time.Duration{5, 9}), time.Duration(6))
}

func TestTakeoverCapCPUMeasurement(t *testing.T) {
	if os.Getenv("RELAIS_MEASURE_CALLER_COST") != "1" {
		t.Skip("opt-in capped process CPU measurement")
	}
	prev := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(prev)
	// Wider intervals and a recent, active floor force a full capped evaluation.
	// Four distinct baseline references also exercise per-reference work within
	// each cell. This is cap stress, not a claim about observed clock widths.
	for _, nrefs := range []int{1, 4} {
		r, point, _ := replayDensityCall(t, "takeover", 150*time.Millisecond)
		floor := DurationInterval{Lower: 9500 * time.Millisecond, Upper: 9800 * time.Millisecond}
		ref := firstBaselinesOf(point.Measurement)
		refs := make([]FirstBaselines, nrefs)
		for i := range refs {
			refs[i] = ref
			refs[i].Audio.Samples += i
		}
		var worst float64
		for range 5 {
			p := budgetPoint(r, 0, r.hungUpAt, floor.Lower)
			cpu := processCPUSeconds()
			r.classifyDistributedEvent(&p, r.hungUpAt, floor, refs, false)
			worst = max(worst, processCPUSeconds()-cpu)
			require.Equal(t, maxClassificationEvaluations, p.Distributed.Evaluations)
			require.Nil(t, p.Distributed.FirstBaselines)
		}
		t.Logf("cap=%d refs=%d worst_cpu=%.6fs (5 trials)", maxClassificationEvaluations, nrefs, worst)
		if !densityRaceDetector && os.Getenv("RELAIS_TEST_STRICT_TIMING") == "1" {
			require.Less(t, worst, 0.25, "capped event process CPU budget")
		}
	}
}

func TestTakeoverBudgetMargin(t *testing.T) {
	r, _, floor := replayDensityCall(t, "takeover", 15*time.Millisecond)
	event := r.moves[0].event
	start, end := event.Start, event.End
	worst := 0
	for _, startShift := range []time.Duration{-2 * time.Millisecond, 0, 2 * time.Millisecond} {
		for _, endShift := range []time.Duration{-4 * time.Millisecond, -time.Millisecond, time.Millisecond, 4 * time.Millisecond} {
			event.Start, event.End = start.Add(startShift), end.Add(endShift)
			point := budgetPoint(r, 0, r.hungUpAt, floor.Lower)
			refs := []FirstBaselines{firstBaselinesOf(point.Measurement)}
			r.classifyDistributedEventWithBudget(&point, r.hungUpAt, floor, refs, false, 1<<30)
			worst = max(worst, point.Distributed.Evaluations)
			require.NotNil(t, point.Distributed.FirstBaselines)
		}
	}
	require.GreaterOrEqual(t, maxClassificationEvaluations, 2*worst, "at least 2x replay phase margin")
	t.Logf("15ms phase sweep: worst=%d cells, cap=%d, margin=%.2fx", worst, maxClassificationEvaluations, float64(maxClassificationEvaluations)/float64(worst))
}
