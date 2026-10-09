package mediaworker

import (
	"net/netip"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

// sessionStateVersion is bumped whenever sessionState's layout changes, so a
// stored session state can be checked before a worker resumes it.
const sessionStateVersion = 1

// sessionState is the session state: everything needed to continue a session
// on another media worker except the established DTLS connection state, kept
// as one plain value. Its fields are exported so the value serializes with
// encoding/json or encoding/gob.
//
// The established DTLS connection state (master secret, cipher suite, epochs
// and record sequence numbers) is deliberately not held here. It lives in the
// dtls.Conn, which updates it on every record, and is exported only at
// snapshot time: issue #4 marshals it with the conn's ConnectionState and
// dtls.State.MarshalBinary, and a resuming worker rebuilds the connection
// with dtls.ResumeWithOptions. This value holds everything else.
//
// The live transport objects on a session are caches built from this value
// (plus, for DTLS, that exported connection state):
//
//   - the DTLS connection, established by the handshake today and rebuilt
//     with dtls.ResumeWithOptions in issue #4.
//   - the SRTP contexts, which are re-derived from DTLS keying material and
//     can be primed with the indexes below through SetROC and SetIndex.
type sessionState struct {
	Version int
	ID      string // the session ID, which is also the worker's ICE ufrag

	ICE   iceState
	DTLS  dtlsState
	SRTP  srtpState
	Audio trackState
}

type iceState struct {
	LocalUfrag  string
	LocalPwd    string
	RemoteUfrag string
	RemotePwd   string

	// RemoteAddr is the caller address nominated with USE-CANDIDATE. The
	// worker sends everything there, and only checks from there refresh
	// consent. A USE-CANDIDATE from another address re-nominates. It is the
	// zero value until nomination.
	RemoteAddr netip.AddrPort
}

type dtlsState struct {
	// Certificate (DER) and PrivateKey (PKCS #8 DER) are the worker's DTLS
	// certificate for this session. Its fingerprint is in the answer.
	Certificate []byte
	PrivateKey  []byte

	// CallerFingerprintHash and CallerFingerprint pin the caller's DTLS
	// certificate, from the offer (e.g. "sha-256", "AB:CD:...").
	CallerFingerprintHash string
	CallerFingerprint     string
}

type srtpState struct {
	// Profile is the negotiated SRTP protection profile. It is zero until the
	// DTLS handshake completes.
	Profile srtp.ProtectionProfile

	// Inbound maps each caller SSRC to the highest extended sequence number
	// (rollover counter << 16 | sequence number) the worker has decrypted.
	Inbound map[uint32]uint64
}

// trackState is one of the worker's outbound tracks. The worker echoes one
// of the caller's tracks back as its own continuous RTP stream: its own SSRC,
// and sequence numbers and timestamps that start at the worker's own random
// values.
//
// Rewriting shifts the caller's sequence numbers and timestamps by offsets
// fixed at the first packet, so loss and reordering on the caller's stream
// stay visible to the caller's jitter buffer and NACK logic.
type trackState struct {
	ID          string // msid track ID advertised in the answer
	MID         string
	PayloadType uint8
	SSRC        uint32 // the worker's outbound SSRC

	// Rewriting.
	InitialSeq  uint16 // first outbound sequence number
	InitialTS   uint32 // first outbound timestamp
	Anchored    bool   // the offsets are fixed once the first packet arrives
	InboundSSRC uint32 // the caller SSRC this track echoes
	SeqOffset   uint16
	TSOffset    uint32

	// Counters updated at encrypt time.
	Packets uint64
	// HighestSentIndex is the highest extended sequence number (rollover
	// counter << 16 | sequence number) sent on the track.
	HighestSentIndex uint64
	LastTimestamp    uint32
	// SRTCPIndex is the last SRTCP index sent for this SSRC. The worker
	// sends no RTCP yet, so it stays zero.
	SRTCPIndex uint32
}

// rewrite returns the outbound header for an inbound packet. It reports false
// for packets from a caller SSRC other than the one the track echoes.
func (t *trackState) rewrite(in *rtp.Header) (rtp.Header, bool) {
	if !t.Anchored {
		t.Anchored = true
		t.InboundSSRC = in.SSRC
		t.SeqOffset = t.InitialSeq - in.SequenceNumber
		t.TSOffset = t.InitialTS - in.Timestamp
	} else if in.SSRC != t.InboundSSRC {
		return rtp.Header{}, false
	}

	return rtp.Header{
		Version:        2,
		Marker:         in.Marker,
		PayloadType:    t.PayloadType,
		SequenceNumber: in.SequenceNumber + t.SeqOffset,
		Timestamp:      in.Timestamp + t.TSOffset,
		SSRC:           t.SSRC,
	}, true
}

// noteSent records a packet the track has encrypted.
func (t *trackState) noteSent(header *rtp.Header) {
	if t.Packets == 0 {
		t.HighestSentIndex = uint64(header.SequenceNumber)
	} else if index := extendIndex(t.HighestSentIndex, header.SequenceNumber); index > t.HighestSentIndex {
		t.HighestSentIndex = index
	}
	t.LastTimestamp = header.Timestamp
	t.Packets++
}

// noteInbound records a packet the worker has decrypted.
func (s *srtpState) noteInbound(ssrc uint32, seq uint16) {
	highest, ok := s.Inbound[ssrc]
	if !ok {
		s.Inbound[ssrc] = uint64(seq)

		return
	}
	if index := extendIndex(highest, seq); index > highest {
		s.Inbound[ssrc] = index
	}
}

// extendIndex estimates the extended sequence number (rollover counter << 16
// | sequence number) of seq from the highest one seen so far, as in RFC 3711
// section 3.3.1.
func extendIndex(highest uint64, seq uint16) uint64 {
	roc := highest >> 16
	last := uint16(highest)

	if last < 1<<15 {
		if seq > last && seq-last > 1<<15 && roc > 0 {
			roc--
		}
	} else if last-(1<<15) > seq {
		roc++
	}

	return roc<<16 | uint64(seq)
}
