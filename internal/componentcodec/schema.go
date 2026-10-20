package componentcodec

import "time"

type DTLSSetup struct {
	LocalRandom, RemoteRandom                                      [32]byte
	CipherSuiteID                                                  uint16
	MasterSecret                                                   []byte
	SRTPProtectionProfile                                          uint16
	PeerCertificates                                               [][]byte
	IdentityHint, SessionID, LocalConnectionID, RemoteConnectionID []byte
	IsClient                                                       bool
	NegotiatedProtocol                                             string
}
type DTLSProgress struct {
	LocalEpoch, RemoteEpoch uint16
	SequenceNumber          uint64
}

type ConnectionSetup struct {
	ID                                           string
	LocalUfrag, LocalPwd, RemoteUfrag, RemotePwd string
	Certificate, PrivateKey                      []byte
	CallerFingerprintHash, CallerFingerprint     string
	DTLS                                         DTLSSetup
}

// Registration identity, tasks/cursors/timers/Control, move-only lengths,
// playback metadata and quota use the canonical v9 host JSON grammar, under
// its existing 2 KiB cap. App bytes and references are never in this payload.
// Audio/Video include negotiation IDs as well as all changing rewrite fields.
type TransportState struct {
	LegacyVersion     int
	RemoteAddr        string
	DTLS              DTLSProgress
	Profile           uint16
	Inbound, Received map[uint32]uint64
	Audio, Video      Track
	Checkpoint        CheckpointState
	Progress          Progress
	AgentRevision     uint64
	Host              []byte
}

// Frozen v10 wire layout. Do not evolve with the legacy state type.
type Track struct {
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
type CheckpointState struct {
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

// Frozen v10 consumed-input progress. Public agent.Progress may evolve without
// changing this wire layout. Convert explicitly at the capture/restore edges.
type Progress struct {
	SSRC      uint32
	Index     uint64
	Timestamp uint32
	Consumed  uint64
}
