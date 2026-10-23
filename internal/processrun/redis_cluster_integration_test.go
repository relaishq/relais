package processrun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/relaishq/relais/pkg/storage"
	"gopkg.in/yaml.v3"

	"github.com/relaishq/relais/internal/redistestcluster"
	"github.com/relaishq/relais/pkg/framecache"
	"github.com/stretchr/testify/require"
)

func TestClusterTLSACLIntegration(t *testing.T) {
	if testing.Short() || os.Getenv("RELAIS_TEST_REDIS_CLUSTER") != "1" {
		t.Skip("set RELAIS_TEST_REDIS_CLUSTER=1 for disposable TLS/ACL cluster")
	}
	ca, cert, key, _ := redisTLSCertificates(t, t.TempDir())
	username, password := "relais-"+rand.Text(), rand.Text()
	addrs := redistestcluster.StartWithOptions(t, redistestcluster.Options{CAFile: ca, CertFile: cert, KeyFile: key, Username: username, Password: password})
	for name, value := range map[string]string{
		"RELAIS_REDIS_ADDR": strings.Join(addrs, ","), "RELAIS_REDIS_CLUSTER": "1", "RELAIS_REDIS_TLS": "1",
		"RELAIS_REDIS_TLS_CA_FILE": ca, "RELAIS_REDIS_TLS_CERT_FILE": cert, "RELAIS_REDIS_TLS_KEY_FILE": key, "RELAIS_REDIS_TLS_SERVER_NAME": "redis.test",
		"RELAIS_REDIS_USERNAME": username, "RELAIS_REDIS_PASSWORD": password, "RELAIS_SESSIONSTORE_KEY": hex.EncodeToString(make([]byte, 32)),
	} {
		t.Setenv(name, value)
	}
	var cfg StoreConfig
	cfg.ConnectionFlags(flag.NewFlagSet("cluster", flag.ContinueOnError))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := cfg.Open(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	lease, err := store.Claim(ctx, "tls-call", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
	require.NoError(t, err)
	require.NoError(t, store.PutState(ctx, lease, []byte("encrypted snapshot")))
	state, err := store.GetState(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, []byte("encrypted snapshot"), state)
	owners, err := cfg.OpenOwners(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, owners.Close()) }()
	owner, err := owners.Owner(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, lease.Worker, owner)
	frames, err := cfg.OpenFrames(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, frames.Close()) }()
	track := framecache.Track{Kind: "video", SSRC: 42}
	for i := range 12 {
		id := fmt.Sprintf("tls-frame-%d", i)
		frame := framecache.Frame{Track: track, Timestamp: 100, Arrival: time.Now(), Keyframe: true, Packets: []framecache.Packet{{SequenceNumber: 1, Marker: true, Payload: []byte("frame")}}}
		require.NoError(t, frames.Append(ctx, id, frame))
		current, err := frames.Current(ctx, id, track)
		require.NoError(t, err)
		require.Len(t, current, 1)
		require.Equal(t, frame.Packets, current[0].Packets)
	}
	// Every discovered node uses the same verified TLS transport and ACL user.
	for _, mutate := range []func(*StoreConfig){func(c *StoreConfig) { c.Password = "wrong" }, func(c *StoreConfig) { c.Username = "wrong" }} {
		bad := cfg
		mutate(&bad)
		_, err := bad.OpenOwners(ctx)
		require.Error(t, err)
		require.NotContains(t, err.Error(), password)
	}
}

// Apply the shipped role ACLs unchanged on every TLS node. HELLO may be refused;
// v9 must fall back to AUTH without requiring CLIENT SETINFO permission.
func TestClusterRestrictedACLHandshake(t *testing.T) {
	if testing.Short() || os.Getenv("RELAIS_TEST_REDIS_CLUSTER") != "1" {
		t.Skip("owns TLS Cluster processes")
	}
	ca, cert, key, _ := redisTLSCertificates(t, t.TempDir())
	addrs := redistestcluster.StartWithOptions(t, redistestcluster.Options{CAFile: ca, CertFile: cert, KeyFile: key, Username: "fixture-admin", Password: "fixture-password"})
	for name, value := range map[string]string{
		"RELAIS_REDIS_TLS": "1", "RELAIS_REDIS_TLS_CA_FILE": ca, "RELAIS_REDIS_TLS_CERT_FILE": cert,
		"RELAIS_REDIS_TLS_KEY_FILE": key, "RELAIS_REDIS_TLS_SERVER_NAME": "redis.test",
	} {
		t.Setenv(name, value)
	}
	transport, err := redisendpoint.TLSFromEnv()
	require.NoError(t, err)
	config := storage.RedisConfig{Cluster: true, Addrs: addrs, Username: "fixture-admin", Password: "fixture-password", TLSConfig: transport, Prefix: "relais:preview:handshake:"}
	options, err := config.ConnectionOptions()
	require.NoError(t, err)
	admin := redis.NewClusterClient(storage.RedisClusterOptions(options))
	defer admin.Close()
	data, err := os.ReadFile("../../deploy/aws/preview.yaml")
	require.NoError(t, err)
	var template struct {
		Resources map[string]struct {
			Properties struct {
				AccessString string `yaml:"AccessString"`
			} `yaml:"Properties"`
		} `yaml:"Resources"`
	}
	require.NoError(t, yaml.Unmarshal(data, &template))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, role := range []string{"Worker", "Control", "Relay"} {
		t.Run(role, func(t *testing.T) {
			user := strings.ToLower(role) + "-handshake"
			password := "owned-test-password"
			access := template.Resources[role+"StoreUser"].Properties.AccessString
			require.NotEmpty(t, access)
			require.NoError(t, admin.ForEachShard(ctx, func(ctx context.Context, node *redis.Client) error {
				args := []interface{}{"ACL", "SETUSER", user, "reset", ">" + password}
				for _, token := range strings.Fields(access) {
					args = append(args, token)
				}
				if err := node.Do(ctx, args...).Err(); err != nil {
					return err
				}
				return node.Do(ctx, "ACL", "LOG", "RESET").Err()
			}))
			scoped := config
			scoped.Username = user
			scoped.Password = password
			opts, err := scoped.ConnectionOptions()
			require.NoError(t, err)
			client := redis.NewClusterClient(storage.RedisClusterOptions(opts))
			defer client.Close()
			require.NoError(t, client.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
				require.Equal(t, 2, node.Options().Protocol)
				require.True(t, node.Options().DisableIdentity)
				// Exercise a reconnect too; both handshakes use the same restricted ACL.
				if err := node.Ping(ctx).Err(); err != nil {
					return err
				}
				connection := node.Conn()
				defer connection.Close()
				return connection.Ping(ctx).Err()
			}))
			require.NoError(t, admin.ForEachShard(ctx, func(ctx context.Context, node *redis.Client) error {
				entries, err := node.Do(ctx, "ACL", "LOG", 128).Result()
				if err != nil {
					return err
				}
				require.NotContains(t, fmt.Sprint(entries), "client|setinfo")
				return nil
			}))
		})
	}
}
