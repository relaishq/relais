package callharness

import (
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestRecordExternalMovePreservesFailure(t *testing.T) {
	r := newRecorder()
	c := &Call{rec: r}
	c.RecordExternalMove(controlplane.MoveResult{ID: "call", From: "0", To: "1", Start: r.start, End: r.start.Add(time.Second), Lost: true, LossCause: "ownership_expired", Error: "outcome uncertain"})
	require.Len(t, r.moves, 1)
	require.True(t, r.moves[0].lost)
	require.Equal(t, "ownership_expired", r.moves[0].lossCause)
	require.ErrorContains(t, r.moves[0].err, "outcome uncertain")
}
