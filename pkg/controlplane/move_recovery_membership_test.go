package controlplane

import (
	"context"
	"fmt"
	"net/netip"
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/stretchr/testify/require"
)

func TestMoveRecoveryStaleWorkDoesNotRetainReplacedSource(t *testing.T) {
	p, _, _, _ := setup(t)
	old := replaceAfterStaleRecoveryWork(t, p)
	require.Eventually(t, func() bool {
		runtime.GC()
		return old.Value() == nil
	}, 3*time.Second, time.Millisecond, "stale scheduling tokens retained the replaced registration")
	runtime.KeepAlive(p) // Collection must happen while the plane is still live.
}

// Return before checking collection, so the fixture's stack cannot retain source.
func replaceAfterStaleRecoveryWork(t *testing.T, p *Plane) weak.Pointer[registration] {
	t.Helper()
	ctx := context.Background()
	source := p.workers["a"]
	old := weak.Make(source)
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	base := p.store
	p.store = &retainedMoveStore{Store: base}
	_, err = p.Move(ctx, id, "b")
	require.Error(t, err)
	p.store = base
	// Reconcile the candidate but fail routing, retaining the new owner's token.
	routing := p.relay.(*fakeRelay)
	routing.fail = true
	require.Nil(t, p.recovery.recover(ctx, source, *pendingRecovery(p, source, id), time.Now()))
	routing.fail = false

	// Planned retry and death recovery can dispatch the same token. The second
	// dispatch arrives after the first completes and removes its transfer record.
	p.mu.Lock()
	planned := p.recovery.workLocked(source, recoveryPlanned)
	death := p.recovery.workLocked(source, recoveryAll)
	p.mu.Unlock()
	require.Len(t, planned, 1)
	require.Equal(t, planned, death)
	require.Equal(t, p.workers["b"].addr, death[0].Worker)
	expireObservedWorker(t, source)
	result := p.recovery.recover(ctx, source, planned[0], time.Now())
	require.NotNil(t, result)
	require.False(t, result.Lost)
	require.Nil(t, p.recovery.recover(ctx, source, death[0], time.Now()))
	require.Empty(t, recoveryWork(p, source))
	recovered := p.recoverWorker(ctx, source, time.Now())
	require.True(t, recovered)
	source.observation.recoveryFinished(recovered)
	replacement := &fakeWorker{store: base, addr: netip.MustParseAddrPort("127.0.0.1:3"), running: map[string]bool{}}
	require.NoError(t, p.Replace("a", replacement.addr, replacement))
	require.NotSame(t, source, p.workers["a"])
	return old
}

func TestMoveRecoveryMembershipDoesNotAllocate(t *testing.T) {
	for _, count := range []int{0, 1, 128} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			p, _, _, _ := setup(t)
			ctx := context.Background()
			source := p.workers["a"]
			p.store = &retainedMoveStore{Store: p.store}
			for range count {
				id, _, err := p.Create(ctx, "offer", "a")
				require.NoError(t, err)
				_, err = p.Move(ctx, id, "b")
				require.Error(t, err)
			}
			require.Len(t, recoveryWork(p, source), count)
			for _, scope := range []recoveryScope{recoveryAll, recoveryPlanned} {
				var present bool
				allocs := testing.AllocsPerRun(1000, func() {
					p.mu.Lock()
					present = p.recovery.hasWorkLocked(source, scope)
					p.mu.Unlock()
				})
				require.Equal(t, count > 0, present)
				require.Zero(t, allocs, "membership check must not materialize scheduling tokens")
				t.Logf("retained=%d scope=%d allocations/check=%.0f", count, scope, allocs)
			}
		})
	}
}
