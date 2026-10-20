package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func TestStoreStallReaderUsesWorkerKeyRing(t *testing.T) {
	addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
	if addr == "" {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			t.Fatal("dedicated Redis required")
		}
		t.Skip("dedicated Redis required")
	}
	require.NoError(t, redisendpoint.ValidateDev(addr))
	ctx := context.Background()
	key, old := make([]byte, 32), make([]byte, 32)
	key[0] = 1
	old[0] = 2
	cfg := storage.RedisConfig{Addr: addr, Prefix: fmt.Sprintf("stall-key-ring:%d:", time.Now().UnixNano())}
	writer, err := sessionstore.NewRedis(ctx, cfg, old, sessionstore.RedisOptions{KeyID: 6})
	require.NoError(t, err)
	defer writer.Close()
	lease, err := writer.Claim(ctx, "old-ring", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
	require.NoError(t, err)
	require.NoError(t, writer.PutState(ctx, lease, []byte("old-key snapshot")))
	defer func() { require.NoError(t, writer.Release(ctx, lease)) }()
	env := []string{"RELAIS_REDIS_ADDR=" + addr, "RELAIS_REDIS_PREFIX=" + cfg.Prefix, "RELAIS_SESSIONSTORE_KEY=" + base64.StdEncoding.EncodeToString(key), "RELAIS_SESSIONSTORE_KEY_ID=7", `RELAIS_SESSIONSTORE_OLD_KEYS={"6":"` + hex.EncodeToString(old) + `"}`}
	reader, err := openStoreStallStore(ctx, env)
	require.NoError(t, err)
	defer reader.Close()
	data, err := reader.GetState(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, []byte("old-key snapshot"), data)
	require.NoError(t, reader.PutState(ctx, lease, []byte("active-key snapshot")))
	current, err := sessionstore.NewRedis(ctx, cfg, key, sessionstore.RedisOptions{KeyID: 7})
	require.NoError(t, err)
	defer current.Close()
	data, err = current.GetState(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, []byte("active-key snapshot"), data)
}
