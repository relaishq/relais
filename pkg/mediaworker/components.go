package mediaworker

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"net/netip"
	"time"

	"github.com/pion/dtls/v3/pkg/protocol/recordlayer"
	"github.com/pion/srtp/v3"
	"github.com/relaishq/relais/pkg/agent"
)

// Slice A is an unwired codec. No live call selects v10, no legacy decoder
// accepts it, and no checkpoint/store path calls these functions yet.
const (
	componentSetup      byte = 1
	componentTransport  byte = 2
	componentWork       byte = 3
	componentHistory    byte = 4
	componentRoot       byte = 5
	componentCheckpoint byte = 6
)

// Only the three mutable serialized DTLS fields live in transport. This wire
// adapter is pinned to pion/dtls v3.1.8, like dtlsSerializedState. Epoch/sequence
// ownership must never be copied into the setup component.
type componentDTLSSetup struct {
	LocalRandom, RemoteRandom                                      [32]byte
	CipherSuiteID                                                  uint16
	MasterSecret                                                   []byte
	SRTPProtectionProfile                                          uint16
	PeerCertificates                                               [][]byte
	IdentityHint, SessionID, LocalConnectionID, RemoteConnectionID []byte
	IsClient                                                       bool
	NegotiatedProtocol                                             string
}
type componentDTLSProgress struct {
	LocalEpoch, RemoteEpoch uint16
	SequenceNumber          uint64
}

type connectionSetupComponent struct {
	ID                                           string
	LocalUfrag, LocalPwd, RemoteUfrag, RemotePwd string
	Certificate, PrivateKey                      []byte
	CallerFingerprintHash, CallerFingerprint     string
	DTLS                                         componentDTLSSetup
}

// Registration identity, tasks/cursors/timers/Control, move-only lengths,
// playback metadata and quota use the canonical v9 host JSON grammar, under
// its existing 2 KiB cap. App bytes and references are never in this payload.
// Audio/Video include negotiation IDs as well as all changing rewrite fields.
type transportComponent struct {
	LegacyVersion     int
	RemoteAddr        string
	DTLS              componentDTLSProgress
	Profile           uint16
	Inbound, Received map[uint32]uint64
	Audio, Video      componentTrack
	Checkpoint        componentCheckpointState
	Progress          agent.Progress
	AgentRevision     uint64
	Host              []byte
}

type sessionComponents struct {
	Setup, Transport, Work, History []byte
}

func splitDTLSComponents(data []byte) (componentDTLSSetup, componentDTLSProgress, error) {
	s, err := decodeDTLSSerializedState(data)
	if err != nil {
		return componentDTLSSetup{}, componentDTLSProgress{}, err
	}
	return componentDTLSSetup{
		LocalRandom: s.LocalRandom, RemoteRandom: s.RemoteRandom, CipherSuiteID: s.CipherSuiteID,
		MasterSecret: s.MasterSecret, SRTPProtectionProfile: s.SRTPProtectionProfile,
		PeerCertificates: s.PeerCertificates, IdentityHint: s.IdentityHint, SessionID: s.SessionID,
		LocalConnectionID: s.LocalConnectionID, RemoteConnectionID: s.RemoteConnectionID,
		IsClient: s.IsClient, NegotiatedProtocol: s.NegotiatedProtocol,
	}, componentDTLSProgress{s.LocalEpoch, s.RemoteEpoch, s.SequenceNumber}, nil
}

func joinDTLSComponents(s componentDTLSSetup, p componentDTLSProgress) ([]byte, error) {
	if p.LocalEpoch == 0 || p.SequenceNumber > recordlayer.MaxSequenceNumber+1 {
		return nil, fmt.Errorf("%w: invalid checkpoint DTLS progress", errBadState)
	}
	state := dtlsSerializedState{
		LocalEpoch: p.LocalEpoch, RemoteEpoch: p.RemoteEpoch, SequenceNumber: p.SequenceNumber,
		LocalRandom: s.LocalRandom, RemoteRandom: s.RemoteRandom, CipherSuiteID: s.CipherSuiteID,
		MasterSecret: s.MasterSecret, SRTPProtectionProfile: s.SRTPProtectionProfile,
		PeerCertificates: s.PeerCertificates, IdentityHint: s.IdentityHint, SessionID: s.SessionID,
		LocalConnectionID: s.LocalConnectionID, RemoteConnectionID: s.RemoteConnectionID,
		IsClient: s.IsClient, NegotiatedProtocol: s.NegotiatedProtocol,
	}
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(state); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// expectedSetup, when supplied, is the committed canonical setup content.
// Unexpected negotiation/key changes refuse the split; key rotation changes
// only encryption, never this content. history is owned app data, not parsed.
func splitSessionComponents(snap snapshot, history, expectedSetup []byte) (sessionComponents, error) {
	var c sessionComponents
	setup, progress, err := splitDTLSComponents(snap.DTLSConnection)
	if err != nil {
		return c, err
	}
	if progress.LocalEpoch == 0 {
		return c, fmt.Errorf("%w: DTLS checkpoint epoch zero", errBadState)
	}
	s := snap.State
	c.Setup, err = marshalComponent(componentSetup, connectionSetupComponent{
		ID: s.ID, LocalUfrag: s.ICE.LocalUfrag, LocalPwd: s.ICE.LocalPwd, RemoteUfrag: s.ICE.RemoteUfrag, RemotePwd: s.ICE.RemotePwd,
		Certificate: s.DTLS.Certificate, PrivateKey: s.DTLS.PrivateKey, CallerFingerprintHash: s.DTLS.CallerFingerprintHash, CallerFingerprint: s.DTLS.CallerFingerprint, DTLS: setup,
	})
	if err != nil {
		return c, err
	}
	if expectedSetup != nil && !bytes.Equal(expectedSetup, c.Setup) {
		return c, fmt.Errorf("%w: changed connection setup", errBadState)
	}
	var host []byte
	if s.Agent.V2 != nil {
		host, err = json.Marshal(s.Agent.V2)
		if err != nil {
			return c, err
		}
		if len(host) > v2HostMetadataBytes {
			return c, fmt.Errorf("%w: v10 host metadata exceeds 2048 bytes", errBadState)
		}
	}
	c.Transport, err = marshalComponent(componentTransport, transportComponent{
		LegacyVersion: s.Version, RemoteAddr: s.ICE.RemoteAddr.String(), DTLS: progress, Profile: uint16(s.SRTP.Profile), Inbound: s.SRTP.Inbound, Received: s.SRTP.Received,
		Audio: componentTrack(s.Audio), Video: componentTrack(s.Video), Checkpoint: componentCheckpointState(s.Checkpoint), Progress: s.Agent.Progress, AgentRevision: s.Agent.Revision, Host: host,
	})
	if err != nil {
		return c, err
	}
	c.Work, err = marshalComponent(componentWork, s.Agent.State.Bytes)
	if err != nil {
		return c, err
	}
	c.History, err = marshalComponent(componentHistory, history)
	return c, err
}

// The result is an internal restore input, not a v10 wire/private API writer.
// Callers must feed it to session.resume with the original ResumeOptions. That
// single existing path performs advanceDTLSSequence on crash restore and keeps
// confirmed planned moves exact; the adapter contains no second sequence rule.
func assembleSessionComponents(c sessionComponents, stateVersion uint32) (snapshot, []byte, error) {
	var setup connectionSetupComponent
	var transport transportComponent
	var work, history []byte
	for _, item := range []struct {
		data  []byte
		kind  byte
		value any
	}{{c.Setup, componentSetup, &setup}, {c.Transport, componentTransport, &transport}, {c.Work, componentWork, &work}, {c.History, componentHistory, &history}} {
		if err := unmarshalComponent(item.data, item.kind, item.value); err != nil {
			return snapshot{}, nil, err
		}
	}
	var host *v2Envelope
	if len(transport.Host) > 0 {
		if len(transport.Host) > v2HostMetadataBytes {
			return snapshot{}, nil, fmt.Errorf("%w: host metadata size", errBadState)
		}
		if err := json.Unmarshal(transport.Host, &host); err != nil {
			return snapshot{}, nil, err
		}
		canonical, err := json.Marshal(host)
		if err != nil || !bytes.Equal(canonical, transport.Host) {
			return snapshot{}, nil, fmt.Errorf("%w: noncanonical host metadata", errBadState)
		}
	}
	addr, err := netip.ParseAddrPort(transport.RemoteAddr)
	if err != nil {
		return snapshot{}, nil, err
	}
	dtlsBytes, err := joinDTLSComponents(setup.DTLS, transport.DTLS)
	if err != nil {
		return snapshot{}, nil, err
	}
	state := sessionState{Version: transport.LegacyVersion, ID: setup.ID,
		ICE:   iceState{LocalUfrag: setup.LocalUfrag, LocalPwd: setup.LocalPwd, RemoteUfrag: setup.RemoteUfrag, RemotePwd: setup.RemotePwd, RemoteAddr: addr},
		DTLS:  dtlsState{Certificate: setup.Certificate, PrivateKey: setup.PrivateKey, CallerFingerprintHash: setup.CallerFingerprintHash, CallerFingerprint: setup.CallerFingerprint},
		Audio: trackState(transport.Audio), Video: trackState(transport.Video), Checkpoint: CheckpointState(transport.Checkpoint),
		Agent: agentState{V2: host, State: agent.State{Version: stateVersion, Bytes: work}, Progress: transport.Progress, Revision: transport.AgentRevision},
	}
	state.SRTP.Profile = srtp.ProtectionProfile(transport.Profile)
	state.SRTP.Inbound, state.SRTP.Received = transport.Inbound, transport.Received
	return snapshot{Version: state.Version, State: state, DTLSConnection: dtlsBytes}, history, nil
}

// Frozen v10 wire layout. Do not evolve with the legacy state type.
type componentTrack struct {
	ID          string // msid track ID advertised in the answer
	MID         string // the answered m-line; empty when the track is not negotiated
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
	// AdvanceSinceSend accumulates takeover margins until this track sends.
	// It survives snapshots, including failed adoption and silent cameras.
	AdvanceSinceSend uint32
	Packets          uint64
	// HighestSentIndex is the highest extended sequence number (rollover
	// counter << 16 | sequence number) sent or durably reserved on the track.
	HighestSentIndex uint64
	// ReplayFloor protects cached indexes and advances to the snapshot high
	// water mark on later resumes. Never echo a
	// rewritten source index at or below it, including after planned resumes.
	// After a margin-zero move of an ever-replayed session, reordered late packets
	// never echoed by the old owner are dropped: a small quality cost, not a safety issue.
	ReplayFloor   uint64
	LastTimestamp uint32
	// SRTCPIndex is the last SRTCP index the worker used for RTCP it sent
	// with this track's SSRC as the sender: the keyframe requests (PLI) it
	// sends the caller on the video track. It stays zero until the first.
	SRTCPIndex uint32
}

// Frozen v10 wire layout. Do not evolve with the legacy state type.
type componentCheckpointState struct {
	CapturedAt          time.Time     `json:"captured_at"`
	AgeAtCapture        time.Duration `json:"age_at_capture"`
	Attempts            uint64        `json:"attempts"`
	Successes           uint64        `json:"successes"`
	Failures            uint64        `json:"failures"`
	RTPPacketRate       float64       `json:"rtp_packet_rate"`
	SRTCPPacketRate     float64       `json:"srtcp_packet_rate"`
	TakeoverAge         time.Duration `json:"takeover_age"`
	TakeoverSnapshotAge time.Duration `json:"takeover_snapshot_age"`
	TakeoverStoredAt    time.Time     `json:"takeover_stored_at"`
}
