package mediaworker

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"time"

	"github.com/pion/rtp"
)

// PacketTrace identifies one successfully sent ciphertext. It contains no keys
// or payload. The fingerprint matches the caller's SRTPFailureSample, even when
// the caller estimates a different ROC. Epoch and Origin identify its tenure
// and whether it came from live output, a cached group, or RTCP feedback.
type PacketTrace struct {
	SentAt    time.Time
	Worker    netip.AddrPort
	SessionID string
	Epoch     uint64
	Origin    string
	Protocol  string
	SSRC      uint32
	Sequence  uint16 `json:",omitempty"`
	Timestamp uint32 `json:",omitempty"`
	// Index is reconstructed from track state for SRTP, explicit on wire for
	// SRTCP. ContextROC is Pion's highest ROC, not a reordered packet's ROC.
	Index            uint64
	ContextROC       *uint32 `json:",omitempty"`
	CiphertextSHA256 string
	Bytes            int
}

// tracePacket runs under the session lock. All work is behind the opt-in hook.
func (s *session) tracePacket(raw []byte, origin string) {
	if s.worker.cfg.PacketTrace == nil || len(raw) < 8 {
		return
	}
	packet := PacketTrace{SentAt: time.Now(), Worker: s.worker.localAddr, SessionID: s.id, Epoch: s.lease.Epoch, Origin: origin, Bytes: len(raw)}
	if isRTCP(raw) {
		packet.Protocol = "SRTCP"
		packet.SSRC = binary.BigEndian.Uint32(raw[4:8])
		tag, _ := s.state.SRTP.Profile.AuthTagRTCPLen()
		if len(raw) < 8+4+tag {
			return
		}
		packet.Index = uint64(binary.BigEndian.Uint32(raw[len(raw)-tag-4:]) & 0x7fffffff)
	} else {
		var header rtp.Header
		if _, err := header.Unmarshal(raw); err != nil {
			return
		}
		packet.Protocol = "SRTP"
		packet.SSRC, packet.Sequence, packet.Timestamp = header.SSRC, header.SequenceNumber, header.Timestamp
		if roc, ok := s.srtpOut.ROC(header.SSRC); ok {
			packet.ContextROC = &roc
		}
		for _, track := range []*trackState{&s.state.Audio, &s.state.Video} {
			if track.SSRC == packet.SSRC {
				packet.Index = extendIndex(track.HighestSentIndex, header.SequenceNumber)
			}
		}
	}
	sum := sha256.Sum256(raw)
	packet.CiphertextSHA256 = hex.EncodeToString(sum[:])
	s.worker.cfg.PacketTrace(packet)
}
