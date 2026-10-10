package sessionstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

// Block only pre-execution settlement, after the transition actually committed.
type failedSettleHook struct {
	lostResponseHook
	disabled bool
}

func (h *failedSettleHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	args := cmd.Args()
	if !h.disabled && (cmd.Name() == "evalsha" || cmd.Name() == "eval") && len(args) > 7 && args[7] == "settle" {
		return ctx, errors.New("EOF: settlement unavailable")
	}
	return ctx, nil
}
func TestRedisFailedSettlementCarriesCandidate(t *testing.T) {
	for _, op := range []string{"claim", "transfer"} {
		t.Run(op, func(t *testing.T) {
			r := testRedis(t)
			ctx := context.Background()
			a := netip.MustParseAddrPort("127.0.0.1:1")
			b := netip.MustParseAddrPort("127.0.0.1:2")
			var from Lease
			var err error
			if op == "transfer" {
				from, err = r.Claim(ctx, "settlement", a, time.Minute)
				require.NoError(t, err)
			}
			hook := &failedSettleHook{lostResponseHook: lostResponseHook{op: op}}
			r.maintenance.Wait()
			r.client.AddHook(hook)
			if op == "claim" {
				_, err = r.Claim(ctx, "settlement", a, time.Minute)
			} else {
				_, err = r.Transfer(ctx, from, b, time.Minute)
			}
			var transient *TransientError
			require.ErrorAs(t, err, &transient)
			require.NotNil(t, transient.Candidate)
			current, getErr := r.Get(ctx, "settlement")
			require.NoError(t, getErr)
			require.True(t, same(current, *transient.Candidate))
			listed, listErr := r.ListByWorker(ctx, current.Worker)
			require.NoError(t, listErr)
			require.Len(t, listed, 1, "prepublished index survives failed settlement")
			r.maintenance.Wait()
			hook.disabled = true
			settled, committed, settleErr := r.Settle(ctx, *transient.Candidate)
			require.NoError(t, settleErr)
			require.True(t, committed)
			require.Equal(t, current, settled)
		})
	}
}
func TestRedisSettlementFencesDelayedTransition(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	a := netip.MustParseAddrPort("127.0.0.1:1")
	b := netip.MustParseAddrPort("127.0.0.1:2")
	epoch, err := r.allocate(ctx)
	require.NoError(t, err)
	candidate := candidateLease("late-claim", epoch, a)
	_, committed, err := r.Settle(ctx, candidate)
	require.NoError(t, err)
	require.False(t, committed)
	_, err = r.run(ctx, "claim", candidate.SessionID, a.String(), epoch, time.Minute)
	require.ErrorIs(t, err, ErrTransient)
	from, err := r.Claim(ctx, "late-transfer", a, time.Minute)
	require.NoError(t, err)
	epoch, err = r.allocate(ctx)
	require.NoError(t, err)
	candidate = candidateLease(from.SessionID, epoch, b)
	_, committed, err = r.Settle(ctx, candidate)
	require.NoError(t, err)
	require.False(t, committed)
	_, err = r.run(ctx, "transfer", from.SessionID, a.String(), strconv.FormatUint(from.Epoch, 10), time.Minute, b.String(), epoch)
	require.ErrorIs(t, err, ErrTransient)
	current, err := r.Get(ctx, from.SessionID)
	require.NoError(t, err)
	require.Equal(t, from, current)
	// Young prepublications survive listing; old ones are fenced before pruning.
	epoch, err = r.allocate(ctx)
	require.NoError(t, err)
	candidate = candidateLease("pending-index", epoch, b)
	require.NoError(t, r.index(candidate, time.Minute))
	listed, err := r.ListByWorker(ctx, b)
	require.NoError(t, err)
	require.Empty(t, listed)
	score, err := r.client.ZScore(ctx, r.indexKey(b), indexMember(candidate)).Result()
	require.NoError(t, err)
	_, err = r.run(ctx, "claim", candidate.SessionID, b.String(), epoch, time.Minute)
	require.NoError(t, err)
	require.NoError(t, r.Release(ctx, candidate))
	r.maintenance.Wait()
	epoch, err = r.allocate(ctx)
	require.NoError(t, err)
	candidate = candidateLease("old-pending-index", epoch, b)
	require.NoError(t, r.client.ZAdd(ctx, r.indexKey(b), &redis.Z{Score: score - float64(r.indexGrace.Milliseconds()) - 1000, Member: indexMember(candidate)}).Err())
	_, err = r.ListByWorker(ctx, b)
	require.NoError(t, err)
	_, err = r.run(ctx, "claim", candidate.SessionID, b.String(), epoch, time.Minute)
	require.ErrorIs(t, err, ErrTransient)
}
func TestRedisMetadataRetentionAndIndexCleanup(t *testing.T) {
	r := testRedis(t, RedisOptions{Retention: 100 * time.Millisecond})
	ctx := context.Background()
	a := netip.MustParseAddrPort("127.0.0.1:1")
	b := netip.MustParseAddrPort("127.0.0.1:2")
	for i := range 5 {
		lease, err := r.Claim(ctx, fmt.Sprint(i), a, time.Second)
		require.NoError(t, err)
		lease, err = r.Transfer(ctx, lease, b, time.Second)
		require.NoError(t, err)
		require.NoError(t, r.Release(ctx, lease))
	}
	r.maintenance.Wait()
	// A repair can finish after release; listing prunes that bounded stale entry.
	for _, worker := range []netip.AddrPort{a, b} {
		listed, listErr := r.ListByWorker(ctx, worker)
		require.NoError(t, listErr)
		require.Empty(t, listed)
	}
	members, err := r.client.ZRange(ctx, r.indexKey(a), 0, -1).Result()
	require.NoError(t, err)
	require.Empty(t, members)
	members, err = r.client.ZRange(ctx, r.indexKey(b), 0, -1).Result()
	require.NoError(t, err)
	require.Empty(t, members)
	keys, err := r.client.Keys(ctx, r.prefix+"*").Result()
	require.NoError(t, err)
	for _, key := range keys {
		if key == r.prefix+"session_epoch" {
			continue
		}
		ttl, err := r.client.PTTL(ctx, key).Result()
		require.NoError(t, err)
		require.Positive(t, ttl)
	}
	require.Eventually(t, func() bool {
		keys, err := r.client.Keys(ctx, r.prefix+"*").Result()
		if err != nil {
			return false
		}
		for _, key := range keys {
			if key != r.prefix+"session_epoch" && key != r.indexKey(a) && key != r.indexKey(b) {
				return false
			}
		}
		return true
	}, 5*time.Second, 10*time.Millisecond)
}
func TestRedisIndexDoesNotExpireBeforeLongestLease(t *testing.T) {
	r := testRedis(t, RedisOptions{Retention: 100 * time.Millisecond})
	ctx := context.Background()
	a := netip.MustParseAddrPort("127.0.0.1:1")
	_, err := r.Claim(ctx, "long", a, time.Minute)
	require.NoError(t, err)
	_, err = r.Claim(ctx, "short", a, 50*time.Millisecond)
	require.NoError(t, err)
	ttl, err := r.client.PTTL(ctx, r.indexKey(a)).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 50*time.Second)
}
func TestListingPrunesOnlyListedWorker(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		s := factory(t)
		ctx := context.Background()
		a := netip.MustParseAddrPort("127.0.0.1:1")
		b := netip.MustParseAddrPort("127.0.0.1:2")
		_, err := s.Claim(ctx, "expired-other", a, 50*time.Millisecond)
		require.NoError(t, err)
		time.Sleep(80 * time.Millisecond)
		_, err = s.ListByWorker(ctx, b)
		require.NoError(t, err)
		_, err = s.Claim(ctx, "expired-other", b, time.Minute)
		require.ErrorIs(t, err, ErrLeaseHeld)
		_, err = s.ListByWorker(ctx, a)
		require.NoError(t, err)
		_, err = s.Claim(ctx, "expired-other", b, time.Minute)
		require.NoError(t, err)
	})
}
func TestRedisRotationAcrossStores(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	cfg := storage.RedisConfig{Addr: r.client.(*redis.Client).Options().Addr, Prefix: r.prefix}
	oldKey, newKey := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	old, err := NewRedis(ctx, cfg, oldKey, RedisOptions{KeyID: 1, OldKeys: map[byte][]byte{2: newKey}})
	require.NoError(t, err)
	defer func() { require.NoError(t, old.Close()) }()
	next, err := NewRedis(ctx, cfg, newKey, RedisOptions{KeyID: 2, OldKeys: map[byte][]byte{1: oldKey}})
	require.NoError(t, err)
	defer func() { require.NoError(t, next.Close()) }()
	lease, err := old.Claim(ctx, "rotation", netip.MustParseAddrPort("127.0.0.1:1"), time.Minute)
	require.NoError(t, err)
	require.NoError(t, old.PutState(ctx, lease, []byte("old snapshot")))
	got, err := next.GetState(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, "old snapshot", string(got))
	_, err = next.Checkpoint(ctx, lease.SessionID, got)
	require.NoError(t, err, "checkpoint authentication accepts the old master key during rotation")
	require.NoError(t, next.PutState(ctx, lease, []byte("new snapshot")))
	got, err = old.GetState(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, "new snapshot", string(got))
	_, err = old.Checkpoint(ctx, lease.SessionID, got)
	require.NoError(t, err, "checkpoint authentication accepts the new master key on the old writer")
	blob, err := r.client.Get(ctx, r.keys(lease.SessionID)[1]).Bytes()
	require.NoError(t, err)
	require.Equal(t, []byte{1, 2}, blob[:2])
	blob[1] = 1
	require.NoError(t, r.client.Set(ctx, r.keys(lease.SessionID)[1], blob, time.Minute).Err())
	_, err = next.GetState(ctx, lease.SessionID)
	require.Error(t, err)
}

type delayedPutHook struct {
	lostResponseHook
	count   atomic.Int32
	blocked chan struct{}
	release chan struct{}
}

func (h *delayedPutHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	args := cmd.Args()
	if (cmd.Name() == "evalsha" || cmd.Name() == "eval") && len(args) > 7 && args[7] == "put" && h.count.Add(1) == 1 {
		close(h.blocked)
		select {
		case <-h.release:
		case <-ctx.Done():
			return ctx, ctx.Err()
		}
	}
	return ctx, nil
}
func TestRedisDelayedPutCannotRollBackState(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	lease, err := r.Claim(ctx, "ordered", netip.MustParseAddrPort("127.0.0.1:1"), time.Minute)
	require.NoError(t, err)
	require.NoError(t, r.PutState(ctx, lease, []byte("original")))
	raw, err := r.client.Get(ctx, r.keys(lease.SessionID)[1]).Bytes()
	require.NoError(t, err)
	hook := &delayedPutHook{blocked: make(chan struct{}), release: make(chan struct{})}
	r.maintenance.Wait()
	r.client.AddHook(hook)
	late := make(chan error, 1)
	go func() { late <- r.PutState(ctx, lease, []byte("delayed")) }()
	<-hook.blocked
	require.NoError(t, r.PutState(ctx, lease, []byte("newest")))
	close(hook.release)
	require.ErrorIs(t, <-late, ErrStateSuperseded)
	got, err := r.GetState(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, "newest", string(got))
	require.NoError(t, r.client.Set(ctx, r.keys(lease.SessionID)[1], raw, time.Minute).Err())
	got, err = r.GetState(ctx, lease.SessionID)
	require.Error(t, err, "old ciphertext with newer state_seq cannot authenticate")
	require.Nil(t, got)
}

type pipelineCountHook struct {
	lostResponseHook
	batches  int
	commands int
}

func (h *pipelineCountHook) BeforeProcessPipeline(ctx context.Context, cmds []redis.Cmder) (context.Context, error) {
	h.batches++
	h.commands += len(cmds)
	return ctx, nil
}
func TestRedisListingPipelinesAndIsolatesBadMembers(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	a := netip.MustParseAddrPort("127.0.0.1:1")
	for i := range 10 {
		_, err := r.Claim(ctx, fmt.Sprint(i), a, time.Minute)
		require.NoError(t, err)
	}
	require.NoError(t, r.client.ZAdd(ctx, r.indexKey(a), &redis.Z{Score: 0, Member: "invalid JSON"}, &redis.Z{Score: 0, Member: indexMember(Lease{SessionID: "bad-type", Epoch: 99})}).Err())
	require.NoError(t, r.client.Set(ctx, r.keys("bad-type")[0], "corrupt", time.Minute).Err())
	hook := &pipelineCountHook{}
	r.maintenance.Wait()
	r.client.AddHook(hook)
	leases, err := r.ListByWorker(ctx, a)
	require.NoError(t, err)
	require.Len(t, leases, 10)
	require.Equal(t, 1, hook.batches)
	require.Equal(t, 11, hook.commands)
	members, err := r.client.ZRange(ctx, r.indexKey(a), 0, -1).Result()
	require.NoError(t, err)
	require.NotContains(t, members, "invalid JSON")
}

type errorBeforeHook struct {
	lostResponseHook
	op        string
	errorText string
	remaining int
	attempts  int
}

func (h *errorBeforeHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	args := cmd.Args()
	if (cmd.Name() == "evalsha" || cmd.Name() == "eval") && len(args) > 7 && args[7] == h.op {
		h.attempts++
		if h.remaining > 0 {
			h.remaining--
			return ctx, errors.New(h.errorText)
		}
	}
	return ctx, nil
}
func TestRedisErrorClassification(t *testing.T) {
	for _, kind := range []string{"MOVED 1 127.0.0.1:58213", "ASK 1 127.0.0.1:58213", "TRYAGAIN temporary", "CLUSTERDOWN unavailable", "NOAUTH denied", "ERR permanent script error"} {
		t.Run(kind, func(t *testing.T) {
			r := testRedis(t)
			ctx := context.Background()
			lease, err := r.Claim(ctx, "classify", netip.MustParseAddrPort("127.0.0.1:1"), time.Minute)
			require.NoError(t, err)
			hook := &errorBeforeHook{op: "renew", errorText: kind, remaining: 1}
			r.maintenance.Wait()
			r.client.AddHook(hook)
			_, err = r.Renew(ctx, lease, time.Minute)
			if definiteNotRun(redisKind(errors.New(kind))) {
				require.NoError(t, err)
				require.Equal(t, 2, hook.attempts)
			} else {
				require.Error(t, err)
				require.NotErrorIs(t, err, ErrTransient)
				require.Equal(t, 1, hook.attempts)
			}
		})
	}
	r := testRedis(t)
	ctx := context.Background()
	err := r.retryRead(ctx, "permanent", func() error { return errors.New("NOAUTH denied") })
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrTransient)
	_, err = NewRedis(ctx, storage.RedisConfig{}, make([]byte, 32))
	require.ErrorContains(t, err, "address required")
	_, err = NewRedis(ctx, storage.RedisConfig{Cluster: true, Addrs: []string{""}}, make([]byte, 32))
	require.ErrorContains(t, err, "empty Redis cluster address")
}

// Retry idempotent SADD before any lease exists. After commit, cancel the
// caller and fail every repair: the detached prepublication still discovers it.
type indexFaultHook struct {
	mu sync.Mutex
	lostResponseHook
	adds      int
	failures  int
	committed bool
	cancel    context.CancelFunc
}

func (h *indexFaultHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cmd.Name() == "sadd" || (cmd.Name() == "eval" && cmd.Args()[1] == indexLua) {
		h.adds++
		if h.failures > 0 || h.committed {
			if h.failures > 0 {
				h.failures--
			}
			return ctx, errors.New("EOF: index write unavailable")
		}
	}
	return ctx, nil
}
func (h *indexFaultHook) AfterProcess(_ context.Context, cmd redis.Cmder) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	args := cmd.Args()
	if (cmd.Name() == "evalsha" || cmd.Name() == "eval") && len(args) > 7 && (args[7] == "claim" || args[7] == "transfer") && cmd.Err() == nil {
		h.committed = true
		h.cancel()
	}
	return nil
}
func TestRedisPrepublishedIndexSurvivesCancelledRepair(t *testing.T) {
	for _, op := range []string{"claim", "transfer"} {
		t.Run(op, func(t *testing.T) {
			r := testRedis(t)
			ctx := context.Background()
			a := netip.MustParseAddrPort("127.0.0.1:1")
			b := netip.MustParseAddrPort("127.0.0.1:2")
			var lease Lease
			var err error
			if op == "transfer" {
				lease, err = r.Claim(ctx, "pre-index", a, time.Minute)
				require.NoError(t, err)
			}
			caller, cancel := context.WithCancel(ctx)
			defer cancel()
			hook := &indexFaultHook{failures: 2, cancel: cancel}
			r.maintenance.Wait()
			r.client.AddHook(hook)
			if op == "claim" {
				lease, err = r.Claim(caller, "pre-index", a, time.Minute)
			} else {
				lease, err = r.Transfer(caller, lease, b, time.Minute)
			}
			require.NoError(t, err)
			require.ErrorIs(t, caller.Err(), context.Canceled)
			hook.mu.Lock()
			require.GreaterOrEqual(t, hook.adds, 3)
			hook.mu.Unlock()
			listed, err := r.ListByWorker(ctx, lease.Worker)
			require.NoError(t, err)
			require.Len(t, listed, 1)
			require.Equal(t, lease, listed[0])
		})
	}
}

type transientPipelineHook struct {
	lostResponseHook
	failures int
}

func (h *transientPipelineHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	if h.failures > 0 {
		h.failures--
		return ctx, errors.New("EOF: pipeline unavailable")
	}
	return ctx, nil
}
func TestRedisListingRetriesUntrustedPipeline(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	a := netip.MustParseAddrPort("127.0.0.1:1")
	_, err := r.Claim(ctx, "pipeline", a, time.Minute)
	require.NoError(t, err)
	hook := &transientPipelineHook{failures: 1}
	r.maintenance.Wait()
	r.client.AddHook(hook)
	listed, err := r.ListByWorker(ctx, a)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	hook.failures = 3
	listed, err = r.ListByWorker(ctx, a)
	require.ErrorIs(t, err, ErrTransient)
	require.Nil(t, listed, "never trust partial results after exhausted transport failures")
}

// Redis can return io.ErrUnexpectedEOF when a reply is truncated mid-frame.
// This is uncertain, even though its text does not begin with EOF.
type unexpectedEOFHook struct{ lostResponseHook }

func (h *unexpectedEOFHook) AfterProcess(ctx context.Context, cmd redis.Cmder) error {
	if h.lostResponseHook.AfterProcess(ctx, cmd) != nil {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func TestRedisTruncatedTransitionReply(t *testing.T) {
	for _, op := range []string{"claim", "transfer"} {
		t.Run(op, func(t *testing.T) {
			r := testRedis(t)
			ctx := context.Background()
			a := netip.MustParseAddrPort("127.0.0.1:1")
			b := netip.MustParseAddrPort("127.0.0.1:2")
			var lease Lease
			var err error
			if op == "transfer" {
				lease, err = r.Claim(ctx, "truncated", a, time.Minute)
				require.NoError(t, err)
			}
			hook := &unexpectedEOFHook{lostResponseHook: lostResponseHook{op: op}}
			r.maintenance.Wait()
			r.client.AddHook(hook)
			if op == "claim" {
				lease, err = r.Claim(ctx, "truncated", a, time.Minute)
			} else {
				lease, err = r.Transfer(ctx, lease, b, time.Minute)
			}
			require.NoError(t, err)
			require.Equal(t, 1, hook.calls)
			current, err := r.Get(ctx, lease.SessionID)
			require.NoError(t, err)
			require.Equal(t, lease, current)
		})
	}
}

type staleWorkerIndexHook struct {
	lostResponseHook
	key      string
	disabled bool
}

func (h *staleWorkerIndexHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	if !h.disabled && cmd.Name() == "zrem" && cmd.Args()[1] == h.key {
		return ctx, errors.New("EOF: old worker cleanup unavailable")
	}
	return ctx, nil
}
func TestRedisStaleIndexCannotPruneAnotherWorkersRecord(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	a := netip.MustParseAddrPort("127.0.0.1:1")
	b := netip.MustParseAddrPort("127.0.0.1:2")
	old, err := r.Claim(ctx, "stale-worker-index", b, time.Minute)
	require.NoError(t, err)
	hook := &staleWorkerIndexHook{key: r.indexKey(b)}
	r.maintenance.Wait()
	r.client.AddHook(hook)
	_, err = r.Transfer(ctx, old, a, 150*time.Millisecond)
	require.NoError(t, err)
	r.maintenance.Wait()
	hook.disabled = true
	time.Sleep(200 * time.Millisecond)
	leases, err := r.ListByWorker(ctx, b)
	require.NoError(t, err)
	require.Empty(t, leases)
	_, err = r.Claim(ctx, old.SessionID, b, time.Minute)
	require.ErrorIs(t, err, ErrLeaseHeld, "listing the prior worker must not prune the current owner's marker")
	_, err = r.ListByWorker(ctx, a)
	require.NoError(t, err)
	_, err = r.Claim(ctx, old.SessionID, b, time.Minute)
	require.NoError(t, err)
}

func TestRedisAuthenticatesHighBitKeyIDBytes(t *testing.T) {
	// Identical master bytes deliberately isolate header authentication from
	// key selection. Both IDs are valid byte values and must remain distinct.
	r := testRedis(t, RedisOptions{KeyID: 128, OldKeys: map[byte][]byte{129: make([]byte, 32)}})
	ctx := context.Background()
	lease, err := r.Claim(ctx, "binary-key-id", netip.MustParseAddrPort("127.0.0.1:1"), time.Minute)
	require.NoError(t, err)
	require.NoError(t, r.PutState(ctx, lease, []byte("snapshot")))
	blob, err := r.client.Get(ctx, r.keys(lease.SessionID)[1]).Bytes()
	require.NoError(t, err)
	blob[1] = 129
	require.NoError(t, r.client.Set(ctx, r.keys(lease.SessionID)[1], blob, time.Minute).Err())
	state, err := r.GetState(ctx, lease.SessionID)
	require.Error(t, err, "tampered key ID must fail authentication even when configured keys alias")
	require.Nil(t, state)
}

func TestStoreIndexesPreserveSessionIDBytes(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		store := factory(t)
		ctx := context.Background()
		worker := netip.MustParseAddrPort("127.0.0.1:1")
		want := make(map[string]Lease)
		for _, id := range []string{"brace}suffix", "{brace}", "~encoded", "nul\x00id", "nonutf\xff"} {
			lease, err := store.Claim(ctx, id, worker, time.Minute)
			require.NoError(t, err)
			want[id] = lease
		}
		leases, err := store.ListByWorker(ctx, worker)
		require.NoError(t, err)
		require.Len(t, leases, len(want))
		for _, lease := range leases {
			require.Equal(t, want[lease.SessionID], lease)
			delete(want, lease.SessionID)
		}
		require.Empty(t, want)
	})
}
