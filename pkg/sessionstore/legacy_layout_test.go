package sessionstore

import (
	"context"
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The fixture is the unmodified sessionLua from the local v0.2.0 tag.
func TestLegacyLayoutMarkerCompatibleWithV02Script(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	lease, err := r.Claim(ctx, "legacy-layout", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
	require.NoError(t, err)
	state := []byte("unchanged legacy ciphertext plaintext")
	require.NoError(t, r.PutState(ctx, lease, state))
	require.Equal(t, "legacy", r.client.HGet(ctx, r.keys(lease.SessionID)[0], "layout").Val())
	lua, err := os.ReadFile("testdata/v0.2-session.lua")
	require.NoError(t, err)
	run := func(op string) []interface{} {
		result, err := r.client.Eval(ctx, string(lua), r.keys(lease.SessionID), op, lease.Worker.String(), strconv.FormatUint(lease.Epoch, 10), 120000, 300000).Slice()
		require.NoError(t, err)
		require.EqualValues(t, 1, result[0])
		return result
	}
	got := run("get")
	require.Equal(t, lease.Worker.String(), got[1])
	before := run("state")
	renewed, err := decodeLease(lease.SessionID, run("renew"))
	require.NoError(t, err)
	require.True(t, renewed.ExpiresAt.After(lease.ExpiresAt))
	require.Equal(t, before, run("state"), "old renew preserves exact ciphertext and sequence")
	require.Equal(t, "legacy", r.client.HGet(ctx, r.keys(lease.SessionID)[0], "layout").Val())
	restored, err := r.GetState(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, state, restored)
}
