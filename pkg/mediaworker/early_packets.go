package mediaworker

import (
	"slices"

	"github.com/relais/pkg/framecache"
)

const maxEarlyPackets = 64

type earlyPacket struct {
	data []byte
	rtcp bool
}

type mediaPacketResult struct {
	protocol   string
	complete   *framecache.Frame
	failure    uint64
	decryptErr error
}

// handleSRTPPacket preserves arrival order across initial key installation.
// The UDP reader reuses its buffer, so queued ciphertext must be owned here.
func (s *session) handleSRTPPacket(pkt earlyPacket) {
	s.mu.Lock()
	for s.earlyDrain != nil && !s.fenced.Load() {
		done := s.earlyDrain
		s.mu.Unlock()
		select {
		case <-done:
		case <-s.ctx.Done():
			return
		}
		s.mu.Lock()
	}
	if s.fenced.Load() {
		s.mu.Unlock()
		return
	}
	if s.srtpIn == nil {
		pkt.data = slices.Clone(pkt.data)
		if s.earlyPackets == nil {
			s.earlyPackets = make([]earlyPacket, 0, maxEarlyPackets)
		}
		if len(s.earlyPackets) == maxEarlyPackets {
			copy(s.earlyPackets, s.earlyPackets[1:])
			s.earlyPackets[maxEarlyPackets-1] = pkt
			s.worker.metrics.earlyPacketDrops.Add(1)
		} else {
			s.earlyPackets = append(s.earlyPackets, pkt)
		}
		s.mu.Unlock()
		return
	}
	result := s.processSRTPPacketLocked(pkt)
	s.mu.Unlock()
	s.finishMediaPacket(result)
}

func (s *session) processSRTPPacketLocked(pkt earlyPacket) mediaPacketResult {
	if pkt.rtcp {
		failure, err := s.handleRTCPLocked(pkt.data)
		return mediaPacketResult{protocol: "SRTCP", failure: failure, decryptErr: err}
	}
	complete, failure, err := s.handleRTPLocked(pkt.data)
	return mediaPacketResult{protocol: "SRTP", complete: complete, failure: failure, decryptErr: err}
}

// Logging and frame-cache appends retain their ordinary position outside mu.
func (s *session) finishMediaPacket(result mediaPacketResult) {
	s.logDecryptFailure(result.protocol, result.failure, result.decryptErr)
	s.appendFrame(result.complete)
}
