package mediaworker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// frozenV020DecodeSnapshot is the decoder from tag v0.2.0, handover.go,
// with only its name changed and its constants inlined. Original function SHA256:
// a61a0293e3c629efd2902cd46672be2b059441d994525678041551b4f559e42c .
// It uses the shared snapshot value types; its version gate runs before decoding
// any payload or starting ownership/transport work, as in the old worker.
func frozenV020DecodeSnapshot(data []byte) (*snapshot, error) {
	var version struct{ Version int }
	if err := json.Unmarshal(data, &version); err != nil {
		return nil, fmt.Errorf("%w: %w", errBadState, err)
	}
	if version.Version != 9 && version.Version != 8 && version.Version != 7 && version.Version != 6 {
		return nil, fmt.Errorf("%w %d (want %d)", errStateVersion, version.Version, 7)
	}

	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("%w: %w", errBadState, err)
	}
	state := &snap.State
	switch {
	case state.Version != version.Version:
		return nil, fmt.Errorf("%w %d (want %d)", errStateVersion, state.Version, 7)
	case state.ID == "" || state.ID != state.ICE.LocalUfrag:
		return nil, fmt.Errorf("%w: session ID %q, ICE ufrag %q", errBadState, state.ID, state.ICE.LocalUfrag)
	case !state.ICE.RemoteAddr.IsValid():
		return nil, fmt.Errorf("%w: no nominated caller address", errBadState)
	case len(snap.DTLSConnection) == 0 || state.SRTP.Profile == 0:
		return nil, fmt.Errorf("%w: no DTLS connection state", errBadState)
	}
	if version.Version == 6 {
		var legacy struct {
			State struct{ Agent json.RawMessage }
		}
		if err := json.Unmarshal(data, &legacy); err != nil {
			return nil, fmt.Errorf("%w: %w", errBadState, err)
		}
		state.Agent.legacy = len(legacy.State.Agent) == 0
	}
	if version.Version == 8 || version.Version == 9 {
		if state.Agent.V2 == nil || state.Agent.V2.Contract != "agent/v2" {
			return nil, fmt.Errorf("%w: missing v2 identity", errBadState)
		}
	} else {
		if state.Agent.V2 != nil || snap.AgentMoveOnly != nil {
			return nil, fmt.Errorf("%w: v2 data in legacy envelope", errBadState)
		}
		state.Version = 7
		snap.Version = 7
	}
	if snap.AgentMoveOnly != nil && len(snap.AgentMoveOnly.Bytes) > 4<<20 {
		return nil, fmt.Errorf("%w: oversized move payload", errBadState)
	}
	if state.SRTP.Inbound == nil {
		state.SRTP.Inbound = make(map[uint32]uint64)
	}

	return &snap, nil
}

func TestFrozenV020DecoderRejectsV10(t *testing.T) {
	for _, fixture := range []string{"v8-components-source.json", "v9-control.json"} {
		data, err := os.ReadFile("testdata/" + fixture)
		require.NoError(t, err)
		_, err = frozenV020DecodeSnapshot(data)
		require.NoError(t, err, "the frozen decoder still accepts its real legacy snapshots")
	}
	store := sessionstore.NewMemory()
	s := componentWriterSession(t, store, true)
	require.NoError(t, s.persistSnapshotMode(context.Background(), true))
	before, err := store.ReadComponents(context.Background(), s.id)
	require.NoError(t, err)
	data, err := ComponentSnapshot(before)
	require.NoError(t, err)
	_, err = frozenV020DecodeSnapshot(data)
	require.ErrorIs(t, err, errStateVersion)
}
