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
