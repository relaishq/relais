package sessionstore

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestComponentRevisionsRemainExactAboveLuaIntegerRange(t *testing.T) {
	forComponentStores(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cs := s.(ComponentStore)
		lease, err := s.Claim(ctx, "large-revision", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		const previous uint64 = 1<<53 + 123
		if m, ok := s.(*Memory); ok {
			m.componentSequence = previous
		} else {
			r := s.(*Redis)
			require.NoError(t, r.client.HSet(ctx, r.keys(lease.SessionID)[0], "next_seq", strconv.FormatUint(previous, 10)).Err())
		}
		first, err := cs.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
		require.NoError(t, err)
		require.Equal(t, previous+1, first.Metadata.References[4].Revision)
		c := cloneComponents(first.Components)
		c.History = appendComponentBytes(t, c.History, 4, 1)
		second, err := cs.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 1, Expected: &first.Metadata})
		require.NoError(t, err)
		require.Equal(t, previous+2, second.Metadata.References[4].Revision)
		restored, err := cs.ReadComponents(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, second.Metadata, restored.Metadata)
	})
}
func TestComponentScriptPreflightRefusesBeforeWriting(t *testing.T) {
	forComponentRedis(t, func(t *testing.T, r *Redis) {
		ctx := context.Background()
		for _, kind := range []string{"wrong-type", "referenced-bank", "wrong-sequence", "setup-generation", "oversized-part"} {
			t.Run(kind, func(t *testing.T) {
				id := "preflight-" + kind
				lease, err := r.Claim(ctx, id, netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
				require.NoError(t, err)
				first, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(id), StateVersion: 1})
				require.NoError(t, err)
				record, _, _, err := r.readComponentRecord(ctx, id)
				require.NoError(t, err)
				reservation, err := r.run(ctx, "sequence", id, lease.Worker.String(), fmt.Sprint(lease.Epoch), 0)
				require.NoError(t, err)
				seq, err := sequenceNumber(fmt.Sprint(reservation[1]))
				require.NoError(t, err)
				c := cloneComponents(first.Components)
				c.History = appendComponentBytes(t, c.History, 4, 1)
				p, err := r.prepareComponents(lease, ComponentCommit{Components: c, StateVersion: 1, Expected: &first.Metadata}, &record, seq)
				require.NoError(t, err)
				switch kind {
				case "wrong-type":
					key := r.componentKeys(id)[7]
					require.NoError(t, r.client.Del(ctx, key).Err())
					require.NoError(t, r.client.HSet(ctx, key, "corrupt", "type").Err())
				case "referenced-bank":
					p.record.metadata.Banks[2] = first.Metadata.Banks[2]
				case "wrong-sequence":
					p.record.metadata.References[4].Revision++
				case "setup-generation":
					p.record.metadata.SetupGeneration++
				case "oversized-part":
					p.record.sealed.History = make([]byte, 131103)
				}
				packed, _ := p.record.metadata.Marshal()
				expected, _ := first.Metadata.Marshal()
				before := snapshotComponentKeys(t, r, id)
				_, err = r.runComponent(ctx, componentCommitScript, "component_commit", id, lease.Worker.String(), fmt.Sprint(lease.Epoch), expected, packed, p.record.sealed.Transport, fmt.Sprint(seq), binary.BigEndian.AppendUint64(nil, lease.Epoch), []byte{}, []byte{}, p.record.sealed.History, binary.BigEndian.AppendUint64(nil, seq))
				require.ErrorIs(t, err, ErrUnsafeCheckpoint)
				require.Equal(t, before, snapshotComponentKeys(t, r, id), "all preconditions precede component or metadata writes")
				require.NoError(t, r.Release(ctx, lease))
			})
		}
	})
}
