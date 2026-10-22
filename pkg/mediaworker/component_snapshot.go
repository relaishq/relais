package mediaworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
)

// The private v10 envelope is self-contained. Metadata is an optional expected
// publication token, never a Redis key or a substitute for component bytes.
type componentSnapshot struct {
	Version       int
	Components    sessionstore.Components
	StateVersion  uint32
	Metadata      *sessionstore.ComponentMetadata `json:",omitempty"`
	AgentMoveOnly *movePayload                    `json:",omitempty"`
}
type componentSnapshotState struct {
	history  []byte
	metadata *sessionstore.ComponentMetadata
}

func encodeComponentSnapshot(snap snapshot) ([]byte, error) {
	c, err := splitSessionComponents(snap, snap.split.history, nil)
	if err != nil {
		return nil, err
	}
	return json.Marshal(componentSnapshot{Version: 10, Components: sessionstore.Components(c), StateVersion: snap.State.Agent.State.Version, Metadata: snap.split.metadata, AgentMoveOnly: snap.AgentMoveOnly})
}
func decodeComponentSnapshot(data []byte) (*snapshot, error) {
	var wire componentSnapshot
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("%w: %w", errBadState, err)
	}
	if wire.Version != 10 {
		return nil, errStateVersion
	}
	snap, history, err := assembleSessionComponents(sessionComponents(wire.Components), wire.StateVersion)
	if err != nil {
		return nil, err
	}
	snap.AgentMoveOnly = wire.AgentMoveOnly
	if err := validateSnapshot(&snap); err != nil {
		return nil, err
	}
	snap.split = &componentSnapshotState{history: history, metadata: wire.Metadata}
	return &snap, nil
}

// ComponentSnapshot assembles the already authenticated tuple for the existing
// private restore boundary. The exact publication token travels with it.
func ComponentSnapshot(tuple sessionstore.ComponentTuple) ([]byte, error) {
	snap, history, err := assembleSessionComponents(sessionComponents(tuple.Components), tuple.Metadata.StateVersion)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", sessionstore.ErrUnsafeCheckpoint, err)
	}
	if snap.State.ID != tuple.Lease.SessionID {
		return nil, sessionstore.ErrUnsafeCheckpoint
	}
	metadata := tuple.Metadata
	snap.split = &componentSnapshotState{history: history, metadata: &metadata}
	return json.Marshal(snap)
}

// ReadRecoverySnapshot selects the additive API without trying a legacy read
// for a v10 lease. Unsafe, missing and transient tuple reads never fall back.
// Legacy stores/wrappers retain the original GetState path and its seams.
func ReadRecoverySnapshot(ctx context.Context, store sessionstore.Store, id string) ([]byte, *sessionstore.Checkpoint, error) {
	started := time.Now()
	if components, ok := store.(sessionstore.ComponentStore); ok {
		tuple, err := components.ReadComponents(ctx, id)
		if err == nil {
			data, err := ComponentSnapshot(tuple)
			checkpoint := tuple.Checkpoint
			checkpoint.Age += time.Since(started)
			return data, &checkpoint, err
		}
		if !errors.Is(err, sessionstore.ErrCheckpointLayout) && !errors.Is(err, sessionstore.ErrNotFound) {
			return nil, nil, err
		}
	}
	data, err := store.GetState(ctx, id)
	return data, nil, err
}

// IsComponentSnapshot identifies the explicit v10 envelope before selecting a
// store path. It never accepts the payload; decodeSnapshot performs validation.
func IsComponentSnapshot(data []byte) bool {
	var v struct{ Version int }
	return json.Unmarshal(data, &v) == nil && v.Version == 10
}
