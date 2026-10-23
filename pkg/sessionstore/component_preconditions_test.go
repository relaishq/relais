package sessionstore

import (
	"context"
	"crypto/sha1"
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
		cases := []string{"wrong-type", "referenced-bank", "sequence-metadata-checksum", "setup-generation", "oversized-part", "wrong-owner", "wrong-epoch", "expired", "transferred", "stale-sequence", "future-sequence", "legacy-layout", "owner-metadata-checksum", "epoch-encoding-checksum", "sequence-encoding-checksum", "unchanged-part", "unchanged-bank", "changed-revision", "unchanged-root", "changed-root", "reinitialize", "reinitialize-missing-checkpoint"}
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
				case "sequence-metadata-checksum":
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
				case "owner-metadata-checksum":
					p.record.metadata.Owner = "127.0.0.1:23457"
				case "epoch-encoding-checksum":
					epochBytes = binary.BigEndian.AppendUint64(nil, lease.Epoch+1)
					p.record.metadata.OwnerEpoch++
				case "sequence-encoding-checksum":
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
				_, err = r.runComponent(ctx, componentCommitScript, "component_commit", id, componentCommitFixtureArgs(t, owner, epoch, expected, packed, p.record.sealed.Transport, fmt.Sprint(seq), epochBytes, parts[0], parts[1], parts[2], seqBytes)...)
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
				_, err = r.runComponent(ctx, componentCommitScript, "component_commit", id, componentCommitFixtureArgs(t, lease.Worker.String(), fmt.Sprint(lease.Epoch), "", packed, prepared.record.sealed.Transport, fmt.Sprint(seq), binary.BigEndian.AppendUint64(nil, lease.Epoch), parts[0], parts[1], parts[2], binary.BigEndian.AppendUint64(nil, seq))...)
				require.ErrorIs(t, err, ErrUnsafeCheckpoint)
				require.Equal(t, before, snapshotComponentKeys(t, r, id), "initialization refusal must not recreate or overwrite any bank")
			})
		}
	})
}

// These requests bypass the eleven-field fixture adapter: metadata, checksum,
// ciphertext and CAS expectation are valid, leaving only the lease identity wrong.
func TestComponentMetadataIdentityFencedBeforeWriting(t *testing.T) {
	for _, kind := range []string{"foreign-owner-valid-checksum", "stale-epoch-valid-checksum"} {
		t.Run(kind, func(t *testing.T) {
			r := testRedis(t)
			ctx := context.Background()
			owner := netip.MustParseAddrPort("127.0.0.1:23456")
			lease, err := r.Claim(ctx, kind, owner, time.Minute)
			require.NoError(t, err)
			first, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(kind), StateVersion: 1})
			require.NoError(t, err)
			current, err := r.Transfer(ctx, lease, netip.MustParseAddrPort("127.0.0.1:23457"), time.Minute)
			require.NoError(t, err)
			require.Greater(t, current.Epoch, lease.Epoch)
			r.maintenance.Wait()
			record, _, _, err := r.readComponentRecord(ctx, kind)
			require.NoError(t, err)
			reserved, err := r.run(ctx, "sequence", kind, current.Worker.String(), strconv.FormatUint(current.Epoch, 10), 0)
			require.NoError(t, err)
			seq, err := sequenceNumber(reserved[1].(string))
			require.NoError(t, err)
			writer := current
			if kind == "foreign-owner-valid-checksum" {
				writer.Worker = netip.MustParseAddrPort("127.0.0.1:23458")
				require.Equal(t, current.Epoch, writer.Epoch)
				require.NotEqual(t, current.Worker, writer.Worker)
			} else {
				writer.Epoch = lease.Epoch
				require.Equal(t, current.Worker, writer.Worker)
				require.Less(t, writer.Epoch, current.Epoch)
			}
			c := cloneComponents(first.Components)
			c.History = appendComponentBytes(t, c.History, 4, 1)
			prepared, err := r.prepareComponents(writer, ComponentCommit{Components: c, StateVersion: 1, Expected: &first.Metadata}, &record, seq)
			require.NoError(t, err)
			require.Equal(t, writer.Worker.String(), prepared.record.metadata.Owner)
			require.Equal(t, writer.Epoch, prepared.record.metadata.OwnerEpoch)
			// Prove that metadata and ciphertext agree, independently of the lease fence.
			opened, err := r.openRecord(kind, prepared.record)
			require.NoError(t, err)
			require.Equal(t, c, opened)
			expected, packed := mustPackMetadata(t, first.Metadata), mustPackMetadata(t, prepared.record.metadata)
			encoded := encodeMetadataDelta(expected, packed)
			checksum := sha1.Sum(packed)
			require.Equal(t, checksum[:], encoded[:sha1.Size])
			parts := componentBytes(prepared.record.sealed)
			for i := range parts {
				if !prepared.changed[i] {
					parts[i] = nil
				}
			}
			before := snapshotComponentKeys(t, r, kind)
			_, err = r.runComponent(ctx, componentCommitScript, "component_commit", kind, expected, encoded, prepared.record.sealed.Transport, parts[0], parts[1], parts[2])
			require.ErrorIs(t, err, ErrLeaseLost)
			require.Equal(t, before, snapshotComponentKeys(t, r, kind), "identity refusal precedes every write")
			restored, err := r.ReadComponents(ctx, kind)
			require.NoError(t, err)
			require.Equal(t, first.Metadata, restored.Metadata)
			require.Equal(t, first.Components, restored.Components)
		})
	}
}
