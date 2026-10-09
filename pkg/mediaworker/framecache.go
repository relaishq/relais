package mediaworker

import (
	"context"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/relais/pkg/framecache"
)

// frameCollector accepts only complete VP8 frames with a partition-zero
// start, contiguous source sequence numbers and a final marker. Memory for an
// unfinished frame is bounded independently of the remote cache's policy.
type frameCollector struct {
	building, broken bool
	frame            framecache.Frame
	lastSeq          uint16
	bytes            int
}

func (a *frameCollector) push(in *rtp.Packet, track framecache.Track) *framecache.Frame {
	var desc codecs.VP8Packet
	payload, err := desc.Unmarshal(in.Payload)
	if a.building && in.Timestamp != a.frame.Timestamp {
		a.building = false
	}
	if !a.building {
		a.building = true
		a.broken = err != nil || desc.S != 1 || desc.PID != 0 || len(payload) == 0
		a.frame = framecache.Frame{Track: track, Timestamp: in.Timestamp, SourceSSRC: in.SSRC, Arrival: time.Now(), Keyframe: len(payload) > 0 && payload[0]&1 == 0}
		a.bytes = 0
	} else if err != nil || in.SequenceNumber != a.lastSeq+1 {
		a.broken = true
	}
	a.lastSeq = in.SequenceNumber
	a.bytes += len(in.Payload)
	if a.bytes > 8<<20 || len(a.frame.Packets) >= 8192 {
		a.broken = true
	}
	if !a.broken {
		a.frame.Packets = append(a.frame.Packets, framecache.Packet{SequenceNumber: in.SequenceNumber, Marker: in.Marker, Payload: append([]byte(nil), in.Payload...)})
	}
	if !in.Marker {
		return nil
	}
	a.building = false
	if a.broken {
		a.frame = framecache.Frame{}
		return nil
	}
	f := a.frame
	a.frame = framecache.Frame{}
	return &f
}

// appendFrame runs after releasing mu. cacheMu serializes append with local
// hangup deletion, preventing a delayed write from resurrecting ended media.
// The in-memory append is cheap; remote operations have a bounded context and
// never hold the transport/counter lock.
func (s *session) appendFrame(f *framecache.Frame) {
	if f == nil {
		return
	}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.fenced.Load() || s.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, ownershipTimeout)
	defer cancel()
	if err := s.worker.cfg.FrameCache.Append(ctx, s.id, *f); err != nil && s.ctx.Err() == nil {
		s.log.Warnf("session %s: cache frame: %v", s.id, err)
	}
}

// reserveReplay runs before adoption. Persisting the reservation is mandatory:
// even an immediate second crash must never reuse ciphertext's sequence space.
func (s *session) reserveReplay(frames []framecache.Frame, margin uint16) bool {
	if len(frames) == 0 {
		return s.skipReplay("cache-miss")
	}
	if !frames[0].Keyframe {
		return s.skipReplay("missing-keyframe")
	}
	packets, bytes := 0, 0
	for _, f := range frames {
		if len(f.Packets) == 0 {
			return s.skipReplay("invalid-group")
		}
		packets += len(f.Packets)
		for _, p := range f.Packets {
			bytes += len(p.Payload)
		}
	}
	interval := uint32(3000) // one-frame groups use the 30 fps default
	if len(frames) > 1 {
		step := frames[len(frames)-1].Timestamp - frames[len(frames)-2].Timestamp
		if step > 0 && step <= 90000 {
			interval = step
		}
	}
	duration := time.Duration(uint32(frames[len(frames)-1].Timestamp-frames[0].Timestamp)+interval) * time.Second / 90000
	cfg := s.worker.cfg
	maxBytes, maxDuration := cfg.ReplayMaxBytes, cfg.ReplayMaxDuration
	if maxBytes <= 0 {
		maxBytes = 256 << 10
	}
	maxBytes = min(maxBytes, 256<<10)
	if maxDuration <= 0 {
		maxDuration = time.Second
	}
	// 5500 is the caller-visible staleness allowance used by #7. Limit the
	// burst to 1024 packets even when byte/duration settings are enlarged.
	budget := min(1024, int(margin)-5500-replayWindow-1,
		maxRetainedSequenceAdvance-int(s.state.Video.AdvanceSinceSend)-replayWindow-1)
	// Preserve every remaining #7 retry, including non-default margins.
	remaining := maxRetainedSequenceAdvance - int(s.state.Video.AdvanceSinceSend)
	if margin == 0 {
		return s.skipReplay("sequence-budget")
	}
	budget = min(budget, remaining%int(margin)-replayWindow-1)
	if packets > 1024 {
		return s.skipReplay("packet-cap")
	}
	if packets > budget {
		return s.skipReplay("sequence-budget")
	}
	if bytes > maxBytes {
		return s.skipReplay("byte-cap")
	}
	if duration > maxDuration {
		return s.skipReplay("media-duration-cap")
	}
	// Include the replay's fixed RTP header and the largest supported SRTP tag.
	// Admit at most half the deadline budget, leaving the other half for timer
	// and scheduling overruns. Deadline stops happen only at frame boundaries.
	burstDuration := cfg.ReplayMaxBurstDuration
	if burstDuration <= 0 {
		burstDuration = time.Duration(interval) * time.Second / 90000
	}
	wireBytes := bytes + packets*(12+16)
	if time.Duration(wireBytes)*8*time.Second/time.Duration(cfg.replayBitrate()) > burstDuration/2 {
		return s.skipReplay("burst-duration-budget")
	}
	s.replayBurstDuration = burstDuration
	track := &s.state.Video
	first, last := frames[0], frames[len(frames)-1]
	start := track.HighestSentIndex + 1
	if track.Packets == 0 {
		start = uint64(track.InitialSeq)
	}
	if !track.Anchored {
		track.Anchored = true
		track.InboundSSRC = first.SourceSSRC
		track.SeqOffset = track.InitialSeq - first.Packets[0].SequenceNumber
	}
	age := min(max(time.Since(last.Arrival), 0), 2*time.Second)
	// Never compare an unanchored random clock with zero using serial math.
	// Start at least one source frame interval after the last cached echo.
	ts := last.EchoTimestamp + max(uint32(age.Seconds()*90000), interval)
	if track.Packets > 0 && int32(ts-track.LastTimestamp) <= 0 {
		ts = track.LastTimestamp + interval
	}
	advance := uint32(packets + 1 + replayWindow)
	track.SeqOffset += uint16(advance)
	track.AdvanceSinceSend += advance
	track.HighestSentIndex = start + uint64(packets+replayWindow)
	track.ReplayFloor = start + uint64(packets) - 1
	track.Packets += uint64(packets) // conservative until the reserved burst leaves
	track.LastTimestamp = ts + uint32(len(frames)-1)
	track.TSOffset = track.LastTimestamp + interval - last.Timestamp
	s.replayStart, s.replayTimestamp = start, ts
	s.replay = frames
	s.replayAnchorAt = time.Now()
	return true
}

// encodeReplay runs only before adoption, after durable reservation. No live
// encryption can touch this SRTP context until every burst packet is encoded.
func (s *session) encodeReplay() ([][]byte, error) {
	var packets [][]byte
	seq := s.replayStart
	for i, f := range s.replay {
		for _, p := range f.Packets {
			out := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: s.state.Video.PayloadType, SSRC: s.state.Video.SSRC, SequenceNumber: uint16(seq), Timestamp: s.replayTimestamp + uint32(i), Marker: p.Marker}, Payload: p.Payload}
			raw, err := out.Marshal()
			if err != nil {
				return nil, err
			}
			encrypted, err := s.srtpOut.EncryptRTP(nil, raw, nil)
			if err != nil {
				return nil, err
			}
			packets = append(packets, encrypted)
			seq++
		}
	}
	s.replay = nil
	return packets, nil
}

// sendReplay runs in a worker-tracked goroutine outside the shared UDP reader.
// Only each packet's send holds mu, serializing it with the export fence.
// The deadline is checked only before a frame: finish any started frame,
// rather than turning a decodable prefix into an incomplete keyframe.
func (s *session) sendReplay(packets [][]byte) {
	s.mu.Lock()
	remote := s.state.ICE.RemoteAddr
	maxDuration := s.replayBurstDuration
	s.mu.Unlock()
	if maxDuration <= 0 {
		maxDuration = s.worker.cfg.ReplayMaxBurstDuration
		if maxDuration <= 0 {
			maxDuration = time.Second / 30
		}
	}
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.replaying = false
		s.afterReplay = len(packets) > 0
		if len(packets) > 0 && !s.fenced.Load() && s.ctx.Err() == nil && !s.worker.cfg.DisableResumePLI {
			// A response during replay may have been gated, or the burst interrupted.
			s.needsKeyframe = true
			s.requestKeyframe("resume-cache-complete")
		}
	}()
	rate := s.worker.cfg.replayBitrate()
	next := time.Now()
	deadline := next.Add(maxDuration)
	boundary := true
	for _, packet := range packets {
		wake := next
		if boundary {
			wake = minTime(next, deadline)
		}
		if delay := time.Until(wake); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-s.ctx.Done():
				timer.Stop()
				s.truncateReplay("cancel")
				return
			case <-timer.C:
			}
		}
		if boundary && !time.Now().Before(deadline) {
			s.truncateReplay("deadline")
			return
		}
		// Allow at most 1 ms of pacing credit after a late timer wakeup.
		// This groups tiny packets without releasing an unbounded remainder.
		credit := time.Now().Add(-time.Millisecond)
		if next.Before(credit) {
			next = credit
		}
		next = next.Add(time.Duration(len(packet)) * 8 * time.Second / time.Duration(rate))
		s.mu.Lock()
		if s.fenced.Load() || s.ctx.Err() != nil {
			reason := "cancel"
			if s.fenced.Load() {
				reason = "fence"
			}
			s.mu.Unlock()
			s.truncateReplay(reason)
			return
		}
		_, err := s.worker.send(packet, remote)
		s.mu.Unlock()
		if err != nil {
			s.log.Debugf("session %s: cache replay: %v", s.id, err)
			s.truncateReplay("send-error")
			return
		}
		// SRTP leaves the RTP header, including Marker, in the clear.
		boundary = len(packet) >= 2 && packet[1]&0x80 != 0
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (cfg Config) replayBitrate() int {
	if cfg.ReplayBitrate > 0 {
		return cfg.ReplayBitrate
	}
	return 10_000_000
}

// continueAfterReplay advances the timestamp by elapsed wall time, including
// time spent waiting for the first live frame, then preserves source spacing.
// Sequence space was already reserved; no first-packet offset adjustment is
// allowed to shrink or move the durable reservation.
func (s *session) continueAfterReplay(in *rtp.Packet, header *rtp.Header) {
	track := &s.state.Video
	age := max(time.Since(s.replayAnchorAt), 0)
	elapsed := uint32(uint64(age/time.Second)*90000 + uint64(age%time.Second)*90000/uint64(time.Second))
	track.TSOffset = track.LastTimestamp + max(elapsed, uint32(1)) - in.Timestamp
	header.Timestamp = in.Timestamp + track.TSOffset
	s.afterReplay = false
	s.wantSnapshot()
}

// ReplayStats counts admission skips and interrupted bursts for this worker.
// Reasons are finite strings: cache-miss, cache-timeout, cache-error,
// missing-keyframe, invalid-group, packet-cap, byte-cap, media-duration-cap,
// sequence-budget, burst-duration-budget, reservation-failure, encoding-failure,
// disabled, unconfigured and shared-socket. Truncated reasons are deadline, cancel, fence and
// send-error. Deadline truncation always leaves a complete-frame prefix;
// lifecycle cancellation and transport errors must stop immediately.
type ReplayStats struct {
	Skipped   map[string]uint64
	Truncated map[string]uint64
}

// ReplayStats returns a copied snapshot, independent of session lifetime.
func (w *Worker) ReplayStats() ReplayStats {
	w.replayStatsMu.Lock()
	defer w.replayStatsMu.Unlock()
	out := ReplayStats{Skipped: make(map[string]uint64), Truncated: make(map[string]uint64)}
	for reason, count := range w.replayStats.Skipped {
		out.Skipped[reason] = count
	}
	for reason, count := range w.replayStats.Truncated {
		out.Truncated[reason] = count
	}
	return out
}

func (s *session) recordReplay(reason string, truncated bool) {
	w := s.worker
	w.replayStatsMu.Lock()
	if w.replayStats.Skipped == nil {
		w.replayStats.Skipped = make(map[string]uint64)
		w.replayStats.Truncated = make(map[string]uint64)
	}
	if truncated {
		w.replayStats.Truncated[reason]++
	} else {
		w.replayStats.Skipped[reason]++
	}
	w.replayStatsMu.Unlock()
	event := "skipped"
	if truncated {
		event = "truncated"
	}
	s.log.Infof("session %s: cache replay %s: reason=%s", s.id, event, reason)
}
func (s *session) skipReplay(reason string) bool { s.recordReplay(reason, false); return false }
func (s *session) truncateReplay(reason string)  { s.recordReplay(reason, true) }
