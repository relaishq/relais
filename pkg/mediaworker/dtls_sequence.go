package mediaworker

import (
	"bytes"
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

// A captured plaintext epoch is an invalid checkpoint, not an ordinary session
// still waiting for its handshake. Keep it distinct so it cannot be suppressed
// by the normal ErrNotEstablished path.
var errDTLSCheckpointEpoch = fmt.Errorf("%w: DTLS checkpoint captured plaintext epoch", errBadState)
