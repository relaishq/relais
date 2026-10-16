package mediaworker

import (
	"context"
	"fmt"
	"maps"

	"github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/relay"
)

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

// SnapshotConsumedInput reads a final planned export's consumed caller input:
// each SSRC's high water mark and, when the exporting worker recorded one,
// its exact 64-index receive window (ADR 0009). The relay replays retained
// input outside this set to the owner that resumes from these exact bytes.
// An export without a window is inexact: only input above High is unconsumed.
func SnapshotConsumedInput(state []byte) (map[uint32]relay.ConsumedInput, error) {
	snap, err := decodeSnapshot(state)
	if err != nil {
		return nil, err
	}
	input := make(map[uint32]relay.ConsumedInput, len(snap.State.SRTP.Inbound))
	for ssrc, high := range snap.State.SRTP.Inbound {
		window, exact := snap.State.SRTP.Received[ssrc]
		input[ssrc] = relay.ConsumedInput{High: high, Window: window, Exact: exact}
	}
	return input, nil
}

// SnapshotReplayIndexes reads the caller floors used for relay replay.
func SnapshotReplayIndexes(state []byte) (map[uint32]uint64, error) {
	snap, err := decodeSnapshot(state)
	if err != nil {
		return nil, err
	}
	return replayInboundIndexes(snap.State)
}

// The agent pair is copied atomically with transport counters in the same
// checkpoint. Custom callbacks may still be queued at that copy; synchronous
// Echo and legacy Echo have no queued work and keep the transport floor.
func replayInboundIndexes(state sessionState) (map[uint32]uint64, error) {
	inbound := make(map[uint32]uint64, len(state.SRTP.Inbound))
	maps.Copy(inbound, state.SRTP.Inbound)
	if state.Agent.legacy || (state.Agent.V2 == nil && isEchoState(state.Agent.State)) || !state.Audio.Anchored {
		return inbound, nil
	}
	ssrc := state.Audio.InboundSSRC
	high, known := inbound[ssrc]
	if !known {
		return inbound, nil
	}
	progress := state.Agent.Progress
	if progress.Consumed == 0 {
		// No input has been consumed, including caller index zero. An absent
		// floor retains all available audio; the ring still reports any trim.
		delete(inbound, ssrc)
		return inbound, nil
	}
	if progress.SSRC != ssrc || progress.Index > high {
		return nil, fmt.Errorf("%w: agent progress does not match the audio checkpoint", errBadState)
	}
	inbound[ssrc] = min(high, progress.Index)
	return inbound, nil
}

func isEchoState(state agent.State) bool {
	return (&agent.Echo{}).Restore(context.Background(), state) == nil
}
