package sessionstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func TestConcurrentClaimsExactlyOneWins(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		store := factory(t)
		ctx := context.Background()
		start := make(chan struct{})
		results := make(chan error, 20)
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Go(func() {
				<-start
				_, err := store.Claim(ctx, "race", netip.MustParseAddrPort(fmt.Sprintf("127.0.0.1:%d", i+1)), time.Minute)
				results <- err
			})
		}
		close(start)
		wg.Wait()
		close(results)
		wins := 0
		for err := range results {
			if err == nil {
				wins++
			} else {
				require.ErrorIs(t, err, ErrLeaseHeld)
			}
		}
		require.Equal(t, 1, wins)
	})
}
func TestExpiredReleaseAllowsReclaim(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		store := factory(t)
		ctx := context.Background()
		worker := netip.MustParseAddrPort("127.0.0.1:1")
		lease, err := store.Claim(ctx, "expired-release", worker, 10*time.Millisecond)
		require.NoError(t, err)
		time.Sleep(20 * time.Millisecond)
		wrong := lease
		wrong.Worker = netip.MustParseAddrPort("127.0.0.1:2")
		require.NoError(t, store.Release(ctx, wrong))
		_, err = store.Claim(ctx, lease.SessionID, worker, time.Minute)
		require.ErrorIs(t, err, ErrLeaseHeld)
		require.NoError(t, store.Release(ctx, lease))
		next, err := store.Claim(ctx, lease.SessionID, worker, time.Minute)
		require.NoError(t, err)
		require.Greater(t, next.Epoch, lease.Epoch)
	})
}
func TestRedisEncryptionAuthenticationAndSessionBinding(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	worker := netip.MustParseAddrPort("127.0.0.1:1")
	lease, err := r.Claim(ctx, "secret", worker, time.Minute)
	require.NoError(t, err)
	// Include raw and snapshot-style base64 representations of DTLS, SRTP keys
	// and salts. Neither representation may be readable from the Redis bytes.
	secrets := [][]byte{bytes.Repeat([]byte{0xa1}, 48), bytes.Repeat([]byte{0xb2}, 16), bytes.Repeat([]byte{0xc3}, 14), bytes.Repeat([]byte{0xd4}, 16), bytes.Repeat([]byte{0xe5}, 14)}
	state := []byte("DTLS master secret, SRTP local/remote keys and salts:")
	for _, secret := range secrets {
		state = append(state, secret...)
		state = append(state, base64.StdEncoding.EncodeToString(secret)...)
	}
	require.NoError(t, r.PutState(ctx, lease, state))
	raw, err := r.client.Get(ctx, r.keys("secret")[1]).Bytes()
	require.NoError(t, err)
	var cursor uint64
	for {
		keys, next, err := r.client.Scan(ctx, cursor, r.prefix+"*", 100).Result()
		require.NoError(t, err)
		for _, key := range keys {
			kind, err := r.client.Type(ctx, key).Result()
			require.NoError(t, err)
			all := []byte(key)
			switch kind {
			case "string":
				value, err := r.client.Get(ctx, key).Bytes()
				require.NoError(t, err)
				all = append(all, value...)
			case "hash":
				value, err := r.client.HGetAll(ctx, key).Result()
				require.NoError(t, err)
				for k, v := range value {
					all = append(all, k...)
					all = append(all, v...)
				}
			case "zset":
				value, err := r.client.ZRange(ctx, key, 0, -1).Result()
				require.NoError(t, err)
				all = append(all, strings.Join(value, " ")...)
			case "set":
				value, err := r.client.SMembers(ctx, key).Result()
				require.NoError(t, err)
				for _, v := range value {
					all = append(all, v...)
				}
			default:
				t.Fatalf("unexpected key type %s", kind)
			}
			for _, secret := range secrets {
				require.False(t, bytes.Contains(all, secret), key)
				require.False(t, bytes.Contains(all, []byte(base64.StdEncoding.EncodeToString(secret))), key)
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	got, err := r.GetState(ctx, "secret")
	require.NoError(t, err)
	require.Equal(t, state, got)
	require.NoError(t, r.PutState(ctx, lease, state))
	again, err := r.client.Get(ctx, r.keys("secret")[1]).Bytes()
	require.NoError(t, err)
	require.NotEqual(t, raw, again, "fresh nonce per put")
	other, err := r.Claim(ctx, "other", worker, time.Minute)
	require.NoError(t, err)
	require.NoError(t, r.client.Set(ctx, r.keys(other.SessionID)[1], raw, time.Minute).Err())
	_, err = r.GetState(ctx, other.SessionID)
	require.Error(t, err, "AAD rejects blob copied to another session")
	wrong, err := NewRedis(ctx, storage.RedisConfig{Addr: r.client.(*redis.Client).Options().Addr, Prefix: r.prefix}, bytes.Repeat([]byte{1}, 32))
	require.NoError(t, err)
	defer func() { require.NoError(t, wrong.Close()) }()
	_, err = wrong.GetState(ctx, "secret")
	require.Error(t, err, "wrong key must fail authentication")
	for _, blob := range [][]byte{[]byte("short"), append(bytes.Clone(raw[:len(raw)-1]), raw[len(raw)-1]^1)} {
		require.NoError(t, r.client.Set(ctx, r.keys("secret")[1], blob, time.Minute).Err())
		got, err = r.GetState(ctx, "secret")
		require.Error(t, err)
		require.Nil(t, got)
	}
}
func TestRedisLeaseAndStateTTLAndHangupDeletion(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	a, b := netip.MustParseAddrPort("127.0.0.1:1"), netip.MustParseAddrPort("127.0.0.1:2")
	lease, err := r.Claim(ctx, "ttl", a, time.Second)
	require.NoError(t, err)
	require.NoError(t, r.PutState(ctx, lease, []byte("key material")))
	for _, change := range []func(Lease) (Lease, error){func(l Lease) (Lease, error) { return r.Renew(ctx, l, time.Minute) }, func(l Lease) (Lease, error) { return r.Transfer(ctx, l, b, time.Minute) }} {
		lease, err = change(lease)
		require.NoError(t, err)
		// Read absolute expiry in one Redis script: no sampling-roundtrip skew.
		times, err := r.client.Eval(ctx, `return {redis.call('PEXPIRETIME', KEYS[1]), redis.call('PEXPIRETIME', KEYS[2])}`, r.keys("ttl")[:2]).Int64Slice()
		require.NoError(t, err)
		require.Equal(t, times[0], times[1])
		require.Equal(t, lease.ExpiresAt.UnixMilli(), times[0])
	}
	time.Sleep(1100 * time.Millisecond)
	_, err = r.GetState(ctx, "ttl")
	require.NoError(t, err, "renew and transfer carried state beyond original TTL")
	require.NoError(t, r.Release(ctx, lease))
	count, err := r.client.Exists(ctx, r.keys("ttl")[:3]...).Result()
	require.NoError(t, err)
	require.Zero(t, count, "hangup deletes lease, state and claim marker")
	abandoned, err := r.Claim(ctx, "abandoned", a, time.Second)
	require.NoError(t, err)
	require.NoError(t, r.PutState(ctx, abandoned, []byte("abandoned secret")))
	time.Sleep(1100 * time.Millisecond)
	count, err = r.client.Exists(ctx, r.keys("abandoned")[:2]...).Result()
	require.NoError(t, err)
	require.Zero(t, count, "both expire without any store lookup")
}
func TestRedisConfigurationAndEpochPrecision(t *testing.T) {
	r := testRedis(t) // Skip before constructing any clients.
	ctx := context.Background()
	cfg := storage.RedisConfig{Addr: r.client.(*redis.Client).Options().Addr, Prefix: r.prefix}
	_, err := NewRedis(ctx, cfg, []byte("short"))
	require.ErrorContains(t, err, "32 bytes")
	t.Setenv("RELAIS_SESSIONSTORE_KEY", "invalid*")
	_, err = NewRedis(ctx, cfg, nil)
	require.ErrorContains(t, err, "decode RELAIS_SESSIONSTORE_KEY")
	t.Setenv("RELAIS_SESSIONSTORE_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	envStore, err := NewRedis(ctx, cfg, nil)
	require.NoError(t, err)
	require.NoError(t, envStore.Close())
	require.Equal(t, 0, r.client.(*redis.Client).Options().MaxRetries, "client cannot replay unknown scripts")
	require.NoError(t, r.client.Set(ctx, r.prefix+"session_epoch", "9007199254740992", 0).Err())
	a := netip.MustParseAddrPort("127.0.0.1:1")
	b := netip.MustParseAddrPort("127.0.0.1:2")
	lease, err := r.Claim(ctx, "big-epoch", a, time.Minute)
	require.NoError(t, err)
	require.Equal(t, uint64(9007199254740993), lease.Epoch)
	next, err := r.Transfer(ctx, lease, b, time.Minute)
	require.NoError(t, err)
	require.Equal(t, lease.Epoch+1, next.Epoch)
	// A delayed allocation cannot repeat or lower the epoch after release.
	require.NoError(t, r.Release(ctx, next))
	_, err = r.run(ctx, "claim", lease.SessionID, a.String(), fmt.Sprint(lease.Epoch), time.Minute)
	require.ErrorIs(t, err, ErrTransient)
	for _, id := range []string{"plain", "brace}suffix", "{brace}", "~encoded"} {
		keys := r.keys(id)
		tag := keys[0][len(r.prefix)+len("lease:"):]
		for _, key := range keys {
			require.Contains(t, key, tag)
		}
	}
}

// Inject the real failure seam: let Redis execute a script, then drop its reply.
// The store must reconcile ownership without executing the mutation again.
type lostResponseHook struct {
	op    string
	calls int
}

func (h *lostResponseHook) BeforeProcess(ctx context.Context, _ redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (h *lostResponseHook) AfterProcess(_ context.Context, cmd redis.Cmder) error {
	args := sessionCommandArgs(cmd)
	if (cmd.Name() == "evalsha" || cmd.Name() == "eval") && len(args) > 0 && args[0] == h.op && cmd.Err() == nil {
		h.calls++
		return errors.New("EOF: injected lost script response")
	}
	return nil
}
func (*lostResponseHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (*lostResponseHook) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }
func TestRedisUnknownClaimAndTransferReconcileWithoutReplay(t *testing.T) {
	for _, op := range []string{"claim", "transfer"} {
		t.Run(op, func(t *testing.T) {
			r := testRedis(t)
			hook := &lostResponseHook{op: op}
			r.maintenance.Wait()
			r.client.AddHook(hook)
			ctx := context.Background()
			a, b := netip.MustParseAddrPort("127.0.0.1:1"), netip.MustParseAddrPort("127.0.0.1:2")
			lease, err := r.Claim(ctx, "uncertain", a, time.Minute)
			require.NoError(t, err)
			if op == "transfer" {
				lease, err = r.Transfer(ctx, lease, b, time.Minute)
				require.NoError(t, err)
				require.Equal(t, b, lease.Worker)
			}
			current, err := r.Get(ctx, "uncertain")
			require.NoError(t, err)
			require.Equal(t, lease, current)
			require.Equal(t, 1, hook.calls)
		})
	}
}
func TestLeaseOperationLatency(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		store := factory(t)
		ctx := context.Background()
		worker := netip.MustParseAddrPort("127.0.0.1:1")
		samples := map[string][]time.Duration{}
		for i := range 100 {
			id := fmt.Sprintf("latency-%d", i)
			start := time.Now()
			lease, err := store.Claim(ctx, id, worker, time.Minute)
			samples["claim"] = append(samples["claim"], time.Since(start))
			require.NoError(t, err)
			start = time.Now()
			lease, err = store.Renew(ctx, lease, time.Minute)
			samples["renew"] = append(samples["renew"], time.Since(start))
			require.NoError(t, err)
			start = time.Now()
			lease, err = store.Transfer(ctx, lease, netip.MustParseAddrPort("127.0.0.1:2"), time.Minute)
			samples["transfer"] = append(samples["transfer"], time.Since(start))
			require.NoError(t, err)
			start = time.Now()
			require.NoError(t, store.Release(ctx, lease))
			samples["release"] = append(samples["release"], time.Since(start))
		}
		for _, op := range []string{"claim", "renew", "transfer", "release"} {
			values := samples[op]
			sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
			t.Logf("LEASE_LATENCY op=%s samples=100 p50=%s p99=%s", op, values[49], values[98])
		}
	})
}

func TestRedisUnknownMutationReturnsTransientWithoutReplay(t *testing.T) {
	for _, op := range []string{"renew", "put", "release"} {
		t.Run(op, func(t *testing.T) {
			r := testRedis(t)
			ctx := context.Background()
			lease, err := r.Claim(ctx, "uncertain", netip.MustParseAddrPort("127.0.0.1:1"), time.Minute)
			require.NoError(t, err)
			hook := &lostResponseHook{op: op}
			r.maintenance.Wait()
			r.client.AddHook(hook)
			switch op {
			case "renew":
				_, err = r.Renew(ctx, lease, time.Minute)
			case "put":
				err = r.PutState(ctx, lease, []byte("state"))
			case "release":
				err = r.Release(ctx, lease)
			}
			require.ErrorIs(t, err, ErrTransient)
			require.NotErrorIs(t, err, ErrLeaseLost)
			require.NotErrorIs(t, err, ErrNotFound)
			require.Equal(t, 1, hook.calls)
		})
	}
}

// A committed transition with a lost reply must survive failure of the old
// reconcile read. Settlement is independent of Get and cannot prove a false negative.
type failedReconcileHook struct {
	lostResponseHook
	dropped bool
}

func (h *failedReconcileHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	args := sessionCommandArgs(cmd)
	if h.dropped && (cmd.Name() == "evalsha" || cmd.Name() == "eval") && len(args) > 0 && args[0] == "get" {
		return ctx, errors.New("EOF: injected failed reconcile")
	}
	return ctx, nil
}
func (h *failedReconcileHook) AfterProcess(ctx context.Context, cmd redis.Cmder) error {
	err := h.lostResponseHook.AfterProcess(ctx, cmd)
	if err != nil {
		h.dropped = true
	}
	return err
}
func TestRedisDroppedReplyAndFailedReconcile(t *testing.T) {
	for _, op := range []string{"claim", "transfer"} {
		t.Run(op, func(t *testing.T) {
			r := testRedis(t)
			ctx := context.Background()
			a := netip.MustParseAddrPort("127.0.0.1:1")
			b := netip.MustParseAddrPort("127.0.0.1:2")
			var lease Lease
			var err error
			if op == "transfer" {
				lease, err = r.Claim(ctx, "fault", a, time.Minute)
				require.NoError(t, err)
			}
			hook := &failedReconcileHook{lostResponseHook: lostResponseHook{op: op}}
			r.maintenance.Wait()
			r.client.AddHook(hook)
			if op == "claim" {
				lease, err = r.Claim(ctx, "fault", a, time.Minute)
			} else {
				lease, err = r.Transfer(ctx, lease, b, time.Minute)
			}
			require.NoError(t, err)
			require.Equal(t, 1, hook.calls)
			listed, err := r.ListByWorker(ctx, lease.Worker)
			require.NoError(t, err)
			require.Len(t, listed, 1)
		})
	}
}
