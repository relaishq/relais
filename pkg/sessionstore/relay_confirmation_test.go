package sessionstore

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRelayConfirmationLifecycle(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		s := factory(t)
		store, ok := s.(RelayConfirmations)
		require.True(t, ok)
		ctx := context.Background()
		lease, err := s.Claim(ctx, "relay-confirmation", netip.MustParseAddrPort("127.0.0.1:17001"), time.Minute)
		require.NoError(t, err)
		current := RelayAssignment{Slot: "r1"}
		require.NoError(t, store.CASRelay(ctx, lease, RelayAssignment{}, current))

		a, got, err := store.RelayConfirmation(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, current, a)
		require.Equal(t, RelayConfirmation{}, got, "a call created before the contract has no stored mode")

		mode := RelayConfirmation{Mode: RelayConfirmDecode}
		require.NoError(t, store.CASRelayConfirmation(ctx, lease.SessionID, current, RelayConfirmation{}, mode, time.Time{}))
		// The mode is fixed for the whole call.
		require.ErrorIs(t, store.CASRelayConfirmation(ctx, lease.SessionID, current, mode, RelayConfirmation{Mode: RelayConfirmDelivery}, time.Time{}), ErrRelayTransition)

		pending := RelayAssignment{Slot: "r1", Pending: "r2"}
		require.NoError(t, store.CASRelay(ctx, lease, current, pending))
		attempt := RelayAttempt{Generation: 1, Slot: "r2", Phase: RelayPhasePending, Required: []string{"0", "1"}, Kind: "planned", Tried: []string{"r2"}}
		withAttempt := RelayConfirmation{Mode: RelayConfirmDecode, Attempt: &attempt}
		// A write that names a stale assignment is refused.
		require.ErrorIs(t, store.CASRelayConfirmation(ctx, lease.SessionID, current, mode, withAttempt, time.Time{}), ErrRelayConflict)
		require.NoError(t, store.CASRelayConfirmation(ctx, lease.SessionID, pending, mode, withAttempt, time.Time{}))

		// A full A set saved before the commit is consumed by the commit.
		decoded := attempt
		decoded.Decoded = []string{"0", "1"}
		full := RelayConfirmation{Mode: RelayConfirmDecode, Attempt: &decoded}
		require.NoError(t, store.CASRelayConfirmation(ctx, lease.SessionID, pending, withAttempt, full, time.Time{}))
		a, got, err = store.RelayConfirmation(ctx, lease.SessionID)
		require.NoError(t, err)
		require.False(t, got.Confirmed(a), "A without the switch commit never confirms")
		committed := RelayAssignment{Slot: "r2", Generation: 1}
		require.NoError(t, store.CommitRelay(ctx, lease, pending, committed))
		a, got, err = store.RelayConfirmation(ctx, lease.SessionID)
		require.NoError(t, err)
		require.True(t, got.Confirmed(a))

		// Expiry compares the same assignment and record: a writer that read
		// before the commit cannot expire the confirmed attempt.
		expired := decoded
		expired.Phase = RelayPhaseExpired
		require.ErrorIs(t, store.CASRelayConfirmation(ctx, lease.SessionID, pending, full, RelayConfirmation{Mode: RelayConfirmDecode, Attempt: &expired}, time.Time{}), ErrRelayConflict)

		// A terminal phase is final for its generation; a lower generation is refused.
		confirmed := decoded
		confirmed.Phase = RelayPhaseConfirmed
		done := RelayConfirmation{Mode: RelayConfirmDecode, Attempt: &confirmed}
		require.NoError(t, store.CASRelayConfirmation(ctx, lease.SessionID, committed, got, done, time.Time{}))
		require.ErrorIs(t, store.CASRelayConfirmation(ctx, lease.SessionID, committed, done, RelayConfirmation{Mode: RelayConfirmDecode, Attempt: &expired}, time.Time{}), ErrRelayTransition)
		older := confirmed
		older.Generation = 0
		require.ErrorIs(t, store.CASRelayConfirmation(ctx, lease.SessionID, committed, done, RelayConfirmation{Mode: RelayConfirmDecode, Attempt: &older}, time.Time{}), ErrRelayTransition)

		// The record survives a worker takeover of the same call.
		next, err := s.Transfer(ctx, lease, netip.MustParseAddrPort("127.0.0.1:17002"), time.Minute)
		require.NoError(t, err)
		_, got, err = store.RelayConfirmation(ctx, next.SessionID)
		require.NoError(t, err)
		require.Equal(t, RelayPhaseConfirmed, got.Attempt.Phase)

		require.NoError(t, s.Release(ctx, next))
		_, _, err = store.RelayConfirmation(ctx, next.SessionID)
		require.ErrorIs(t, err, ErrNotFound)
		require.ErrorIs(t, store.CASRelayConfirmation(ctx, next.SessionID, committed, done, done, time.Time{}), ErrNotFound)
	})
}

func TestRelayConfirmationBounds(t *testing.T) {
	ok := RelayAttempt{Generation: 1, Slot: "r2", Phase: RelayPhasePending, Required: []string{"0"}}
	_, _, err := encodeRelayConfirmation(RelayConfirmation{Mode: RelayConfirmDelivery, Attempt: &ok})
	require.NoError(t, err)
	for name, c := range map[string]RelayConfirmation{
		"mode":         {Mode: "commit"},
		"attempt-none": {Mode: RelayConfirmNone, Attempt: &ok},
		"phase":        {Mode: RelayConfirmDecode, Attempt: &RelayAttempt{Generation: 1, Slot: "r2", Phase: "done"}},
		"mid":          {Mode: RelayConfirmDecode, Attempt: &RelayAttempt{Generation: 1, Slot: "r2", Phase: RelayPhasePending, Required: []string{strings.Repeat("m", 33)}}},
		"mids":         {Mode: RelayConfirmDecode, Attempt: &RelayAttempt{Generation: 1, Slot: "r2", Phase: RelayPhasePending, Decoded: make([]string, 17)}},
	} {
		_, _, err := encodeRelayConfirmation(c)
		require.ErrorIs(t, err, ErrRelayConfirmationInvalid, name)
	}
	require.False(t, RelayConfirmation{Mode: RelayConfirmDecode, Attempt: &RelayAttempt{Generation: 1, Slot: "r2", Phase: RelayPhaseSwitched}}.Confirmed(RelayAssignment{Slot: "r2", Generation: 1}), "an empty required set never confirms")
	delivered := RelayAttempt{Generation: 1, Slot: "r2", Phase: RelayPhaseSwitched, Required: []string{"0", "1"}, Delivered: []string{"0", "1"}}
	require.False(t, RelayConfirmation{Mode: RelayConfirmDecode, Attempt: &delivered}.Confirmed(RelayAssignment{Slot: "r2", Generation: 1}), "C alone never confirms A mode")
	require.False(t, RelayConfirmation{Mode: RelayConfirmDelivery, Attempt: &delivered}.Confirmed(RelayAssignment{Slot: "r2", Generation: 1}), "no recorded switch commit")
	require.True(t, RelayConfirmation{Mode: RelayConfirmDelivery, Attempt: &delivered, Commit: RelayCommit{Switched: 1}}.Confirmed(RelayAssignment{Slot: "r2", Generation: 1}))
	require.False(t, RelayConfirmation{Mode: RelayConfirmDelivery, Attempt: &delivered}.Confirmed(RelayAssignment{Slot: "r1", Generation: 1}), "evidence for another slot never confirms")
}

// Findings 4 and 5 (#278 slice 4 review): a retirement is never a switch,
// even on the same slot; a commit after the outer deadline never confirms;
// and the store refuses evidence written after its deadline.
func TestRelayConfirmationCommitRules(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		s := factory(t)
		store := s.(RelayConfirmations)
		ctx := context.Background()
		lease, err := s.Claim(ctx, "relay-commit-rules", netip.MustParseAddrPort("127.0.0.1:17001"), time.Minute)
		require.NoError(t, err)
		current := RelayAssignment{Slot: "r1"}
		require.NoError(t, store.CASRelay(ctx, lease, RelayAssignment{}, current))
		mode := RelayConfirmation{Mode: RelayConfirmDecode}
		require.NoError(t, store.CASRelayConfirmation(ctx, lease.SessionID, current, RelayConfirmation{}, mode, time.Time{}))

		// Same-slot attempt with full A, then retired (abort) instead of switched.
		pending := RelayAssignment{Slot: "r1", Pending: "r1"}
		require.NoError(t, store.CASRelay(ctx, lease, current, pending))
		attempt := RelayAttempt{Generation: 1, Slot: "r1", Phase: RelayPhasePending, Required: []string{"0"}, Decoded: []string{"0"}, Kind: "recovery", EpisodeDeadline: time.Now().Add(time.Minute)}
		_, rec, err := store.RelayConfirmation(ctx, lease.SessionID)
		require.NoError(t, err)
		require.NoError(t, store.CASRelayConfirmation(ctx, lease.SessionID, pending, rec, RelayConfirmation{Mode: RelayConfirmDecode, Attempt: &attempt}, time.Time{}))
		retired := RelayAssignment{Slot: "r1", Generation: 1}
		require.NoError(t, store.CASRelay(ctx, lease, pending, retired))
		a, rec, err := store.RelayConfirmation(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, retired, a)
		require.True(t, rec.Aborted(), "the retired generation is aborted atomically")
		require.False(t, rec.Confirmed(a), "a same-slot retirement must not look like a switch commit")

		// A real switch commit after the outer deadline never confirms.
		pending = RelayAssignment{Slot: "r1", Generation: 1, Pending: "r2"}
		require.NoError(t, store.CASRelay(ctx, lease, retired, pending))
		late := RelayAttempt{Generation: 2, Slot: "r2", Phase: RelayPhasePending, Required: []string{"0"}, Decoded: []string{"0"}, Kind: "planned", PlannedDeadline: time.Now().Add(20 * time.Millisecond), StoreDeadline: time.Now().Add(20 * time.Millisecond)}
		_, rec, err = store.RelayConfirmation(ctx, lease.SessionID)
		require.NoError(t, err)
		require.NoError(t, store.CASRelayConfirmation(ctx, lease.SessionID, pending, rec, RelayConfirmation{Mode: RelayConfirmDecode, Attempt: &late}, time.Time{}))
		time.Sleep(50 * time.Millisecond)
		committed := RelayAssignment{Slot: "r2", Generation: 2}
		require.NoError(t, store.CommitRelay(ctx, lease, pending, committed))
		a, rec, err = store.RelayConfirmation(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, uint64(2), rec.Commit.Switched)
		require.False(t, rec.Aborted())
		require.False(t, rec.Confirmed(a), "a commit that lands after the deadline never confirms")

		// Evidence written after its deadline is refused inside the atomic write.
		next := *rec.Attempt
		next.Delivered = []string{"0"}
		err = store.CASRelayConfirmation(ctx, lease.SessionID, a, rec, RelayConfirmation{Mode: RelayConfirmDecode, Attempt: &next}, time.Now().Add(-time.Millisecond))
		require.ErrorIs(t, err, ErrRelayAttemptExpired)
		_, after, err := store.RelayConfirmation(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Empty(t, after.Attempt.Delivered)
		require.NoError(t, store.CASRelayConfirmation(ctx, lease.SessionID, a, rec, RelayConfirmation{Mode: RelayConfirmDecode, Attempt: &next}, time.Now().Add(time.Minute)))

		// CommitRelay accepts only a switch to the pending slot.
		require.ErrorIs(t, store.CommitRelay(ctx, lease, committed, RelayAssignment{Slot: "r2", Generation: 2, Pending: "r1"}), ErrRelayTransition)
	})
}
