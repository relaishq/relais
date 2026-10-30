package mediaworker

import (
	"slices"

	"github.com/relaishq/relais/internal/relaytrace"
	"github.com/relaishq/relais/pkg/framecache"
)

const maxEarlyPackets = 64

type earlyPacket struct {
	data []byte
	rtcp bool
	path relayPath // zero for already-admitted internal callers
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
	if s.fenced.Load() {
		s.mu.Unlock()
		return
	}
	if s.srtpIn == nil {
		if pkt.path.caller.IsValid() && !s.acceptsRelayPath(pkt.path, !pkt.rtcp) {
			s.mu.Unlock()
			return
		}
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
	if !pkt.rtcp && s.agent != nil && s.agent.paused && s.state.Audio.negotiated() && len(pkt.data) > 1 && pkt.data[1]&0x7f == s.state.Audio.PayloadType {
		// A custom audio agent is flushing for export (ADR 0009). Its audio is
		// not authenticated, so the export never claims it; the relay replays it
		// to the next owner. Video and SRTCP keep their unchanged paths.
		s.mu.Unlock()
		return
	}
	result := s.processSRTPPacketLocked(pkt)
	s.mu.Unlock()
	s.finishMediaPacket(result)
}

func (s *session) processSRTPPacketLocked(pkt earlyPacket) mediaPacketResult {
	if pkt.path.caller.IsValid() {
		allowed := s.acceptsRelayPath(pkt.path, !pkt.rtcp)
		if relaytrace.Enabled() {
			name := "worker-input"
			if !allowed {
				name = "worker-reject"
			}
			relaytrace.Emit(name, pkt.path.leg.String(), pkt.data)
		}
		if !allowed {
			return mediaPacketResult{}
		}
	}
	if pkt.rtcp {
		failure, err := s.handleRTCPLocked(pkt.data)
		return mediaPacketResult{protocol: "SRTCP", failure: failure, decryptErr: err}
	}
	complete, failure, err := s.handleRTPLocked(pkt.data, pkt.path)
	return mediaPacketResult{protocol: "SRTP", complete: complete, failure: failure, decryptErr: err}
}

// Live-packet logging and frame-cache appends run outside mu. The initial drain
// admits frames under mu, before later live packets can append their frames.
func (s *session) finishMediaPacket(result mediaPacketResult) {
	s.logDecryptFailure(result.protocol, result.failure, result.decryptErr)
	s.appendFrame(result.complete)
}
