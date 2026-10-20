package sessionstore

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	codec "github.com/relaishq/relais/internal/componentcodec"
	"github.com/relaishq/relais/internal/redistestcluster"
	"github.com/stretchr/testify/require"
)

type componentFaultHook struct {
	r     *Redis
	mode  string
	armed atomic.Bool
	trips atomic.Int64
}

func (h *componentFaultHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	args := cmd.Args()
	if len(args) < 3 || cmd.Name() != "evalsha" || fmt.Sprint(args[1]) != componentCommitScript.Hash() || h.mode == "lost-reply" || !h.armed.CompareAndSwap(true, false) {
		return ctx, nil
	}
	h.trips.Add(1)
	if h.mode == "before-write" {
		return ctx, io.EOF
	}
	if h.mode == "after-bank" {
		// Execute the actual production script up to its publish boundary. Redis
		// does not roll back writes when Lua fails. The caller sees the script error.
		script := strings.Replace(componentCommitLua, "-- All expiries precede publication; errors retain the previous referenced banks.", "error('injected-before-publish')", 1)
		_, err := h.r.client.Eval(ctx, script, h.r.componentKeys("fault"), sessionCommandArgs(cmd)...).Result()
		return ctx, err
	}
	return ctx, errors.New("injected component fault")
}
func (h *componentFaultHook) AfterProcess(_ context.Context, cmd redis.Cmder) error {
	args := cmd.Args()
	if len(args) >= 3 && cmd.Name() == "evalsha" && fmt.Sprint(args[1]) == componentCommitScript.Hash() && h.mode == "lost-reply" && cmd.Err() == nil && h.armed.CompareAndSwap(true, false) {
		h.trips.Add(1)
		return io.EOF
	}
	return nil
}
func (*componentFaultHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (*componentFaultHook) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }
func forComponentRedis(t *testing.T, test func(*testing.T, *Redis)) {
	t.Helper()
	t.Run("redis", func(t *testing.T) { test(t, testRedis(t)) })
	t.Run("cluster", func(t *testing.T) { addrs := redistestcluster.Addresses(t); test(t, componentCluster(t, addrs)) })
}
func TestComponentPublicationFailuresAndSettlement(t *testing.T) {
	forComponentRedis(t, func(t *testing.T, r *Redis) {
		ctx := context.Background()
		lease, err := r.Claim(ctx, "fault", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		first, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
		require.NoError(t, err)
		hook := &componentFaultHook{r: r}
		r.maintenance.Wait()
		r.client.AddHook(hook)
		hook.mode = "after-bank"
		hook.armed.Store(true)
		changed := cloneComponents(first.Components)
		changed.History = appendComponentBytes(t, changed.History, codec.History, 1)
		_, err = r.CommitComponents(ctx, lease, ComponentCommit{Components: changed, StateVersion: 1, Expected: &first.Metadata})
		require.ErrorContains(t, err, "injected-before-publish")
		require.EqualValues(t, 1, hook.trips.Load())
		restored, err := r.ReadComponents(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, first.Metadata, restored.Metadata)
		require.Equal(t, first.Components, restored.Components)
		bank := 1 - int(first.Metadata.Banks[2])
		orphan, err := r.client.Get(ctx, r.componentKeys("fault")[9+bank]).Bytes()
		require.NoError(t, err)
		require.Greater(t, len(orphan), 1, "failure actually wrote the inactive bank")
		require.Positive(t, r.client.PTTL(ctx, r.componentKeys("fault")[9+bank]).Val(), "a preparation error must not leave an immortal inactive bank")
		hook.mode = "before-write"
		hook.armed.Store(true)
		retry, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: changed, StateVersion: 1, Expected: &first.Metadata})
		require.NoError(t, err)
		require.Equal(t, changed, retry.Components)
		require.EqualValues(t, 2, hook.trips.Load())
		// Lose the acknowledged history commit reply. Settlement is a tuple read,
		// never a blind replay, and charges the successful write exactly once.
		hook.mode = "lost-reply"
		hook.armed.Store(true)
		changed.History = appendComponentBytes(t, changed.History, codec.History, 2)
		beforeSeq, err := r.client.HGet(ctx, r.keys("fault")[0], "next_seq").Int64()
		require.NoError(t, err)
		settled, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: changed, StateVersion: 1, Expected: &retry.Metadata})
		require.NoError(t, err)
		afterSeq, err := r.client.HGet(ctx, r.keys("fault")[0], "next_seq").Int64()
		require.NoError(t, err)
		require.Equal(t, beforeSeq+1, afterSeq)
		require.Positive(t, settled.WrittenBytes)
		require.EqualValues(t, 3, hook.trips.Load())
		require.Equal(t, changed, settled.Components)
		// Bypass the client precheck as well: a delayed host-only command carrying
		// the stale expected root must be rejected by Lua before *any* mutation.
		reserved, err := r.run(ctx, "sequence", "fault", lease.Worker.String(), fmt.Sprint(lease.Epoch), 0)
		require.NoError(t, err)
		seq, err := sequenceNumber(fmt.Sprint(reserved[1]))
		require.NoError(t, err)
		staleRecord := componentRecord{metadata: retry.Metadata}
		engine := componentEngine(r)
		// Recreate a valid stale capture using retained-key seals for preparation.
		for i, kind := range []byte{1, 3, 4, 2} {
			dest := []*[]byte{&staleRecord.sealed.Setup, &staleRecord.sealed.Work, &staleRecord.sealed.History, &staleRecord.sealed.Transport}[i]
			*dest, err = sealForTest(engine, lease.SessionID, kind, retry.Metadata, [][]byte{retry.Components.Setup, retry.Components.Work, retry.Components.History, retry.Components.Transport}[i])
			require.NoError(t, err)
		}
		p, err := r.prepareComponents(lease, ComponentCommit{Components: retry.Components, StateVersion: 1, Expected: &retry.Metadata}, &staleRecord, seq)
		require.NoError(t, err)
		before := snapshotComponentKeys(t, r, "fault")
		packed, _ := p.record.metadata.Marshal()
		expected, _ := retry.Metadata.Marshal()
		_, err = r.runComponent(ctx, componentCommitScript, "component_commit", "fault", lease.Worker.String(), fmt.Sprint(lease.Epoch), expected, packed, p.record.sealed.Transport, fmt.Sprint(seq), binary.BigEndian.AppendUint64(nil, lease.Epoch), []byte{}, []byte{}, []byte{}, binary.BigEndian.AppendUint64(nil, seq))
		require.ErrorIs(t, err, ErrStateSuperseded)
		require.Equal(t, before, snapshotComponentKeys(t, r, "fault"), "stale root refusal precedes all writes")
		restored, err = r.ReadComponents(ctx, "fault")
		require.NoError(t, err)
		require.Equal(t, settled.Components.History, restored.Components.History)
	})
}
func snapshotComponentKeys(t *testing.T, r *Redis, id string) map[string]string {
	t.Helper()
	values := map[string]string{}
	ctx := context.Background()
	for _, key := range r.componentKeys(id) {
		kind := r.client.Type(ctx, key).Val()
		switch kind {
		case "hash":
			v, e := r.client.HGetAll(ctx, key).Result()
			require.NoError(t, e)
			values[key] = fmt.Sprint(v)
		case "string":
			v, e := r.client.Get(ctx, key).Result()
			require.NoError(t, e)
			values[key] = v
		case "none":
			values[key] = "none"
		}
	}
	return values
}
func TestComponentRotationFailureRetainsPublishedTuple(t *testing.T) {
	forComponentRedis(t, func(t *testing.T, r *Redis) {
		ctx := context.Background()
		lease, err := r.Claim(ctx, "fault", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		first, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
		require.NoError(t, err)
		r.keysByID[1] = make([]byte, 32)
		r.keysByID[1][0] = 1
		r.activeKey = 1
		hook := &componentFaultHook{r: r, mode: "after-bank"}
		hook.armed.Store(true)
		r.maintenance.Wait()
		r.client.AddHook(hook)
		_, err = r.CommitComponents(ctx, lease, ComponentCommit{Components: first.Components, StateVersion: 1, Expected: &first.Metadata})
		require.ErrorContains(t, err, "injected-before-publish")
		restored, err := r.ReadComponents(ctx, "fault")
		require.NoError(t, err)
		require.Equal(t, first.Metadata, restored.Metadata)
		require.Equal(t, first.Components, restored.Components)
	})
}

func sealForTest(r *Redis, id string, kind byte, m ComponentMetadata, data []byte) ([]byte, error) {
	index := map[byte]int{1: 0, 3: 1, 4: 2, 2: 4}[kind]
	key := m.References[index].KeyID
	return codec.Seal(r.keysByID[key], key, id, kind, m, data)
}

type componentCommandCounter struct {
	reads       atomic.Int64
	reserves    atomic.Int64
	reserveKeys atomic.Int64
	commits     atomic.Int64
}

func (h *componentCommandCounter) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	args := cmd.Args()
	if len(args) > 2 && (cmd.Name() == "evalsha" || cmd.Name() == "eval") {
		if fmt.Sprint(args[1]) == componentReadScript.Hash() {
			h.reads.Add(1)
		}
		if fmt.Sprint(args[1]) == componentCommitScript.Hash() {
			h.commits.Add(1)
		}
		argv := sessionCommandArgs(cmd)
		if len(argv) > 0 && argv[0] == "sequence" {
			h.reserves.Add(1)
			n, _ := strconv.ParseInt(fmt.Sprint(args[2]), 10, 64)
			h.reserveKeys.Store(n)
		}
	}
	return ctx, nil
}
func (*componentCommandCounter) AfterProcess(context.Context, redis.Cmder) error { return nil }
func (*componentCommandCounter) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (*componentCommandCounter) AfterProcessPipeline(context.Context, []redis.Cmder) error {
	return nil
}
func TestComponentHotCommitUsesReservationAndOneWrite(t *testing.T) {
	forComponentRedis(t, func(t *testing.T, r *Redis) {
		ctx := context.Background()
		lease, err := r.Claim(ctx, "hot-write", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		first, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
		require.NoError(t, err)
		r.maintenance.Wait()
		counter := &componentCommandCounter{}
		r.client.AddHook(counter)
		_, err = r.CommitComponents(ctx, lease, ComponentCommit{Components: first.Components, StateVersion: 1, Expected: &first.Metadata})
		require.NoError(t, err)
		require.Zero(t, counter.reads.Load(), "ordinary hot commits must not download unchanged setup/history")
		require.EqualValues(t, 1, counter.reserves.Load())
		require.EqualValues(t, 1, counter.reserveKeys.Load(), "sequence reservation declares only the lease key")
		require.EqualValues(t, 1, counter.commits.Load())
	})
}

func TestComponentInitialPreparationExpiresAndFailsClosed(t *testing.T) {
	forComponentRedis(t, func(t *testing.T, r *Redis) {
		ctx := context.Background()
		lease, err := r.Claim(ctx, "fault", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		hook := &componentFaultHook{r: r, mode: "after-bank"}
		hook.armed.Store(true)
		r.maintenance.Wait()
		r.client.AddHook(hook)
		_, err = r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
		require.ErrorContains(t, err, "injected-before-publish")
		for _, key := range r.componentKeys(lease.SessionID)[5:] {
			require.Positive(t, r.client.PTTL(ctx, key).Val(), "all unpublished initial preparations must expire")
		}
		_, err = r.ReadComponents(ctx, lease.SessionID)
		require.ErrorIs(t, err, ErrUnsafeCheckpoint, "an unpublished tuple has no empty/legacy fallback")
		require.NoError(t, r.Release(ctx, lease))
		for _, key := range r.componentKeys(lease.SessionID)[5:] {
			require.Zero(t, r.client.Exists(ctx, key).Val())
		}
	})
}
