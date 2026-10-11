package mediaworker

import "maps"

// SnapshotInboundIndexes reads the exact checkpoint's CALLER inbound SRTP
// high water marks. The worker's outbound SSRC/sequence rewrite and #9's
// ReplayFloor are separate spaces and are never returned to the relay.
func SnapshotInboundIndexes(state []byte) (map[uint32]uint64, error) {
	snap, err := decodeSnapshot(state)
	if err != nil {
		return nil, err
	}
	// Nil is the relay protocol's not-yet-prepared marker. A checkpoint with
	// no authenticated media still supplies a valid, empty inbound map.
	inbound := make(map[uint32]uint64, len(snap.State.SRTP.Inbound))
	maps.Copy(inbound, snap.State.SRTP.Inbound)
	return inbound, nil
}
