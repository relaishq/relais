package controlplane

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRebindWaitsForRetainedPlannedMove(t *testing.T) {
	p, _, b, _ := setup(t)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	p.store = &retainedMoveStore{Store: p.store}
	_, err = p.Move(ctx, id, "b")
	require.Error(t, err)
	source := p.workers["a"]
	pending := pendingRecovery(p, source, id)
	require.NotNil(t, pending)
	require.NoError(t, p.MarkDraining("a"))
	err = p.readdress(ctx, "a", source, netip.MustParseAddrPort("127.0.0.1:17003"))
	require.Error(t, err, "a retained transfer must settle before its source registration changes")
	p.recovery.recover(ctx, source, *pending, time.Now())
	require.True(t, b.runs(id))
	require.Empty(t, recoveryWork(p, source))
}

func TestRebindWaitsForOperatorMove(t *testing.T) {
	p, _, b, _ := setup(t)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	b.resumeStarted, b.resumeGate = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { _, err := p.Move(ctx, id, "b"); done <- err }()
	<-b.resumeStarted
	defer func() { close(b.resumeGate); require.NoError(t, <-done) }()
	require.NoError(t, p.MarkDraining("a"))
	err = p.readdress(ctx, "a", p.workers["a"], netip.MustParseAddrPort("127.0.0.1:17003"))
	require.Error(t, err, "an exported operator move still holds its source registration")
}
