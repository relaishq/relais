package callharness

import (
	"context"
	"testing"
	"time"

	"github.com/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

func TestStatusHistoryDoesNotTurnDeferredMoveIntoTakeover(t *testing.T) {
	r := newRecorder()
	c := &Call{harness: &Harness{external: &ExternalTopology{}, workers: &workers{}}, rec: r, remoteUfrag: "call"}
	// The operator request already owns the planned-move caller record.
	move := moveRecord{start: r.start.Add(time.Second), end: r.start.Add(1030 * time.Millisecond), from: 0, to: 1}
	c.rec.move(move)
	events := []controlplane.MoveResult{
		{Kind: "move", ID: "call", From: "0", To: "1", Start: move.start, End: r.start.Add(1200 * time.Millisecond)},
		{Kind: "takeover", ID: "call", From: "1", To: "0", LastHeartbeat: r.start.Add(2 * time.Second), DetectedAt: r.start.Add(2400 * time.Millisecond), End: r.start.Add(2500 * time.Millisecond)},
	}
	read := func(context.Context) (controlplane.Status, error) { return controlplane.Status{Takeovers: events}, nil }
	require.NoError(t, c.collectStatusTakeovers(context.Background(), read))
	require.NoError(t, c.collectStatusTakeovers(context.Background(), read))
	require.Len(t, c.rec.moves, 2, "deferred move history must not duplicate or relabel the operator record")
	require.Equal(t, move.start, c.rec.moves[0].start)
	require.Equal(t, move.end, c.rec.moves[0].end)
	require.Equal(t, "takeover", c.rec.moves[1].kind)
	require.Equal(t, 400*time.Millisecond, c.rec.moves[1].detection)
}
