package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type statusReadKey struct{}

type statusReadStore struct {
	sessionstore.Store
	reads chan chan struct{}
}

func (s *statusReadStore) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	lease, err := s.Store.Get(ctx, id)
	if ctx.Value(statusReadKey{}) != nil {
		release := make(chan struct{})
		select {
		case s.reads <- release:
		case <-ctx.Done():
			return sessionstore.Lease{}, ctx.Err()
		}
		select {
		case <-release:
		case <-ctx.Done():
			return sessionstore.Lease{}, ctx.Err()
		}
	}
	return lease, err
}

// Each poll captures ownership, then waits in store I/O while a real move or
// drain completes. The gate makes the original lock conflict deterministic.
func TestStatusPollingDoesNotBlockMovesAndDrain(t *testing.T) {
	p, a, b, _ := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	s := &statusReadStore{Store: p.store, reads: make(chan chan struct{})}
	p.store = s
	pollCtx, stopPoll := context.WithCancel(context.WithValue(ctx, statusReadKey{}, true))
	done := make(chan struct{})
	polled := make(chan Status)
	pollErrors := make(chan error, 1)
	go func() {
		defer close(done)
		for {
			status, err := p.Status(pollCtx)
			if err != nil {
				if pollCtx.Err() == nil {
					pollErrors <- err
				}
				return
			}
			select {
			case polled <- status:
			case <-pollCtx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() { stopPoll(); <-done })

	const moves = 20
	owner, target := "a", "b"
	for i := 0; i <= moves; i++ {
		var release chan struct{}
		select {
		case release = <-s.reads:
		case err := <-pollErrors:
			t.Fatalf("status poll: %v", err)
		case <-ctx.Done():
			t.Fatal("status poll did not reach store read")
		}
		if i < moves {
			_, err = p.Move(ctx, id, target)
			require.NoError(t, err, "move %d during status read", i)
		} else {
			results, drainErr := p.Drain(ctx, owner)
			require.NoError(t, drainErr, "drain during status read")
			require.Len(t, results, 1)
			require.Empty(t, results[0].Error)
		}
		close(release)
		select {
		case status := <-polled:
			require.Len(t, status.Calls, 1)
			call := status.Calls[0]
			require.Equal(t, owner, call.Owner)
			require.EqualValues(t, i+1, call.Epoch)
			require.EqualValues(t, i, call.MoveCount)
			require.Zero(t, call.TakeoverCount)
			if i == 0 {
				require.Nil(t, call.LastMove)
				require.Empty(t, call.LastMoveKind)
			} else {
				require.NotNil(t, call.LastMove)
				require.Equal(t, "move", call.LastMoveKind)
			}
		case <-ctx.Done():
			t.Fatal("status poll did not finish")
		}
		owner, target = target, owner
	}
	stopPoll()
	<-done
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Calls, 1)
	require.Equal(t, "b", status.Calls[0].Owner)
	require.Equal(t, b.addr, status.Calls[0].Address)
	require.EqualValues(t, moves+2, status.Calls[0].Epoch)
	require.EqualValues(t, moves+1, status.Calls[0].MoveCount)
	require.False(t, a.runs(id))
	require.True(t, b.runs(id))
}

func TestStatusTightPollingDoesNotBlockMoves(t *testing.T) {
	p, _, _, _ := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	pollCtx, stopPoll := context.WithCancel(ctx)
	done := make(chan struct{})
	first := make(chan error, 1)
	pollErrors := make(chan error, 1)
	go func() {
		defer close(done)
		_, err := p.Status(pollCtx)
		first <- err
		for pollCtx.Err() == nil {
			if _, err := p.Status(pollCtx); err != nil {
				if pollCtx.Err() == nil {
					pollErrors <- err
				}
				return
			}
		}
	}()
	t.Cleanup(func() { stopPoll(); <-done })
	require.NoError(t, <-first)
	for i := 0; i < 100; i++ {
		target := "b"
		if i%2 == 1 {
			target = "a"
		}
		_, err := p.Move(ctx, id, target)
		require.NoError(t, err, "move %d during tight status polling", i)
	}
	results, err := p.Drain(ctx, "a")
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Empty(t, results[0].Error)
	stopPoll()
	<-done
	select {
	case err := <-pollErrors:
		t.Fatalf("status poll: %v", err)
	default:
	}
}
