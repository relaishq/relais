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
		cases := []string{"wrong-type", "referenced-bank", "wrong-sequence", "setup-generation", "oversized-part", "wrong-owner", "wrong-epoch", "expired", "transferred", "stale-sequence", "future-sequence", "legacy-layout", "owner-metadata", "epoch-encoding", "sequence-encoding", "unchanged-part", "unchanged-bank", "changed-revision", "unchanged-root", "changed-root", "reinitialize", "reinitialize-missing-checkpoint"}
		for _, kind := range cases {
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
				if kind != "unchanged-root" {
					c.History = appendComponentBytes(t, c.History, 4, 1)
				}
				p, err := r.prepareComponents(lease, ComponentCommit{Components: c, StateVersion: 1, Expected: &first.Metadata}, &record, seq)
				require.NoError(t, err)
				expected, _ := first.Metadata.Marshal()
				owner, epoch := lease.Worker.String(), fmt.Sprint(lease.Epoch)
				epochBytes, seqBytes := binary.BigEndian.AppendUint64(nil, lease.Epoch), binary.BigEndian.AppendUint64(nil, seq)
				parts := componentBytes(p.record.sealed)
				for i := range parts {
					if !p.changed[i] {
						parts[i] = nil
					}
				}
				want := ErrUnsafeCheckpoint
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
					parts[2] = make([]byte, 131103)
				case "wrong-owner":
					owner = "127.0.0.1:23457"
					p.record.metadata.Owner = owner
					want = ErrLeaseLost
				case "wrong-epoch":
					epoch = fmt.Sprint(lease.Epoch + 1)
					epochBytes = binary.BigEndian.AppendUint64(nil, lease.Epoch+1)
					p.record.metadata.OwnerEpoch++
					want = ErrLeaseLost
				case "expired":
					require.NoError(t, r.client.HSet(ctx, r.keys(id)[0], "expires", "1").Err())
					want = ErrLeaseLost
				case "transferred":
					_, err = r.Transfer(ctx, lease, netip.MustParseAddrPort("127.0.0.1:23457"), time.Minute)
					require.NoError(t, err)
					want = ErrLeaseLost
				case "stale-sequence":
					require.NoError(t, r.client.HSet(ctx, r.keys(id)[0], "state_seq", fmt.Sprint(seq)).Err())
					want = ErrStateSuperseded
				case "future-sequence":
					require.NoError(t, r.client.HSet(ctx, r.keys(id)[0], "next_seq", fmt.Sprint(seq-1)).Err())
					want = ErrStateSuperseded
				case "legacy-layout":
					require.NoError(t, r.client.HSet(ctx, r.keys(id)[0], "layout", "legacy").Err())
					want = ErrCheckpointLayout
				case "owner-metadata":
					p.record.metadata.Owner = "127.0.0.1:23457"
				case "epoch-encoding":
					epochBytes = binary.BigEndian.AppendUint64(nil, lease.Epoch+1)
					p.record.metadata.OwnerEpoch++
				case "sequence-encoding":
					seqBytes = binary.BigEndian.AppendUint64(nil, seq+1)
					p.record.metadata.References[2].Revision++
					p.record.metadata.References[3].Revision++
					p.record.metadata.References[4].Revision++
				case "unchanged-part":
					parts[1] = record.sealed.Work
				case "unchanged-bank":
					p.record.metadata.Banks[1] = 1 - first.Metadata.Banks[1]
				case "changed-revision":
					p.record.metadata.References[2].Revision = first.Metadata.References[2].Revision
				case "unchanged-root":
					p.record.metadata.References[3].Digest[0] ^= 1
				case "changed-root":
					p.record.metadata.References[3].Revision = first.Metadata.References[3].Revision
				case "reinitialize", "reinitialize-missing-checkpoint":
					p, err = r.prepareComponents(lease, ComponentCommit{Components: c, StateVersion: 1}, nil, seq)
					require.NoError(t, err)
					parts = componentBytes(p.record.sealed)
					expected = nil
					require.NoError(t, r.client.HSet(ctx, r.componentKeys(id)[6], "m", "").Err())
					if kind == "reinitialize-missing-checkpoint" {
						require.NoError(t, r.client.Del(ctx, r.componentKeys(id)[6]).Err())
					}
				}
				packed, err := p.record.metadata.Marshal()
				require.NoError(t, err)
				before := snapshotComponentKeys(t, r, id)
				_, err = r.runComponent(ctx, componentCommitScript, "component_commit", id, owner, epoch, expected, packed, p.record.sealed.Transport, fmt.Sprint(seq), epochBytes, parts[0], parts[1], parts[2], seqBytes)
				require.ErrorIs(t, err, want)
				require.Equal(t, before, snapshotComponentKeys(t, r, id), "every precondition precedes all writes")
			})
		}
		for _, kind := range []string{"init-other-tenure", "init-missing-bank", "init-generation"} {
			t.Run(kind, func(t *testing.T) {
				id := "preflight-" + kind
				lease, err := r.Claim(ctx, id, netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
				require.NoError(t, err)
				components := componentFixture(id)
				if kind != "init-generation" {
					components = interruptComponentInitialization(t, r, lease)
				}
				if kind == "init-other-tenure" {
					lease, err = r.Transfer(ctx, lease, netip.MustParseAddrPort("127.0.0.1:23457"), time.Minute)
					require.NoError(t, err)
				}
				if kind == "init-missing-bank" {
					require.NoError(t, r.client.Del(ctx, r.componentKeys(id)[10]).Err())
				}
				reservation, err := r.run(ctx, "sequence", id, lease.Worker.String(), fmt.Sprint(lease.Epoch), 0)
				require.NoError(t, err)
				seq, err := sequenceNumber(fmt.Sprint(reservation[1]))
				require.NoError(t, err)
				prepared, err := r.prepareComponents(lease, ComponentCommit{Components: components, StateVersion: 1}, nil, seq)
				require.NoError(t, err)
				if kind == "init-generation" {
					// Raw EVAL bypasses the client's initial-generation assignment.
					prepared.record.metadata.SetupGeneration++
				}
				packed, err := prepared.record.metadata.Marshal()
				require.NoError(t, err)
				parts := componentBytes(prepared.record.sealed)
				before := snapshotComponentKeys(t, r, id)
				_, err = r.runComponent(ctx, componentCommitScript, "component_commit", id, lease.Worker.String(), fmt.Sprint(lease.Epoch), "", packed, prepared.record.sealed.Transport, fmt.Sprint(seq), binary.BigEndian.AppendUint64(nil, lease.Epoch), parts[0], parts[1], parts[2], binary.BigEndian.AppendUint64(nil, seq))
				require.ErrorIs(t, err, ErrUnsafeCheckpoint)
				require.Equal(t, before, snapshotComponentKeys(t, r, id), "initialization refusal must not recreate or overwrite any bank")
			})
		}
	})
}
