package sessionstore

import (
	"context"
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestControlLeaseFencesMetadataAndRetainsEpoch(t *testing.T) {
	forComponentStores(t, func(t *testing.T, store Store) {
		ctx := context.Background()
		control := store.(ControlStore)
		first, err := control.ClaimControl(ctx, "first", time.Second)
		require.NoError(t, err)
		_, err = control.ClaimControl(ctx, "second", time.Second)
		require.ErrorIs(t, err, ErrLeaseHeld)
		require.NoError(t, control.PutControlRecord(ctx, first, "drain:a", json.RawMessage(`true`)))
		renewed, err := control.RenewControl(ctx, first, time.Second)
		require.NoError(t, err)
		require.Equal(t, first.Epoch, renewed.Epoch)
		require.NoError(t, control.ReleaseControl(ctx, first))
		next, err := control.ClaimControl(ctx, "second", time.Second)
		require.NoError(t, err)
		require.Greater(t, next.Epoch, first.Epoch)
		require.ErrorIs(t, control.PutControlRecord(ctx, first, "drain:a", nil), ErrLeaseLost)
		_, err = control.RenewControl(ctx, first, time.Second)
		require.ErrorIs(t, err, ErrLeaseLost)
		require.ErrorIs(t, control.ReleaseControl(ctx, first), ErrLeaseLost)
		records, err := control.ControlRecords(ctx, next)
		require.NoError(t, err)
		require.JSONEq(t, `true`, string(records["drain:a"]))
	})
}
func TestControlLeaseExpiryCannotBeRenewed(t *testing.T) {
	forComponentStores(t, func(t *testing.T, store Store) {
		ctx := context.Background()
		control := store.(ControlStore)
		first, err := control.ClaimControl(ctx, "first", 10*time.Millisecond)
		require.NoError(t, err)
		require.Eventually(t, func() bool { _, err := control.RenewControl(ctx, first, time.Millisecond); return err == ErrLeaseLost }, time.Second, 20*time.Millisecond)
		next, err := control.ClaimControl(ctx, "second", time.Second)
		require.NoError(t, err)
		require.Greater(t, next.Epoch, first.Epoch)
	})
}
func TestControlIntentPersistsCandidateBeforeTransfer(t *testing.T) {
	forComponentStores(t, func(t *testing.T, store Store) {
		ctx := context.Background()
		control := store.(ControlStore)
		a, b := netip.MustParseAddrPort("127.0.0.1:23456"), netip.MustParseAddrPort("127.0.0.1:23457")
		lease, err := store.Claim(ctx, "intent{slot}", a, time.Minute)
		require.NoError(t, err)
		intent := ControlIntent{ID: lease.SessionID, Token: "first", Kind: "move", Started: time.Now(), Lease: lease}
		require.NoError(t, control.PutControlIntent(ctx, intent))
		moved, err := store.Transfer(WithTransitionIntent(ctx, intent.ID, intent.Token), lease, b, time.Minute)
		require.NoError(t, err)
		intents, err := control.ControlIntents(ctx)
		require.NoError(t, err)
		require.Len(t, intents, 1)
		require.NotNil(t, intents[0].Candidate)
		require.Equal(t, moved.Worker, intents[0].Candidate.Worker)
		require.Equal(t, moved.Epoch, intents[0].Candidate.Epoch)
		settled, committed, err := store.(TransitionResolver).Settle(ctx, *intents[0].Candidate)
		require.NoError(t, err)
		require.True(t, committed)
		require.Equal(t, moved.Epoch, settled.Epoch)
		require.ErrorIs(t, control.DeleteControlIntent(ctx, intent.ID, "stale"), ErrLeaseLost)
		require.NoError(t, control.DeleteControlIntent(ctx, intent.ID, intent.Token))
		_, err = store.Transfer(WithTransitionIntent(ctx, intent.ID, intent.Token), moved, a, time.Minute)
		require.ErrorIs(t, err, ErrLeaseLost)
		current, err := store.Get(ctx, intent.ID)
		require.NoError(t, err)
		require.Equal(t, moved.Epoch, current.Epoch)
	})
}
func TestControlRecordsAreBounded(t *testing.T) {
	store := NewMemory()
	ctx := context.Background()
	lease, err := store.ClaimControl(ctx, "holder", time.Second)
	require.NoError(t, err)
	require.ErrorIs(t, store.PutControlRecord(ctx, lease, "large", make([]byte, MaxControlRecordBytes+1)), ErrControlLimit)
	for i := range MaxControlRecords {
		require.NoError(t, store.PutControlRecord(ctx, lease, time.Duration(i).String(), json.RawMessage(`true`)))
	}
	require.ErrorIs(t, store.PutControlRecord(ctx, lease, "overflow", json.RawMessage(`true`)), ErrControlLimit)
}

// A lost initial claim reply must not let a retry overstate authority locally:
// the same-holder retry refreshes TTL atomically without changing the epoch.
func TestControlClaimRetryRefreshesSameTenure(t *testing.T) {
	forComponentStores(t, func(t *testing.T, store Store) {
		ctx := context.Background()
		control := store.(ControlStore)
		first, err := control.ClaimControl(ctx, "same-holder", time.Second)
		require.NoError(t, err)
		retry, err := control.ClaimControl(ctx, "same-holder", 2*time.Second)
		require.NoError(t, err)
		require.Equal(t, first.Epoch, retry.Epoch)
		require.True(t, retry.ExpiresAt.After(first.ExpiresAt.Add(900*time.Millisecond)))
		_, err = control.ClaimControl(ctx, "contender", time.Second)
		require.ErrorIs(t, err, ErrLeaseHeld)
	})
}
