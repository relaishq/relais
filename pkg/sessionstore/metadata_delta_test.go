package sessionstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRedisMetadataDeltaReconstructsExactBytes(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	decode, _, ok := strings.Cut(componentCommitLua, "-- Preflight all key types")
	require.True(t, ok)
	decode += "return m"
	for _, owner := range []string{"a", "127.0.0.1:23456", strings.Repeat("a", 64)} {
		m := ComponentMetadata{Format: 10, Owner: owner, OwnerEpoch: 1, StateVersion: 1, SetupGeneration: 1}
		for i := range m.References {
			m.References[i].Revision = 1
		}
		expected := mustPackMetadata(t, m)
		for mask := 0; mask < 32; mask++ {
			next := m
			for i := range next.References {
				if mask&(1<<i) != 0 {
					next.References[i].Digest[0] ^= 1
				}
			}
			for i := 0; i < 3; i++ {
				if mask&(1<<i) != 0 {
					next.Banks[i] = 1
				}
			}
			packed := mustPackMetadata(t, next)
			encoded := encodeMetadataDelta(expected, packed)
			require.LessOrEqual(t, len(encoded), 20+209)
			result, err := r.client.Eval(ctx, decode, nil, expected, encoded, "", "", "", "").Text()
			require.NoError(t, err)
			require.Equal(t, string(packed), result, "owner %q mask %d", owner, mask)
		}
		for _, kind := range []string{"initialization", "owner", "epoch", "app-version", "setup-generation"} {
			next := m
			base := expected
			switch kind {
			case "initialization":
				base = nil
			case "owner":
				next.Owner = "new owner"
			case "epoch":
				next.OwnerEpoch++
			case "app-version":
				next.StateVersion++
			case "setup-generation":
				next.SetupGeneration++
			}
			packed := mustPackMetadata(t, next)
			encoded := encodeMetadataDelta(base, packed)
			require.Equal(t, byte(10), encoded[20], kind)
			result, err := r.client.Eval(ctx, decode, nil, base, encoded, "", "", "", "").Text()
			require.NoError(t, err)
			require.Equal(t, string(packed), result)
		}
	}
}

func TestRedisMetadataDeltaRejectsBeforePublication(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	lease, err := r.Claim(ctx, "delta-preflight", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
	require.NoError(t, err)
	first, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
	require.NoError(t, err)
	record, _, _, err := r.readComponentRecord(ctx, lease.SessionID)
	require.NoError(t, err)
	reserved, err := r.run(ctx, "sequence", lease.SessionID, lease.Worker.String(), strconv.FormatUint(lease.Epoch, 10), 0)
	require.NoError(t, err)
	seq, err := sequenceNumber(reserved[1].(string))
	require.NoError(t, err)
	c := cloneComponents(first.Components)
	c.Transport = advanceComponentTransport(t, c.Transport)
	prepared, err := r.prepareComponents(lease, ComponentCommit{Components: c, StateVersion: 1, Expected: &first.Metadata}, &record, seq)
	require.NoError(t, err)
	expected, packed := mustPackMetadata(t, first.Metadata), mustPackMetadata(t, prepared.record.metadata)
	encoded := encodeMetadataDelta(expected, packed)
	require.Equal(t, byte(0x90), encoded[20], "host-only delta replaces checkpoint reference")
	for _, kind := range []string{"truncated", "trailing", "tag", "base", "corrupt-reference", "corrupt-binding", "stale-CAS", "old-argument-count"} {
		t.Run(kind, func(t *testing.T) {
			base, delta := bytes.Clone(expected), bytes.Clone(encoded)
			want := ErrUnsafeCheckpoint
			switch kind {
			case "truncated":
				delta = delta[:len(delta)-1]
			case "trailing":
				delta = append(delta, 0)
			case "tag":
				delta[20] |= 0x40
			case "base":
				base = nil
			case "corrupt-reference":
				delta[21] ^= 1 // Same length and valid metadata; only the binding detects this before HSET.
			case "corrupt-binding":
				delta[0] ^= 1
			case "stale-CAS":
				base[165] ^= 1
				want = ErrStateSuperseded // Replaced by the delta, so checksum still matches.
			}
			args := []interface{}{base, delta, prepared.record.sealed.Transport, []byte{}, []byte{}, []byte{}}
			if kind == "old-argument-count" {
				args = append(args, "", "", "", "", "")
			}
			before := snapshotComponentKeys(t, r, lease.SessionID)
			_, err = r.runComponent(ctx, componentCommitScript, "component_commit", lease.SessionID, args...)
			require.ErrorIs(t, err, want)
			require.Equal(t, before, snapshotComponentKeys(t, r, lease.SessionID))
			read, e := r.ReadComponents(ctx, lease.SessionID)
			require.NoError(t, e)
			require.Equal(t, first.Metadata, read.Metadata)
		})
	}
}

func TestRedisComponentDerivedIntegersRemainExact(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	lease, err := r.Claim(ctx, "derived-integers", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
	require.NoError(t, err)
	lease.Epoch = ^uint64(0)
	const previous uint64 = 1<<53 + 122
	require.NoError(t, r.client.HSet(ctx, r.keys(lease.SessionID)[0], "epoch", strconv.FormatUint(lease.Epoch, 10), "next_seq", strconv.FormatUint(previous, 10)).Err())
	first, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
	require.NoError(t, err)
	require.Equal(t, previous+1, first.Metadata.References[4].Revision)
	require.Equal(t, binary.BigEndian.AppendUint64(nil, lease.Epoch), mustPackMetadata(t, first.Metadata)[206:214])
	c := cloneComponents(first.Components)
	c.Transport = advanceComponentTransport(t, c.Transport)
	second, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 1, Expected: &first.Metadata})
	require.NoError(t, err)
	require.Equal(t, previous+2, second.Metadata.References[4].Revision)
	read, err := r.ReadComponents(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, second.Metadata, read.Metadata)
}
