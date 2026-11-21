package processrun

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

const testStartupAddresses = "127.0.0.1:16453,localhost:16454"

// The seed accepts TCP but never answers the TLS handshake. No external host
// or Redis server is needed to reproduce an unavailable startup connection.
func blackholedStartupSeed(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

func startupOpeners(c StoreConfig) map[string]func(context.Context) error {
	return map[string]func(context.Context) error{
		"session": func(ctx context.Context) error {
			store, err := c.Open(ctx)
			if store != nil {
				_ = store.Close()
			}
			return err
		},
		"frames": func(ctx context.Context) error {
			store, err := c.OpenFrames(ctx)
			if store != nil {
				_ = store.Close()
			}
			return err
		},
		"owners": func(ctx context.Context) error {
			store, err := c.OpenOwners(ctx)
			if store != nil {
				_ = store.Close()
			}
			return err
		},
	}
}

func TestStartupBlackholedSeedWaitsForCallerDeadline(t *testing.T) {
	t.Setenv("RELAIS_SESSIONSTORE_KEY", hex.EncodeToString(make([]byte, 32)))
	t.Setenv("RELAIS_REDIS_TLS", "1")
	config := StoreConfig{Address: blackholedStartupSeed(t), Cluster: true}
	for kind, open := range startupOpeners(config) {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
			defer cancel()
			err := open(ctx)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded, "startup must not give up while the caller has time")
		})
	}
}

func TestStartupRetryBudgetAndCancellation(t *testing.T) {
	captureStartupRetries(t, nil)
	for _, timeout := range []time.Duration{startupStoreBudget, 250 * time.Millisecond} {
		t.Run(timeout.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				callerBudget := timeout
				if timeout == startupStoreBudget {
					callerBudget = 2 * startupStoreBudget
				}
				ctx, cancel := context.WithTimeout(context.Background(), callerBudget)
				defer cancel()
				start := time.Now()
				attempts := 0
				_, err := openStartupStore(ctx, "session", testStartupAddresses, func(context.Context) (int, error) {
					attempts++
					return 0, context.DeadlineExceeded
				})
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.Equal(t, timeout, time.Since(start))
				if timeout == startupStoreBudget {
					require.Greater(t, attempts, 3)
				} else {
					require.Equal(t, 1, attempts)
				}
			})
		})
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		attempts := 0
		_, err := openStartupStore(ctx, "owners", testStartupAddresses, func(context.Context) (int, error) {
			attempts++
			cancel()
			return 0, context.DeadlineExceeded
		})
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 1, attempts)
	})
}

func TestStartupRetryAttemptDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		attempts := 0
		value, err := openStartupStore(context.Background(), "frames", testStartupAddresses, func(ctx context.Context) (int, error) {
			attempts++
			if attempts == 1 {
				<-ctx.Done()
				return 0, ctx.Err()
			}
			return 7, nil
		})
		require.NoError(t, err)
		require.Equal(t, 7, value)
		require.Equal(t, 2, attempts)
		require.Equal(t, 6*time.Second, time.Since(start))
	})
}

func TestStartupRetryErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"dial timeout", &net.OpError{Op: "dial", Err: syscall.ETIMEDOUT}, true},
		{"read reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, true},
		{"write pipe", &net.OpError{Op: "write", Err: syscall.EPIPE}, true},
		{"handshake timeout", context.DeadlineExceeded, true},
		{"EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"moved", errors.New("MOVED 1 127.0.0.1:16453"), true},
		{"ask", errors.New("ASK 1 127.0.0.1:16453"), true},
		{"tryagain", errors.New("TRYAGAIN unavailable"), true},
		{"clusterdown", errors.New("CLUSTERDOWN unavailable"), true},
		{"loading", errors.New("LOADING dataset in memory"), true},
		{"busy", errors.New("BUSY script running"), true},
		{"masterdown", errors.New("MASTERDOWN link unavailable"), true},
		{"max clients", errors.New("ERR max number of clients reached"), true},
		{"empty slots", errors.New("redis cluster discovery: empty CLUSTER SLOTS reply"), true},
		{"auth", errors.New("WRONGPASS invalid username-password pair"), false},
		{"noauth", errors.New("NOAUTH Authentication required"), false},
		{"TLS record", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, false},
		{"TLS alert", &net.OpError{Op: "remote error", Err: tls.AlertError(40)}, false},
		{"TLS certificate", &tls.CertificateVerificationError{Err: errors.New("untrusted certificate")}, false},
		{"canceled", context.Canceled, false},
		{"address", &net.OpError{Op: "dial", Err: &net.AddrError{Err: "bad address"}}, false},
		{"DNS", &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", IsNotFound: true}}, false},
		{"permission", &net.OpError{Op: "dial", Err: syscall.EACCES}, false},
		{"other", errors.New("ERR unexpected reply"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, err := range []error{tc.err, &sessionstore.TransientError{Op: "session_ping", Err: tc.err}} {
				require.Equal(t, tc.retry, startupStoreTransient(err, 0))
				if tc.retry {
					synctest.Test(t, func(t *testing.T) {
						attempts := 0
						_, got := openStartupStore(context.Background(), "session", testStartupAddresses, func(context.Context) (int, error) {
							attempts++
							if attempts == 1 {
								return 0, err
							}
							return 1, nil
						})
						require.NoError(t, got)
						require.Equal(t, 2, attempts)
					})
				} else {
					attempts := 0
					_, got := openStartupStore(context.Background(), "session", testStartupAddresses, func(context.Context) (int, error) {
						attempts++
						return 0, err
					})
					require.ErrorIs(t, got, err)
					require.Equal(t, 1, attempts)
				}
			}
		})
	}
}

func TestStartupDisconnectRetryWindow(t *testing.T) {
	captureStartupRetries(t, nil)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"EOF", io.EOF},
		{"unexpected EOF", io.ErrUnexpectedEOF},
		{"reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}},
		{"broken pipe", &net.OpError{Op: "write", Err: syscall.EPIPE}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, err := range []error{tc.err, &sessionstore.TransientError{Op: "session_ping", Err: tc.err}} {
				require.True(t, startupStoreTransient(err, startupStoreDisconnectWindow-time.Nanosecond))
				require.False(t, startupStoreTransient(err, startupStoreDisconnectWindow))
				require.False(t, startupStoreTransient(err, startupStoreDisconnectWindow+time.Nanosecond))
				synctest.Test(t, func(t *testing.T) {
					started := time.Now()
					attempts := 0
					_, got := openStartupStore(context.Background(), "session", testStartupAddresses, func(context.Context) (int, error) {
						attempts++
						return 0, err
					})
					require.ErrorIs(t, got, err)
					require.Greater(t, attempts, 1)
					require.Equal(t, startupStoreDisconnectWindow, time.Since(started))
				})
				synctest.Test(t, func(t *testing.T) {
					started := time.Now()
					disconnects := 0
					_, got := openStartupStore(context.Background(), "session", testStartupAddresses, func(context.Context) (int, error) {
						if time.Since(started) <= startupStoreDisconnectWindow {
							return 0, context.DeadlineExceeded
						}
						if disconnects < 2 {
							disconnects++
							return 0, err
						}
						return 7, nil
					})
					require.NoError(t, got)
					require.Equal(t, 2, disconnects, "disconnects after a long dial outage still retry")
					require.Greater(t, time.Since(started), startupStoreDisconnectWindow)
				})
			}
		})
	}
}

func TestStartupDisconnectRunResetsAfterOtherRetryableErrors(t *testing.T) {
	captureStartupRetries(t, nil)
	for _, reset := range []error{context.DeadlineExceeded, errors.New("LOADING dataset in memory"), &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}} {
		t.Run(reset.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				resetOnce := false
				var secondRun time.Time
				_, err := openStartupStore(context.Background(), "session", testStartupAddresses, func(context.Context) (int, error) {
					if !resetOnce && time.Since(started) >= startupStoreDisconnectWindow/2 {
						resetOnce = true
						return 0, reset
					}
					if resetOnce && secondRun.IsZero() {
						secondRun = time.Now()
					}
					return 0, io.EOF
				})
				require.ErrorIs(t, err, io.EOF)
				require.True(t, resetOnce)
				require.Equal(t, startupStoreDisconnectWindow, time.Since(secondRun))
				require.Greater(t, time.Since(started), 3*startupStoreDisconnectWindow/2)
			})
		})
	}
}

func TestStartupPlainClientToTLSListenerIsDisconnect(t *testing.T) {
	_, cert, key, _ := redisTLSCertificates(t, t.TempDir())
	pair, err := tls.LoadX509KeyPair(cert, key)
	require.NoError(t, err)
	addr := startupRedisServer(t, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}, nil, nil)
	options, err := (storage.RedisConfig{Addr: addr}).ConnectionOptions()
	require.NoError(t, err)
	options.MaxRetries = -1
	client := redis.NewClient(options.Simple())
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = client.Ping(ctx).Err()
	require.Error(t, err)
	require.NoError(t, ctx.Err(), "one attempt must return a disconnect, not consume the caller deadline")
	require.True(t, startupStoreDisconnected(err), "plain client against TLS listener: %v", err)
	require.True(t, startupStoreTransient(err, 0))
}

func TestStartupPermanentErrorQuotesReasonAndPreservesCause(t *testing.T) {
	cause := &net.OpError{Op: "remote error", Err: errors.New("ERR bad\nreply\"" + strings.Repeat("x", 512))}
	_, err := openStartupStore(context.Background(), "session", testStartupAddresses, func(context.Context) (int, error) {
		return 0, cause
	})
	require.ErrorIs(t, err, cause)
	var matched *net.OpError
	require.ErrorAs(t, err, &matched)
	require.Same(t, cause, matched)
	require.NotContains(t, err.Error(), "\n")
	quoted := strings.TrimSuffix(strings.SplitN(err.Error(), "reason ", 2)[1], ")")
	reason, unquoteErr := strconv.Unquote(quoted)
	require.NoError(t, unquoteErr)
	require.Len(t, reason, 256)
	require.Contains(t, reason, "\nreply\"")
}

type startupRetryLog struct {
	bytes.Buffer
	onRetry func()
}

func (w *startupRetryLog) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("store startup retry")) && w.onRetry != nil {
		w.onRetry()
	}
	return w.Buffer.Write(p)
}

func captureStartupRetries(t *testing.T, onRetry func()) *startupRetryLog {
	t.Helper()
	w := &startupRetryLog{onRetry: onRetry}
	previous := log.Writer()
	log.SetOutput(w)
	t.Cleanup(func() { log.SetOutput(previous) })
	return w
}

func TestStartupRetryQuotesAndBoundsReason(t *testing.T) {
	w := captureStartupRetries(t, nil)
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		_, err := openStartupStore(context.Background(), "session", testStartupAddresses, func(context.Context) (int, error) {
			attempts++
			if attempts == 1 {
				return 0, errors.New("CLUSTERDOWN bad\nreply\"" + strings.Repeat("x", 512))
			}
			return 1, nil
		})
		require.NoError(t, err)
	})
	line := w.String()
	require.Equal(t, 1, strings.Count(line, "\n"))
	require.Contains(t, line, "addresses="+strconv.Quote(testStartupAddresses))
	reason, err := strconv.Unquote(strings.TrimSpace(strings.SplitN(line, "reason=", 2)[1]))
	require.NoError(t, err)
	require.Len(t, reason, 256)
	require.Contains(t, reason, "\nreply\"")
}

func TestStartupFinalErrorQuotesAndBoundsReason(t *testing.T) {
	captureStartupRetries(t, nil)
	for _, duringAttempt := range []bool{true, false} {
		t.Run(strconv.FormatBool(duringAttempt), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
				defer cancel()
				_, err := openStartupStore(ctx, "session", testStartupAddresses, func(context.Context) (int, error) {
					if duringAttempt {
						cancel()
					}
					return 0, errors.New("CLUSTERDOWN bad\nreply\"" + strings.Repeat("x", 512))
				})
				require.ErrorIs(t, err, ctx.Err())
				require.NotContains(t, err.Error(), "\n")
				quoted := strings.TrimSuffix(strings.SplitN(err.Error(), "last error: ", 2)[1], ")")
				reason, unquoteErr := strconv.Unquote(quoted)
				require.NoError(t, unquoteErr)
				require.Len(t, reason, 256)
				require.Contains(t, reason, "\nreply\"")
			})
		})
	}
}

// A minimal RESP2 server exercises the production constructors and discovery.
// The seed can black-hole TLS handshakes until an outer startup retry occurs.
func startupRedisServer(t *testing.T, transport *tls.Config, healthy *atomic.Bool, reply func([]string) string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if healthy != nil && !healthy.Load() {
					_, _ = io.Copy(io.Discard, conn)
					return
				}
				stream := conn
				if transport != nil {
					stream = tls.Server(conn, transport)
				}
				reader := bufio.NewReader(stream)
				for {
					var count int
					if _, err := fmt.Fscanf(reader, "*%d\r\n", &count); err != nil {
						return
					}
					args := make([]string, count)
					for i := range args {
						var length int
						if _, err := fmt.Fscanf(reader, "$%d\r\n", &length); err != nil {
							return
						}
						data := make([]byte, length+2)
						if _, err := io.ReadFull(reader, data); err != nil {
							return
						}
						args[i] = string(data[:length])
					}
					response := "+OK\r\n"
					switch strings.ToLower(args[0]) {
					case "hello":
						response = "-ERR unknown command 'HELLO'\r\n"
					case "ping":
						response = "+PONG\r\n"
					case "cluster":
						response = "-CLUSTERDOWN unavailable\r\n"
					}
					if reply != nil {
						if custom := reply(args); custom != "" {
							response = custom
						}
					}
					if _, err := io.WriteString(stream, response); err != nil {
						return
					}
				}
			}()
		}
	}()
	return listener.Addr().String()
}

func TestStartupStoreRecoversAfterBlackholedSeed(t *testing.T) {
	ca, cert, key, _ := redisTLSCertificates(t, t.TempDir())
	pair, err := tls.LoadX509KeyPair(cert, key)
	require.NoError(t, err)
	t.Setenv("RELAIS_SESSIONSTORE_KEY", hex.EncodeToString(make([]byte, 32)))
	t.Setenv("RELAIS_REDIS_TLS", "1")
	t.Setenv("RELAIS_REDIS_TLS_CA_FILE", ca)
	transport := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	for _, kind := range []string{"session", "frames", "owners"} {
		t.Run(kind, func(t *testing.T) {
			var healthy atomic.Bool
			var seed string
			seed = startupRedisServer(t, transport, &healthy, func(args []string) string {
				if args[0] != "cluster" {
					return ""
				}
				_, port, _ := net.SplitHostPort(seed)
				return fmt.Sprintf("*1\r\n*3\r\n:0\r\n:16383\r\n*2\r\n$9\r\n127.0.0.1\r\n:%s\r\n", port)
			})
			w := captureStartupRetries(t, func() { healthy.Store(true) })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = startupOpeners(StoreConfig{Address: seed, Cluster: true})[kind](ctx)
			require.NoError(t, err)
			require.Contains(t, w.String(), "store startup retry store="+kind)
		})
	}
}

func TestStartupStoreRejectsPermanentFailures(t *testing.T) {
	ca, cert, key, _ := redisTLSCertificates(t, t.TempDir())
	pair, err := tls.LoadX509KeyPair(cert, key)
	require.NoError(t, err)
	t.Setenv("RELAIS_SESSIONSTORE_KEY", hex.EncodeToString(make([]byte, 32)))
	t.Setenv("RELAIS_REDIS_TLS_CA_FILE", ca)
	transport := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	for _, failure := range []string{"auth", "TLS certificate"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("RELAIS_REDIS_TLS", "1")
			if failure == "TLS certificate" {
				t.Setenv("RELAIS_REDIS_TLS_SERVER_NAME", "wrong.test")
			}
			seed := startupRedisServer(t, transport, nil, func([]string) string {
				return "-WRONGPASS invalid username-password pair\r\n"
			})
			for kind, open := range startupOpeners(StoreConfig{Address: seed, Cluster: true}) {
				t.Run(kind, func(t *testing.T) {
					w := captureStartupRetries(t, nil)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					err := open(ctx)
					require.Error(t, err)
					require.NoError(t, ctx.Err(), "permanent errors must fail before the startup budget")
					require.Empty(t, w.String(), "no startup retry for permanent errors")
				})
			}
		})
	}
}
