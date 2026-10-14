package callharness

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/srtp/v3"
	"github.com/stretchr/testify/require"
)

// Pre-#125 maintenance: before a call outlived its retention, the distributed
// path returned without expiring idle video assemblies. #125 changed only that
// branch, so every other tick delegates to the production path.
func (r *recorder) legacyCompactRecording(now time.Duration) {
	r.mu.Lock()
	skip := r.distributed != nil && !r.hungUp && now-r.lastCompact >= time.Second && now-r.recording.History <= 0
	if skip {
		r.lastCompact = now
	}
	r.mu.Unlock()
	if !skip {
		r.compactRecording(now)
	}
}

type firstWindowEvent struct {
	op, kind   string
	start, end time.Duration
}

// firstWindowScenario describes one replayed call. By default every picture
// has three fragments and packets reach the recorder directly.
type firstWindowScenario struct {
	name           string
	total, history time.Duration
	events         []firstWindowEvent
	// frameBytes sizes picture f; 1000-byte payloads split larger pictures.
	frameBytes func(f int) int
	// markerDelay delays a noisy picture's marker; zero means 900ms.
	markerDelay time.Duration
	// srtp authenticates video through an SRTP context with the caller's
	// default 64-packet replay window. Rejected packets never reach the recorder.
	srtp bool
}

type firstWindowStep struct {
	at    time.Duration
	order int
	do    func()
}

// firstWindowCall replays 50Hz audio and 30fps video through the production
// packet path with one maintenance tick per second. A planned
// move pauses returns for 40ms. A takeover pauses them for 600ms; the new
// worker then replays the last 1.2s of echoed pictures with a sequence
// margin and the same timestamps (crossing the one-second assembly expiry),
// completes the frame cut off by the kill, and returns the outage sends late.
// From 1.5s to 3s after each event, some fragmented frames also get an exact
// duplicate fragment (40ms later), an exact duplicate frame (300ms later) or a
// delayed marker fragment.
func firstWindowCall(t *testing.T, base time.Time, s firstWindowScenario, compact func(*recorder, time.Duration)) (*Report, *recorder) {
	t.Helper()
	const frame = time.Second / 30
	total, history, events := s.total, s.history, s.events
	markerDelay := s.markerDelay
	if markerDelay == 0 {
		markerDelay = 900 * time.Millisecond
	}
	r := newRecorder()
	r.start = base
	r.recording = RecordingOptions{History: history}
	r.frameInterval = frame
	audio := r.addTrack(kindAudio, 1, 111)
	video := r.addTrack(kindVideo, 2, 96)
	clock, err := NewClockTracker(0)
	require.NoError(t, err)
	mid := base.Round(0).Add(time.Millisecond)
	require.NoError(t, clock.Add(ClockProbe{CallerSend: base, CallerReceive: base.Add(2 * time.Millisecond), ServerReceive: mid, ServerSend: mid}))
	bound := OffsetInterval{Lower: -time.Millisecond, Upper: time.Millisecond}
	r.distributed = &DistributedOptions{RunID: "run", Clock: clock, ControlOffset: bound, WorkerOffset: bound}
	c := &Call{rec: r, remoteUfrag: "session"}
	var sender, receiver *srtp.Context
	if s.srtp {
		profile := srtp.ProtectionProfileAes128CmHmacSha1_80
		keyLen, err := profile.KeyLen()
		require.NoError(t, err)
		saltLen, err := profile.SaltLen()
		require.NoError(t, err)
		key, salt := make([]byte, keyLen), make([]byte, saltLen)
		sender, err = srtp.CreateContext(key, salt, profile)
		require.NoError(t, err)
		// pion/srtp sessions default to this window; the caller sets no other.
		receiver, err = srtp.CreateContext(key, salt, profile, srtp.SRTPReplayProtection(64))
		require.NoError(t, err)
	}

	var steps []firstWindowStep
	add := func(at time.Duration, do func()) {
		steps = append(steps, firstWindowStep{at: at, order: len(steps), do: do})
	}
	// returned maps a send to its normal echo time and reports the takeover
	// resume when the old worker could not return it.
	returned := func(at, latency time.Duration) (time.Duration, time.Duration) {
		for _, e := range events {
			switch {
			case e.kind == "takeover" && at >= e.start && at < e.start+600*time.Millisecond:
				resume := e.start + 600*time.Millisecond
				return resume + 20*time.Millisecond + (at-e.start)/10, resume
			case e.kind == "move" && at >= e.start && at < e.start+40*time.Millisecond:
				return e.start + 40*time.Millisecond + latency, 0
			}
		}
		return at + latency, 0
	}
	noisy := func(at time.Duration) bool {
		for _, e := range events {
			if at >= e.start+1500*time.Millisecond && at < e.start+3*time.Second {
				return true
			}
		}
		return false
	}
	margin := func(at time.Duration, jumped map[time.Duration]bool) uint16 {
		for _, e := range events {
			if e.kind == "takeover" && at >= e.start+600*time.Millisecond && !jumped[e.start] {
				jumped[e.start] = true
				return 8192
			}
		}
		return 0
	}

	type audioReturn struct {
		at      time.Duration
		payload []byte
	}
	var audioReturns []audioReturn
	for i, at := 0, time.Duration(0); at < total; i, at = i+1, at+20*time.Millisecond {
		payload := []byte(fmt.Sprintf("audio-%06d", i))
		key := string(payload)
		add(at, func() {
			r.sentAudioUnits[key] = contentUnit{at: at, duration: 20 * time.Millisecond, written: true}
			r.audioSends = append(r.audioSends, evidenceRef{at: at, audio: key})
			r.sentFrames[kindAudio][key] = struct{}{}
			r.sentAudio.Frames++
		})
		ret, _ := returned(at, 30*time.Millisecond+densityJitter(i))
		audioReturns = append(audioReturns, audioReturn{at: ret, payload: payload})
	}
	slices.SortStableFunc(audioReturns, func(a, b audioReturn) int { return cmp.Compare(a.at, b.at) })
	audioSeq, audioJumped := uint16(0), map[time.Duration]bool{}
	for _, ret := range audioReturns {
		if ret.at >= total {
			continue
		}
		audioSeq += margin(ret.at, audioJumped)
		pkt := &rtp.Packet{Header: rtp.Header{SequenceNumber: audioSeq, Timestamp: uint32(ret.at / time.Millisecond * 48)}, Payload: ret.payload}
		audioSeq++
		add(ret.at, func() { r.packet(audio, pkt, base.Add(ret.at)) })
	}

	// One transmission is one worker send of a picture: its own sequence span.
	type fragment struct {
		part int
		at   time.Duration
	}
	type transmission struct {
		f     int
		at    time.Duration
		parts []fragment
	}
	payloader := &codecs.VP8Payloader{EnablePictureID: true}
	var payloads [][][]byte
	var transmissions []transmission
	for f, at := 0, time.Duration(0); at < total; f, at = f+1, at+frame {
		size := 2400
		if s.frameBytes != nil {
			size = s.frameBytes(f)
		}
		data := bytes.Repeat([]byte{byte(f)}, size)
		binary.BigEndian.PutUint64(data, uint64(f))
		payloads = append(payloads, payloader.Payload(1000, data))
		n := len(payloads[f])
		id := uint16(f)
		add(at, func() {
			r.sentVideoFrames[id] = sentVideoFrame{identity: uint64(f), at: at, data: string(data), duration: frame, written: true}
			r.videoSends = append(r.videoSends, evidenceRef{at: at, video: id, generation: uint64(f)})
			r.sentFrames[kindVideo][string(data)] = struct{}{}
			r.sentVideo.Frames++
		})
		ret, resume := returned(at, 35*time.Millisecond+densityJitter(f))
		tx := transmission{f: f, at: ret - time.Millisecond}
		for j := range n {
			tx.parts = append(tx.parts, fragment{part: j, at: ret - time.Duration(n-1-j)*time.Millisecond/2})
		}
		for _, e := range events {
			if e.kind != "takeover" || resume != 0 || at >= e.start || at < e.start-1200*time.Millisecond {
				continue
			}
			replay := e.start + 600*time.Millisecond + time.Duration(f-int((e.start-1200*time.Millisecond)/frame))*100*time.Microsecond
			if at+frame >= e.start && n > 1 {
				// The kill interrupts this picture before its marker.
				tx.parts = tx.parts[:n-1]
			}
			again := transmission{f: f, at: replay}
			for j := range n {
				again.parts = append(again.parts, fragment{part: j, at: replay})
			}
			transmissions = append(transmissions, again)
		}
		if noisy(at) && n > 1 {
			switch {
			case f%14 == 0:
				tx.parts[n-1].at += markerDelay
			case f%9 == 0:
				for j := range n {
					tx.parts = append(tx.parts, fragment{part: j, at: ret + 300*time.Millisecond})
				}
			case f%5 == 0:
				tx.parts = append(tx.parts, fragment{part: 1, at: ret + 40*time.Millisecond})
			}
		}
		transmissions = append(transmissions, tx)
	}
	slices.SortStableFunc(transmissions, func(a, b transmission) int { return cmp.Compare(a.at, b.at) })
	videoSeq, videoJumped := uint16(0), map[time.Duration]bool{}
	for _, tx := range transmissions {
		videoSeq += margin(tx.at, videoJumped)
		first := videoSeq
		n := len(payloads[tx.f])
		videoSeq += uint16(n)
		var wire [][]byte
		if s.srtp {
			// Encrypt in worker send order; delivery order may differ.
			for j := range n {
				pkt := &rtp.Packet{Header: rtp.Header{Version: 2, SSRC: 2, PayloadType: 96, SequenceNumber: first + uint16(j), Timestamp: uint32(tx.f * 3000), Marker: j == n-1}, Payload: payloads[tx.f][j]}
				raw, err := pkt.Marshal()
				require.NoError(t, err)
				encrypted, err := sender.EncryptRTP(nil, raw, nil)
				require.NoError(t, err)
				wire = append(wire, encrypted)
			}
		}
		for _, part := range tx.parts {
			if part.at >= total {
				continue
			}
			pkt := &rtp.Packet{Header: rtp.Header{Version: 2, SSRC: 2, PayloadType: 96, SequenceNumber: first + uint16(part.part), Timestamp: uint32(tx.f * 3000), Marker: part.part == n-1}, Payload: payloads[tx.f][part.part]}
			id := uint16(tx.f)
			add(part.at, func() {
				if s.srtp {
					plain, err := receiver.DecryptRTP(nil, wire[part.part], nil)
					if err != nil {
						return // replayed or older than the replay window
					}
					pkt = &rtp.Packet{}
					require.NoError(t, pkt.Unmarshal(plain))
				}
				before := r.sentVideoFrames[id].returnedAt
				r.packet(video, pkt, base.Add(part.at))
				// The strict decoder path is independent of content assemblies.
				// Mark the first complete return as one decodable picture.
				if source := r.sentVideoFrames[id]; before == 0 && source.returnedAt > 0 {
					video.video.frames = append(video.video.frames, frameMark{at: part.at + time.Millisecond, receivedAt: part.at, firstArrival: part.at - time.Millisecond, decodable: true, source: source, pictureID: id, restartsDecodeChain: tx.f%60 == 0})
				}
			})
		}
	}
	for _, e := range events {
		add(e.end+100*time.Millisecond, func() { recordDenseEvent(t, r, c, e.op, e.kind, e.start, e.end) })
	}

	slices.SortStableFunc(steps, func(a, b firstWindowStep) int {
		return cmp.Or(cmp.Compare(a.at, b.at), cmp.Compare(a.order, b.order))
	})
	tick := time.Second
	for _, step := range steps {
		for ; tick <= step.at; tick += time.Second {
			compact(r, tick)
		}
		step.do()
	}
	r.hungUpAt, r.hungUp = total, true
	return r.report(), r
}

const historyExpiredReason = "event history expired before observation"

func historyExpired(m MoveReport) bool {
	for _, v := range []MetricVerdict{m.Measurement.AudioLoss, m.Measurement.VideoLoss, m.Measurement.Audio.Verdict, m.Measurement.Video.Verdict, m.Measurement.ResumeVerdict, m.Measurement.FirstContentVerdict, m.Measurement.FirstNewContentVerdict, m.Measurement.FirstLiveVerdict} {
		if slices.Contains(v.Reasons, historyExpiredReason) {
			return true
		}
	}
	return m.Distributed != nil && slices.Contains(m.Distributed.Causes, "mapped event history or settle window unavailable")
}

var firstWindowScenarios = []firstWindowScenario{
	// Hangup inside the first retention window measures every event.
	{name: "hangup-in-first-window", total: 18 * time.Second, history: 180 * time.Second, events: []firstWindowEvent{
		{"move-early", "move", 3 * time.Second, 3030 * time.Millisecond},
		{"kill", "takeover", 11500 * time.Millisecond, 12 * time.Second},
		{"move-late", "move", 16500 * time.Millisecond, 16530 * time.Millisecond},
	}},
	// Maintenance saves the first three events after the window has ended.
	{name: "maintenance-after-first-window", total: 36 * time.Second, history: 20 * time.Second, events: []firstWindowEvent{
		{"move-early", "move", 3 * time.Second, 3030 * time.Millisecond},
		{"kill", "takeover", 11500 * time.Millisecond, 12 * time.Second},
		{"kill-mid", "takeover", 15 * time.Second, 15500 * time.Millisecond},
		{"move-late", "move", 30 * time.Second, 30030 * time.Millisecond},
	}},
}

// requireSameJudgment compares an event the old maintenance evaluated. Only
// content-loss comparison windows may extend past the old eviction watermark.
func requireSameJudgment(t *testing.T, old, now MoveReport) {
	t.Helper()
	oldBaselines, newBaselines := old.Measurement.ContentLoss.Baselines, now.Measurement.ContentLoss.Baselines
	require.LessOrEqual(t, len(oldBaselines), len(newBaselines))
	for j := range oldBaselines {
		require.Equal(t, oldBaselines[j], newBaselines[j])
	}
	old.Measurement.ContentLoss.Baselines, now.Measurement.ContentLoss.Baselines = nil, nil
	require.Equal(t, old, now, old.Identity.OperationID)
}

// #125: idle completed assemblies once filled the 256-entry limit in about
// 8.5s, and each eviction advanced historyStart over retained evidence.
func TestDistributedFirstWindowAssembliesKeepHistory(t *testing.T) {
	for _, scenario := range firstWindowScenarios {
		t.Run(scenario.name, func(t *testing.T) {
			base := time.Now().Add(-scenario.total - time.Second)
			rep, _ := firstWindowCall(t, base, scenario, (*recorder).compactRecording)
			require.Equal(t, max(0, scenario.total-time.Second-scenario.history), rep.Recording.HistoryStart, "only time-based retention moves the boundary")
			require.LessOrEqual(t, rep.Recording.Assemblies, 4*30, "completed assemblies expire after a second of idleness")
			require.Len(t, rep.Moves, len(scenario.events))
			for _, m := range rep.Moves {
				require.False(t, historyExpired(m), "%s: %s %v", m.Identity.OperationID, m.Measurement.Summary(), m.Distributed.Causes)
				require.True(t, m.Measurement.AudioLoss.Trusted, "%s: %s", m.Identity.OperationID, m.Measurement.Summary())
				require.True(t, m.Measurement.ResumeVerdict.Trusted, "%s: %s", m.Identity.OperationID, m.Measurement.Summary())
			}
		})
	}
}

// Equivalence for #125: an event the old maintenance already evaluated keeps
// identical verdicts, reasons, durations and evidence, including duplicate,
// reordered and replayed fragments. Only content-loss comparison windows may
// extend further back, past the old eviction watermark.
func TestDistributedFirstWindowAssemblyExpiryEquivalent(t *testing.T) {
	for _, scenario := range firstWindowScenarios {
		t.Run(scenario.name, func(t *testing.T) {
			base := time.Now().Add(-scenario.total - time.Second)
			legacy, _ := firstWindowCall(t, base, scenario, (*recorder).legacyCompactRecording)
			fixed, _ := firstWindowCall(t, base, scenario, (*recorder).compactRecording)
			require.Len(t, legacy.Moves, len(scenario.events))
			require.Len(t, fixed.Moves, len(scenario.events))
			expired, compared := 0, 0
			for i := range legacy.Moves {
				old, now := legacy.Moves[i], fixed.Moves[i]
				require.Equal(t, old.Identity, now.Identity)
				require.False(t, historyExpired(now), "%s: %s", now.Identity.OperationID, now.Measurement.Summary())
				if historyExpired(old) {
					expired++
					continue
				}
				compared++
				requireSameJudgment(t, old, now)
			}
			require.Positive(t, expired, "the scenario must reproduce the expired first-window history")
			require.Positive(t, compared, "the scenario must retain events the old maintenance evaluated")
		})
	}
}

// A fragmented picture's marker arrives 1.6s late, after a maintenance tick
// has seen its assembly idle for over a second. It is still inside the
// caller's SRTP replay window, so it completes the picture as a stale return.
// Expiring the unfinished assembly would hide that freshness failure.
func TestDistributedFirstWindowLateMarkerMatchesLegacy(t *testing.T) {
	const late = 140
	scenario := firstWindowScenario{name: "late-marker", total: 9 * time.Second, history: 180 * time.Second,
		events: []firstWindowEvent{{"move", "move", 3 * time.Second, 3030 * time.Millisecond}},
		frameBytes: func(f int) int {
			if f == late {
				return 2400 // three fragments; every other picture is one packet
			}
			return 500
		},
		markerDelay: 1600 * time.Millisecond, srtp: true}
	base := time.Now().Add(-scenario.total - time.Second)
	legacy, oldRecorder := firstWindowCall(t, base, scenario, (*recorder).legacyCompactRecording)
	fixed, newRecorder := firstWindowCall(t, base, scenario, (*recorder).compactRecording)
	for _, r := range []*recorder{oldRecorder, newRecorder} {
		source := r.sentVideoFrames[late]
		require.Greater(t, source.returnedAt, source.at+scenario.markerDelay, "the delayed marker passed SRTP and completed the picture")
	}
	require.Len(t, legacy.Moves, 1)
	require.Len(t, fixed.Moves, 1)
	old, now := legacy.Moves[0], fixed.Moves[0]
	require.False(t, historyExpired(old), old.Measurement.Summary())
	require.True(t, now.Measurement.Video.Verdict.Trusted, now.Measurement.Summary())
	require.True(t, now.Measurement.Video.Verdict.Failed, "the stale return is a known freshness failure")
	require.Equal(t, "fail", now.Distributed.Outcome)
	requireSameJudgment(t, old, now)
}
