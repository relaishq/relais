package mediaworker

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"

	"github.com/pion/dtls/v3/pkg/protocol/recordlayer"
)

// DTLS uses few post-handshake records. The guard enforces this reservation,
// independent of timing or retransmission rate; 64 is headroom, not a rate
// assumption. The peer accepts a forward jump in its record replay window.
const dtlsSequenceReservation = uint64(64)

// ErrDTLSSequenceExhausted refuses a crash restore that cannot reserve another
// tail within DTLS's 48-bit sequence space under the same traffic key.
var ErrDTLSSequenceExhausted = errors.New("mediaworker: DTLS sequence reservation exhausted")

// dtlsSerializedState mirrors pion/dtls v3.1.8 state.go's serializedState.
// Keep every field: gob accepts a structural match but silently drops fields
// absent from the destination. The pinned-version and round-trip tests protect
// this adapter when upgrading Pion. The session snapshot format is unchanged.
type dtlsSerializedState struct {
	LocalEpoch            uint16
	RemoteEpoch           uint16
	LocalRandom           [32]byte
	RemoteRandom          [32]byte
	CipherSuiteID         uint16
	MasterSecret          []byte
	SequenceNumber        uint64
	SRTPProtectionProfile uint16
	PeerCertificates      [][]byte
	IdentityHint          []byte
	SessionID             []byte
	LocalConnectionID     []byte
	RemoteConnectionID    []byte
	IsClient              bool
	NegotiatedProtocol    string
}

type dtlsSequenceCheckpoint struct {
	epoch uint16
	next  uint64
}

func decodeDTLSSerializedState(data []byte) (dtlsSerializedState, error) {
	var state dtlsSerializedState
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&state); err != nil {
		return state, fmt.Errorf("%w: DTLS serialization: %v", errBadState, err)
	}
	if state.SequenceNumber > recordlayer.MaxSequenceNumber+1 {
		return state, fmt.Errorf("%w: DTLS sequence exceeds 48 bits", errBadState)
	}
	return state, nil
}

func advanceDTLSSequence(data []byte) ([]byte, error) {
	state, err := decodeDTLSSerializedState(data)
	if err != nil {
		return nil, err
	}
	if state.SequenceNumber > recordlayer.MaxSequenceNumber-dtlsSequenceReservation {
		return nil, ErrDTLSSequenceExhausted
	}
	state.SequenceNumber += dtlsSequenceReservation
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(state); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// allowDTLSDatagram runs under session.mu, at the last point before a caller
// datagram can leave the worker. A coalesced datagram is all-or-nothing: every
// record must fit. No caller bytes are sent when any record cannot be parsed.
func (s *session) allowDTLSDatagram(p []byte) bool {
	bound := s.dtlsAcknowledged
	if bound == nil {
		bound = s.dtlsProvisional
	}
	if bound == nil {
		return true // no checkpoint write has been attempted
	}
	// Invariant: every encrypted record this owner sends has
	// sequence < N_c + R for every checkpoint c that could be latest at takeover.
	// The first capture arms the provisional bound under this same lock;
	// only successful writes advance it. Serialized captures are monotone,
	// so a lost reply can only leave a stricter bound than the committed one.
	for len(p) > 0 {
		const headerBytes = 13 // DTLS 1.2, no connection ID negotiated by this worker
		if len(p) < headerBytes || !isDTLS(p) || p[0] == 25 {
			return s.dropDTLSDatagram()
		}
		size := headerBytes + int(binary.BigEndian.Uint16(p[11:13]))
		if size > len(p) {
			return s.dropDTLSDatagram()
		}
		epoch := binary.BigEndian.Uint16(p[3:5])
		sequence := uint64(binary.BigEndian.Uint16(p[5:7]))<<32 | uint64(binary.BigEndian.Uint32(p[7:11]))
		// Epoch zero is plaintext and has no AEAD nonce to reuse. Pion
		// coalesces its retransmitted ChangeCipherSpec with encrypted Finished;
		// allowing it is necessary to complete a handshake after packet loss.
		// Only the checkpointed encrypted epoch is supported; a new keyed epoch
		// would require an explicitly supported rekey protocol.
		if epoch != 0 && (epoch != bound.epoch || sequence >= bound.next+dtlsSequenceReservation) {
			return s.dropDTLSDatagram()
		}
		p = p[size:]
	}
	return true
}

func (s *session) dropDTLSDatagram() bool {
	s.worker.metrics.dtlsSequenceDrops.Add(1)
	if !s.dtlsCheckpointRequested {
		s.dtlsCheckpointRequested = true
		s.wantSnapshot()
	}
	return false
}
