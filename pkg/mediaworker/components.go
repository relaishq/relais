package mediaworker

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"net/netip"

	"github.com/pion/dtls/v3/pkg/protocol/recordlayer"
	"github.com/pion/srtp/v3"
	"github.com/relaishq/relais/internal/componentcodec"
	"github.com/relaishq/relais/pkg/agent"
)

// V10 components use the shared codec. New calls select this format only
// through SplitCheckpoints; legacy v8/v9 encoding remains independent.
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
type componentDTLSSetup = componentcodec.DTLSSetup
type componentDTLSProgress = componentcodec.DTLSProgress
type connectionSetupComponent = componentcodec.ConnectionSetup
type transportComponent = componentcodec.TransportState
type componentTrack = componentcodec.Track
type componentCheckpointState = componentcodec.CheckpointState
type componentProgress = componentcodec.Progress

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
	}, componentDTLSProgress{LocalEpoch: s.LocalEpoch, RemoteEpoch: s.RemoteEpoch, SequenceNumber: s.SequenceNumber}, nil
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
	// UDP nomination has one IPv4 representation in v10, including captures
	// supplied as IPv4-mapped IPv6. Legacy snapshot decoding is unchanged.
	address := s.ICE.RemoteAddr
	if address.Addr().Is4In6() {
		address = netip.AddrPortFrom(address.Addr().Unmap(), address.Port())
	}
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
		LegacyVersion: s.Version, RemoteAddr: address.String(), DTLS: progress, Profile: uint16(s.SRTP.Profile), Inbound: s.SRTP.Inbound, Received: s.SRTP.Received,
		Audio: componentTrack(s.Audio), Video: componentTrack(s.Video), Checkpoint: componentCheckpointState(s.Checkpoint), Progress: componentProgress(s.Agent.Progress), AgentRevision: s.Agent.Revision, Host: host,
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

// The result is the internal restore input for the self-contained v10 envelope.
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
			return snapshot{}, nil, fmt.Errorf("%w: host metadata JSON: %w", errBadState, err)
		}
		canonical, err := json.Marshal(host)
		if err != nil || !bytes.Equal(canonical, transport.Host) {
			return snapshot{}, nil, fmt.Errorf("%w: noncanonical host metadata", errBadState)
		}
	}
	addr, err := netip.ParseAddrPort(transport.RemoteAddr)
	if err != nil {
		return snapshot{}, nil, fmt.Errorf("%w: nominated address: %w", errBadState, err)
	}
	dtlsBytes, err := joinDTLSComponents(setup.DTLS, transport.DTLS)
	if err != nil {
		return snapshot{}, nil, err
	}
	state := sessionState{Version: transport.LegacyVersion, ID: setup.ID,
		ICE:   iceState{LocalUfrag: setup.LocalUfrag, LocalPwd: setup.LocalPwd, RemoteUfrag: setup.RemoteUfrag, RemotePwd: setup.RemotePwd, RemoteAddr: addr},
		DTLS:  dtlsState{Certificate: setup.Certificate, PrivateKey: setup.PrivateKey, CallerFingerprintHash: setup.CallerFingerprintHash, CallerFingerprint: setup.CallerFingerprint},
		Audio: trackState(transport.Audio), Video: trackState(transport.Video), Checkpoint: CheckpointState(transport.Checkpoint),
		Agent: agentState{V2: host, State: agent.State{Version: stateVersion, Bytes: work}, Progress: agent.Progress(transport.Progress), Revision: transport.AgentRevision},
	}
	state.SRTP.Profile = srtp.ProtectionProfile(transport.Profile)
	state.SRTP.Inbound, state.SRTP.Received = transport.Inbound, transport.Received
	snap := snapshot{Version: state.Version, State: state, DTLSConnection: dtlsBytes}
	if transport.LegacyVersion < 7 || transport.LegacyVersion > 9 {
		return snapshot{}, nil, fmt.Errorf("%w: %w: v10 host version", errBadState, errStateVersion)
	}
	if err := validateSnapshot(&snap); err != nil {
		return snapshot{}, nil, fmt.Errorf("%w: %w", errBadState, err)
	}
	// A binary encoding can be canonical while its decoded host/address has a
	// different representation. Reject aliases rather than silently normalize.
	canonical, err := splitSessionComponents(snap, history, nil)
	if err != nil {
		return snapshot{}, nil, fmt.Errorf("%w: component re-encoding: %w", errBadState, err)
	}
	if !bytes.Equal(c.Setup, canonical.Setup) || !bytes.Equal(c.Transport, canonical.Transport) || !bytes.Equal(c.Work, canonical.Work) || !bytes.Equal(c.History, canonical.History) {
		return snapshot{}, nil, fmt.Errorf("%w: noncanonical assembled components", errBadState)
	}
	return snap, history, nil
}
