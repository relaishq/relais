package sessionstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	codec "github.com/relaishq/relais/internal/componentcodec"
	"github.com/relaishq/relais/internal/redistestenv"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func forStores(t *testing.T, test func(*testing.T, func(*testing.T) Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { test(t, func(*testing.T) Store { return NewMemory() }) })
	t.Run("redis", func(t *testing.T) { test(t, func(t *testing.T) Store { return testRedis(t) }) })
}
func testRedis(t *testing.T, options ...RedisOptions) *Redis {
	t.Helper()
	addr := redistestenv.Address(t)
	if addr == "" {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			t.Fatal("required Redis endpoint is not configured")
		}
		t.Skip("Redis integration requires RELAIS_TEST_REDIS_ADDR")
	}
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			t.Fatalf("required Redis endpoint unavailable: %v", err)
		}
		t.Skipf("Redis test endpoint unavailable: %v", err)
	}
	require.NoError(t, conn.Close())
	token := make([]byte, 16)
	_, err = rand.Read(token)
	require.NoError(t, err)
	r, err := NewRedis(context.Background(), storage.RedisConfig{Addr: addr, Prefix: "session-test:" + hex.EncodeToString(token) + ":"}, make([]byte, 32), options...)
	require.NoError(t, err)
	t.Cleanup(func() {
		r.maintenance.Wait()
		ctx := context.Background()
		var cursor uint64
		for {
			keys, next, err := r.client.Scan(ctx, cursor, r.prefix+"*", 100).Result()
			require.NoError(t, err)
			for _, key := range keys {
				require.NoError(t, r.client.Del(ctx, key).Err())
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
		require.NoError(t, r.Close())
	})
	return r
}

// sessionCommandArgs returns ARGV without assuming how many per-session keys
// the production script declares. Fault injection must keep reaching the real
// transition/snapshot seam as keys are added to the atomic session lifecycle.
func sessionCommandArgs(cmd redis.Cmder) []interface{} {
	args := cmd.Args()
	if (cmd.Name() != "eval" && cmd.Name() != "evalsha") || len(args) < 3 {
		return nil
	}
	n, err := strconv.Atoi(fmt.Sprint(args[2]))
	if err != nil || n < 0 || len(args) < 3+n {
		return nil
	}
	return args[3+n:]
}

// componentCommitFixtureArgs keeps the former eleven-field fixture notation in
// tests only. The production wire contract always has six arguments. Former
// duplicate-field inconsistencies deliberately corrupt only the checksum and
// test checksum rejection, not the lease identity fence. Fence tests must send
// six direct arguments with a valid checksum.
func componentCommitFixtureArgs(t *testing.T, old ...interface{}) []interface{} {
	t.Helper()
	require.Len(t, old, 11)
	asBytes := func(v interface{}) []byte {
		if b, ok := v.([]byte); ok {
			return b
		}
		return []byte(fmt.Sprint(v))
	}
	expected, m := asBytes(old[2]), asBytes(old[3])
	encoded := encodeMetadataDelta(expected, m)
	metadata, err := codec.DecodeMetadata(m)
	require.NoError(t, err)
	if fmt.Sprint(old[0]) != metadata.Owner || fmt.Sprint(old[1]) != strconv.FormatUint(metadata.OwnerEpoch, 10) || fmt.Sprint(old[5]) != strconv.FormatUint(metadata.References[4].Revision, 10) || !bytes.Equal(asBytes(old[6]), binary.BigEndian.AppendUint64(nil, metadata.OwnerEpoch)) || !bytes.Equal(asBytes(old[10]), binary.BigEndian.AppendUint64(nil, metadata.References[4].Revision)) {
		encoded[0] ^= 1
	}
	return []interface{}{expected, encoded, old[4], old[7], old[8], old[9]}
}
