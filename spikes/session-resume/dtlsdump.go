package main

import (
	"bytes"
	"encoding/gob"
	"fmt"
)

// dtlsStateMirror mirrors pion/dtls v3.1.10's unexported serializedState so
// gob can decode it by field name. Used only to report what the blob holds.
type dtlsStateMirror struct {
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

func describeDTLSState(b []byte) string {
	var s dtlsStateMirror
	if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&s); err != nil {
		return "decode error: " + err.Error()
	}
	certBytes := 0
	for _, c := range s.PeerCertificates {
		certBytes += len(c)
	}

	return fmt.Sprintf("gob %dB: epochs local=%d remote=%d, randoms 2x32B, cipherSuite=0x%04x, masterSecret=%dB, "+
		"localSeq(current epoch)=%d, srtpProfile=0x%04x, peerCerts=%d (%dB DER), sessionID=%dB, CIDs=%d/%dB, isClient=%v, alpn=%q",
		len(b), s.LocalEpoch, s.RemoteEpoch, s.CipherSuiteID, len(s.MasterSecret), s.SequenceNumber,
		s.SRTPProtectionProfile, len(s.PeerCertificates), certBytes, len(s.SessionID),
		len(s.LocalConnectionID), len(s.RemoteConnectionID), s.IsClient, s.NegotiatedProtocol)
}
