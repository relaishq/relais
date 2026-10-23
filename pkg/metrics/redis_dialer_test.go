package metrics

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestRedisClientPreservesDefaultTLSAndRetryOptions(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "encrypted") }))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "https://")
	tlsConfig := server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	options := &redis.Options{Protocol: 2, DisableIdentity: true, ContextTimeoutEnabled: true, DialerRetries: 1, PipelinePoolSize: -1, Addr: address, TLSConfig: tlsConfig, DialTimeout: 200 * time.Millisecond, MaxRetries: -1}
	client := RedisClient(options, "tls-test")
	defer client.Close()
	require.Nil(t, options.Dialer, "caller options must stay unchanged")
	require.Equal(t, 0, client.Options().MaxRetries, "disabled retries must remain disabled")
	require.Equal(t, 200*time.Millisecond, client.Options().DialTimeout)
	conn, err := RedisDialer(client.Options().Dialer, "tls-test")(context.Background(), "tcp", address)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
	_, err = fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", address)
	require.NoError(t, err)
	reply, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer reply.Body.Close()
	require.Equal(t, http.StatusOK, reply.StatusCode)
	require.Positive(t, counterValue(transportBytes.WithLabelValues("redis", "tls-test", "in")))
	require.Positive(t, counterValue(transportBytes.WithLabelValues("redis", "tls-test", "out")))
}

func TestRedisClientPreservesDialTimeoutDuringTLSHandshake(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	// This owned TCP peer accepts but never answers the TLS handshake.
	client := RedisClient(&redis.Options{Protocol: 2, DisableIdentity: true, ContextTimeoutEnabled: true, DialerRetries: 1, PipelinePoolSize: -1, Addr: listener.Addr().String(), DialTimeout: 40 * time.Millisecond, TLSConfig: &tls.Config{ServerName: "local.test"}, MaxRetries: -1}, "timeout-test")
	defer client.Close()
	at := time.Now()
	conn, err := client.Options().Dialer(context.Background(), "tcp", listener.Addr().String())
	require.Nil(t, conn)
	require.Error(t, err)
	var networkError net.Error
	require.ErrorAs(t, err, &networkError)
	require.True(t, networkError.Timeout())
	require.Less(t, time.Since(at), 2*time.Second)
	select {
	case peer := <-accepted:
		_ = peer.Close()
	case <-time.After(time.Second):
		t.Fatal("missing accepted connection")
	}
}
