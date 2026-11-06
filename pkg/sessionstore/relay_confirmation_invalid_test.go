package sessionstore

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func setRawRelayConfirmation(t *testing.T, store Store, id string, raw [3]string) {
	t.Helper()
	switch s := store.(type) {
	case *Memory:
		s.mu.Lock()
		if s.relayConfirmations == nil {
			s.relayConfirmations = make(map[string][3]string)
		}
		s.relayConfirmations[id] = raw
		s.mu.Unlock()
	case *Redis:
		require.NoError(t, s.client.HSet(context.Background(), s.keys(id)[0], "relay_confirmation", raw[0], "relay_confirmation_state", raw[1], "relay_episode", raw[2]).Err())
	default:
		t.Fatalf("unknown store %T", store)
	}
}

type replaceBeforeConfirmationClear struct {
	lostResponseHook
	store *Redis
	id    string
	once  sync.Once
}

func (h *replaceBeforeConfirmationClear) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	args := cmd.Args()
	if len(args) < 2 || (cmd.Name() != "evalsha" && cmd.Name() != "eval") ||
		args[1] != clearInvalidRelayConfirmationScript.Hash() && args[1] != clearInvalidRelayConfirmationLua {
		return ctx, nil
	}
	var err error
	h.once.Do(func() {
		err = h.store.client.HSet(ctx, h.store.keys(h.id)[0], "relay_confirmation", RelayConfirmDecode, "relay_confirmation_state", "", "relay_episode", "").Err()
	})
	return ctx, err
}

func TestRedisInvalidConfirmationClearPreservesConcurrentValidWrite(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	lease, err := r.Claim(ctx, "replace-before-clear", netip.MustParseAddrPort("127.0.0.1:17001"), time.Minute)
	require.NoError(t, err)
	setRawRelayConfirmation(t, r, lease.SessionID, [3]string{"unknown", "", ""})
	r.maintenance.Wait()
	r.client.AddHook(redisFaultHook{&replaceBeforeConfirmationClear{store: r, id: lease.SessionID}})
	require.ErrorIs(t, r.ClearInvalidRelayConfirmation(ctx, lease.SessionID, time.Now().Add(time.Minute)), ErrRelayConflict)
	_, rec, err := r.RelayConfirmation(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, RelayConfirmation{Mode: RelayConfirmDecode}, rec)
}

func TestClearInvalidRelayConfirmation(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		for name, raw := range map[string][3]string{
			"mode":         {"unknown", "", ""},
			"attempt-json": {RelayConfirmDecode, "{", ""},
			"phase":        {RelayConfirmDecode, `{"slot":"r2","phase":"unknown"}`, ""},
			"episode-json": {RelayConfirmDecode, "", "{"},
		} {
			t.Run(name, func(t *testing.T) {
				s := factory(t)
				store := s.(RelayConfirmations)
				ctx := context.Background()
				lease, err := s.Claim(ctx, "invalid-confirmation", netip.MustParseAddrPort("127.0.0.1:17001"), time.Minute)
				require.NoError(t, err)
				initial := RelayAssignment{Slot: "r1"}
				require.NoError(t, store.CASRelay(ctx, lease, RelayAssignment{}, initial))
				pending := RelayAssignment{Slot: "r1", Pending: "r2"}
				require.NoError(t, store.CASRelay(ctx, lease, initial, pending))
				assignment := RelayAssignment{Slot: "r2", Generation: 1}
				require.NoError(t, store.CommitRelay(ctx, lease, pending, assignment))
				_, before, err := store.RelayConfirmation(ctx, lease.SessionID)
				require.NoError(t, err)
				setRawRelayConfirmation(t, s, lease.SessionID, raw)
				_, _, err = store.RelayConfirmation(ctx, lease.SessionID)
				require.ErrorIs(t, err, ErrRelayConfirmationInvalid)
				now, err := s.(interface {
					Clock(context.Context, string) (time.Time, error)
				}).Clock(ctx, lease.SessionID)
				require.NoError(t, err)
				require.ErrorIs(t, store.ClearInvalidRelayConfirmation(ctx, lease.SessionID, now.Add(-time.Second)), ErrRelayAttemptExpired, "an expired control cannot delete the successor's record")
				_, _, err = store.RelayConfirmation(ctx, lease.SessionID)
				require.ErrorIs(t, err, ErrRelayConfirmationInvalid, "a refused clear preserves the record")
				require.NoError(t, store.ClearInvalidRelayConfirmation(ctx, lease.SessionID, now.Add(time.Minute)))
				a, rec, err := store.RelayConfirmation(ctx, lease.SessionID)
				require.NoError(t, err)
				require.Equal(t, assignment, a, "clearing confirmation does not change routing")
				require.Equal(t, RelayConfirmation{Commit: before.Commit}, rec, "the switch commit survives")
				current, err := s.Get(ctx, lease.SessionID)
				require.NoError(t, err)
				require.Equal(t, lease, current, "clearing confirmation does not end the call")
				require.NoError(t, store.ClearInvalidRelayConfirmation(ctx, lease.SessionID, now.Add(time.Minute)), "retry after a lost clear reply is idempotent")
				valid := RelayConfirmation{Mode: RelayConfirmDecode, Commit: before.Commit}
				require.NoError(t, store.CASRelayConfirmation(ctx, lease.SessionID, a, rec, valid, time.Time{}, time.Time{}))
				require.ErrorIs(t, store.ClearInvalidRelayConfirmation(ctx, lease.SessionID, now.Add(time.Minute)), ErrRelayConflict, "a valid replacement must survive")
				_, rec, err = store.RelayConfirmation(ctx, lease.SessionID)
				require.NoError(t, err)
				require.Equal(t, valid, rec)
				require.NoError(t, s.Release(ctx, lease))
				require.ErrorIs(t, store.ClearInvalidRelayConfirmation(ctx, lease.SessionID, now.Add(time.Minute)), ErrNotFound)
			})
		}
	})
}
