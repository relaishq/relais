package storage

import (
	"crypto/tls"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConnectionOptionsPropagateSecurity(t *testing.T) {
	t.Setenv("RELAIS_REDIS_ALLOW_LOCAL_6379", "")
	transport := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "cache.example.invalid"}
	cfg := RedisConfig{Addr: "cache.example.invalid:6379", Username: "acl-private-user", Password: "private-password", DB: 2, TLSConfig: transport}
	options, err := cfg.ConnectionOptions()
	require.NoError(t, err)
	single, cluster := options.Simple(), options.Cluster()
	require.Equal(t, cfg.Addr, single.Addr)
	require.Equal(t, []string{cfg.Addr}, cluster.Addrs)
	require.Equal(t, cfg.Username, single.Username)
	require.Equal(t, cfg.Username, cluster.Username)
	require.Equal(t, cfg.Password, single.Password)
	require.Equal(t, cfg.Password, cluster.Password)
	require.Equal(t, transport.ServerName, single.TLSConfig.ServerName)
	require.Equal(t, transport.MinVersion, single.TLSConfig.MinVersion)
	require.NotSame(t, transport, single.TLSConfig)
	require.Same(t, single.TLSConfig, cluster.TLSConfig)
	require.Equal(t, 2, single.DB)
	cfg.Addrs = []string{"node-a.example.invalid:6379", "node-b.example.invalid:6379"}
	options, err = cfg.ConnectionOptions()
	require.NoError(t, err)
	require.Equal(t, cfg.Addrs, options.Cluster().Addrs)
	cfg.Addrs[0] = "changed.example.invalid:6379"
	require.NotEqual(t, cfg.Addrs, options.Cluster().Addrs, "builder owns the endpoint list")
}

func TestConnectionOptionsValidateEveryEndpointWithoutLeakingCredentials(t *testing.T) {
	t.Setenv("RELAIS_REDIS_ALLOW_LOCAL_6379", "")
	for _, cfg := range []RedisConfig{
		{},
		{Addr: "localhost:6379"},
		{Cluster: true, Addrs: []string{"cache.example.invalid:6379", "localhost:6379"}},
		{Addrs: []string{"cache.example.invalid:6379", ""}},
	} {
		cfg.Username, cfg.Password = "private-user-sentinel", "private-password-sentinel"
		_, err := cfg.ConnectionOptions()
		require.Error(t, err)
		require.NotContains(t, err.Error(), cfg.Username)
		require.NotContains(t, err.Error(), cfg.Password)
	}
}

func TestConnectionOptionsRequireVerifiedModernTLS(t *testing.T) {
	cfg := RedisConfig{Addr: "cache.example.invalid:6379", TLSConfig: &tls.Config{MinVersion: tls.VersionTLS10}}
	options, err := cfg.ConnectionOptions()
	require.NoError(t, err)
	require.Equal(t, uint16(tls.VersionTLS12), options.TLSConfig.MinVersion)
	require.Equal(t, uint16(tls.VersionTLS10), cfg.TLSConfig.MinVersion, "caller settings stay untouched")
	cfg.TLSConfig.InsecureSkipVerify = true
	_, err = cfg.ConnectionOptions()
	require.ErrorContains(t, err, "certificate verification")
	cfg.TLSConfig.InsecureSkipVerify = false
	cfg.TLSConfig.MaxVersion = tls.VersionTLS11
	_, err = cfg.ConnectionOptions()
	require.ErrorContains(t, err, "TLS 1.2")
}
