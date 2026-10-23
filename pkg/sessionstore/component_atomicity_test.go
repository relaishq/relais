package sessionstore

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	codec "github.com/relaishq/relais/internal/componentcodec"
	"github.com/stretchr/testify/require"
)

func TestComponentTupleReadRejectsLayoutAndFutureOwner(t *testing.T) {
	forComponentRedis(t, func(t *testing.T, r *Redis) {
		ctx := context.Background()
		lease, err := r.Claim(ctx, "tuple-owner", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		first, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
		require.NoError(t, err)
		require.NoError(t, r.client.HSet(ctx, r.keys(lease.SessionID)[0], "layout", "legacy").Err())
		_, err = r.ReadComponents(ctx, lease.SessionID)
		require.ErrorIs(t, err, ErrCheckpointLayout)
		require.NoError(t, r.client.HSet(ctx, r.keys(lease.SessionID)[0], "layout", "10").Err())
		// Authenticated future-owner bytes must still fail the client/lease check.
		m := first.Metadata
		m.OwnerEpoch++
		digest, err := codec.WholeDigest(r.keysByID[r.activeKey], lease.SessionID, m, first.Components.Transport)
		require.NoError(t, err)
		m.References[4].Digest = digest
		transport, err := codec.Seal(r.keysByID[r.activeKey], r.activeKey, lease.SessionID, codec.Transport, m, first.Components.Transport)
		require.NoError(t, err)
		packed, err := m.Marshal()
		require.NoError(t, err)
		require.NoError(t, r.client.HSet(ctx, r.componentKeys(lease.SessionID)[6], "m", packed, "b", transport).Err())
		_, err = r.ReadComponents(ctx, lease.SessionID)
		require.ErrorIs(t, err, ErrUnsafeCheckpoint)
	})
}

func interruptComponentInitialization(t *testing.T, r *Redis, lease Lease) Components {
	t.Helper()
	ctx := context.Background()
	reservation, err := r.run(ctx, "sequence", lease.SessionID, lease.Worker.String(), fmt.Sprint(lease.Epoch), 0)
	require.NoError(t, err)
	seq, err := sequenceNumber(fmt.Sprint(reservation[1]))
	require.NoError(t, err)
	c := componentFixture(lease.SessionID)
	p, err := r.prepareComponents(lease, ComponentCommit{Components: c, StateVersion: 1}, nil, seq)
	require.NoError(t, err)
	packed, err := p.record.metadata.Marshal()
	require.NoError(t, err)
	script := strings.Replace(componentCommitLua, "for i=0,2 do\n local part=ARGV[8+i]\n if #part>0 then", "error('initialization interrupted')\nfor i=0,2 do\n local part=ARGV[8+i]\n if #part>0 then", 1)
	_, err = r.client.Eval(ctx, script, r.componentKeys(lease.SessionID), lease.Worker.String(), fmt.Sprint(lease.Epoch), "", packed, p.record.sealed.Transport, fmt.Sprint(seq), binary.BigEndian.AppendUint64(nil, lease.Epoch), p.record.sealed.Setup, p.record.sealed.Work, p.record.sealed.History, binary.BigEndian.AppendUint64(nil, seq)).Result()
	require.ErrorContains(t, err, "initialization interrupted")
	require.Equal(t, "10", r.client.HGet(ctx, r.keys(lease.SessionID)[0], "layout").Val())
	return c
}

func TestComponentUnpublishedInitializationCanRetry(t *testing.T) {
	forComponentRedis(t, func(t *testing.T, r *Redis) {
		ctx := context.Background()
		lease, err := r.Claim(ctx, "init-retry", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		c := interruptComponentInitialization(t, r, lease)
		_, err = r.ReadComponents(ctx, lease.SessionID)
		require.ErrorIs(t, err, ErrUnsafeCheckpoint, "unpublished preparation is never a recovery value")
		retry, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 1})
		require.NoError(t, err)
		require.Equal(t, c, retry.Components)
		read, err := r.ReadComponents(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, retry.Metadata, read.Metadata)
	})
}

// Force the first command to land AFTER its settlement read, but BEFORE the
// retry. The retry's -4 must settle against this call's first prepared metadata.
type lateComponentCommit struct {
	r         *Redis
	id        string
	phase     atomic.Int32
	args      []interface{}
	loseRetry bool
}

func (h *lateComponentCommit) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	if cmd.Name() == "evalsha" && fmt.Sprint(cmd.Args()[1]) == componentCommitScript.Hash() {
		if h.phase.CompareAndSwap(0, 1) {
			h.args = append([]interface{}(nil), sessionCommandArgs(cmd)...)
			return ctx, io.EOF
		}
		if h.loseRetry && h.phase.CompareAndSwap(2, 3) {
			return ctx, io.EOF
		}
	}
	return ctx, nil
}
func (h *lateComponentCommit) AfterProcess(ctx context.Context, cmd redis.Cmder) error {
	if ((cmd.Name() == "evalsha" && fmt.Sprint(cmd.Args()[1]) == componentReadScript.Hash()) || (cmd.Name() == "eval" && fmt.Sprint(cmd.Args()[1]) == componentReadLua)) && cmd.Err() == nil && h.phase.CompareAndSwap(1, 2) {
		// Use EVAL so the hook cannot recursively intercept itself.
		_, err := h.r.client.Eval(ctx, componentCommitLua, h.r.componentKeys(h.id), h.args...).Result()
		return err
	}
	return nil
}
func (*lateComponentCommit) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (*lateComponentCommit) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }
func TestComponentLateOwnPublicationSettlesSupersededRetry(t *testing.T) {
	forComponentRedis(t, func(t *testing.T, r *Redis) {
		ctx := context.Background()
		lease, err := r.Claim(ctx, "late-own", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		first, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
		require.NoError(t, err)
		r.maintenance.Wait()
		h := &lateComponentCommit{r: r, id: lease.SessionID}
		r.client.AddHook(redisFaultHook{h})
		changed := cloneComponents(first.Components)
		changed.History = appendComponentBytes(t, changed.History, codec.History, 1)
		result, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: changed, StateVersion: 1, Expected: &first.Metadata})
		require.NoError(t, err)
		require.EqualValues(t, 2, h.phase.Load())
		require.Positive(t, result.WrittenBytes)
		require.Equal(t, changed, result.Components)
		read, err := r.ReadComponents(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, result.Metadata, read.Metadata)
		require.EqualValues(t, first.Metadata.References[4].Revision+1, result.Metadata.References[4].Revision, "success identifies the FIRST publication, not a later debit/write")
	})
}

func TestComponentEarlierPublicationSettlesAfterTwoUnknownAttempts(t *testing.T) {
	forComponentRedis(t, func(t *testing.T, r *Redis) {
		ctx := context.Background()
		lease, err := r.Claim(ctx, "two-unknown", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		first, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
		require.NoError(t, err)
		r.maintenance.Wait()
		hook := &lateComponentCommit{r: r, id: lease.SessionID, loseRetry: true}
		r.client.AddHook(redisFaultHook{hook})
		changed := cloneComponents(first.Components)
		changed.History = appendComponentBytes(t, changed.History, codec.History, 1)
		result, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: changed, StateVersion: 1, Expected: &first.Metadata})
		require.NoError(t, err, "second settlement must recognize the first uncertain attempt")
		require.EqualValues(t, 3, hook.phase.Load())
		read, err := r.ReadComponents(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, changed, read.Components)
		require.Equal(t, read.Metadata, result.Metadata)
		require.EqualValues(t, first.Metadata.References[4].Revision+1, result.Metadata.References[4].Revision)
		require.Positive(t, result.WrittenBytes, "settlement returns the first publication's charge")
	})
}
