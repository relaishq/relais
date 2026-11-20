package sessionstore

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// injectedErrorHook fails every session script command with err before any
// network I/O, so the retry path runs without a Redis server.
type injectedErrorHook struct {
	err      error
	attempts atomic.Int32
}

func (*injectedErrorHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *injectedErrorHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() != "evalsha" && cmd.Name() != "eval" {
			return next(ctx, cmd)
		}
		h.attempts.Add(1)
		cmd.SetErr(h.err)
		return h.err
	}
}
func (*injectedErrorHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func refusedDial() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
}

// goDialTimeout is the error Go returns when a connect attempt runs out of time.
func goDialTimeout() error {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now())
	defer cancel()
	_, err := (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:1")
	return err
}

// Pins which error kinds the session write and read paths retry, which return
// at once, whether the result is transient, and that every path stays inside
// the two-second command limit.
func TestRedisRetryPathByErrorKind(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		kind      string
		writes    int32 // attempts by the write loop (renew); 0: until the command limit
		reads     int32 // attempts by retryRead (get)
		transient bool
	}{
		{"moved", errors.New("MOVED 1 127.0.0.1:1"), "moved", 3, 3, true},
		{"ask", errors.New("ASK 1 127.0.0.1:1"), "ask", 3, 3, true},
		{"tryagain", errors.New("TRYAGAIN temporary"), "tryagain", 3, 3, true},
		{"clusterdown", errors.New("CLUSTERDOWN unavailable"), "clusterdown", 3, 3, true},
		{"busy", errors.New("BUSY script"), "busy", 3, 3, true},
		{"eof", io.EOF, "eof", 1, 3, true},
		{"read reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, "net", 1, 3, true},
		{"write timeout", &net.OpError{Op: "write", Net: "tcp", Err: timeoutError{}}, "timeout", 1, 3, true},
		{"read timeout", &net.OpError{Op: "read", Net: "tcp", Err: timeoutError{}}, "timeout", 1, 3, true},
		{"dial refused", refusedDial(), "dial", 0, 3, true},
		{"wrapped dial refused", fmt.Errorf("pool: %w", refusedDial()), "dial", 0, 3, true},
		{"dial timeout", &net.OpError{Op: "dial", Net: "tcp", Err: timeoutError{}}, "dial", 0, 3, true},
		// Go's own dial timeout also matches context.DeadlineExceeded.
		{"go dial timeout", goDialTimeout(), "dial", 0, 3, true},
		{"deadline", context.DeadlineExceeded, "canceled", 1, 1, true},
		{"tls handshake remote alert", &net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")}, "net", 1, 3, true},
		{"tls record header", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, "other", 1, 1, false},
		{"canceled", context.Canceled, "canceled", 1, 1, true},
		{"permanent", errors.New("NOAUTH denied"), "other", 1, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.kind, redisKind(tc.err))
			for _, path := range []struct {
				op   string
				want int32
			}{{"renew", tc.writes}, {"get", tc.reads}} {
				client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
				hook := &injectedErrorHook{err: tc.err}
				client.AddHook(hook)
				r := &Redis{client: client, prefix: "kinds:", retention: time.Minute}
				start := time.Now()
				_, err := r.run(context.Background(), path.op, "id", "127.0.0.1:1", "1", time.Minute)
				elapsed := time.Since(start)
				require.NoError(t, client.Close())
				require.Error(t, err)
				if path.want == 0 {
					require.Greater(t, hook.attempts.Load(), int32(3), path.op)
					require.GreaterOrEqual(t, elapsed, transitionCommandLimit*3/4, path.op)
				} else {
					require.Equal(t, path.want, hook.attempts.Load(), path.op)
				}
				require.Equal(t, tc.transient, errors.Is(err, ErrTransient), path.op)
				require.Less(t, elapsed, transitionCommandLimit, path.op)
			}
		})
	}
}

type countScriptsHook struct{ attempts atomic.Int32 }

func (*countScriptsHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *countScriptsHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "evalsha" || cmd.Name() == "eval" {
			h.attempts.Add(1)
		}
		return next(ctx, cmd)
	}
}
func (*countScriptsHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// A real go-redis dial to a closed port, as after a primary dies: the error
// shape comes from the driver, not from a hand-built value.
func TestRedisRefusedDialRetryPath(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	client := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, DialerRetries: 1, ContextTimeoutEnabled: true})
	defer func() { _ = client.Close() }()
	hook := &countScriptsHook{}
	client.AddHook(hook)
	r := &Redis{client: client, prefix: "kinds:", retention: time.Minute}
	start := time.Now()
	_, err = r.run(context.Background(), "renew", "id", "127.0.0.1:1", "1", time.Minute)
	require.Error(t, err)
	require.Less(t, time.Since(start), transitionCommandLimit)
	require.Equal(t, "dial", redisKind(err))
	require.Greater(t, hook.attempts.Load(), int32(3))
	require.ErrorIs(t, err, ErrTransient)
}

// Pins how long the write loop keeps retrying a refused dial and how many
// attempts it makes inside the two-second command limit.
func TestRedisDialWriteRetryTiming(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	defer func() { _ = client.Close() }()
	hook := &injectedErrorHook{err: refusedDial()}
	client.AddHook(hook)
	r := &Redis{client: client, prefix: "kinds:", retention: time.Minute}
	start := time.Now()
	_, err := r.run(context.Background(), "renew", "id", "127.0.0.1:1", "1", time.Minute)
	elapsed := time.Since(start)
	require.ErrorIs(t, err, ErrTransient)
	require.Equal(t, "dial", redisKind(err))
	// Delays 20, 40, 80, 160, then 200 ms while the limit leaves time.
	require.GreaterOrEqual(t, hook.attempts.Load(), int32(6))
	require.LessOrEqual(t, hook.attempts.Load(), int32(14))
	require.GreaterOrEqual(t, elapsed, transitionCommandLimit*3/4)
	require.Less(t, elapsed, transitionCommandLimit)
}

// sequenceErrorHook fails session script commands with errs in order, then
// repeats the last one.
type sequenceErrorHook struct {
	errs     []error
	attempts atomic.Int32
}

func (*sequenceErrorHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *sequenceErrorHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() != "evalsha" && cmd.Name() != "eval" {
			return next(ctx, cmd)
		}
		n := int(h.attempts.Add(1))
		err := h.errs[min(n, len(h.errs))-1]
		cmd.SetErr(err)
		return err
	}
}
func (*sequenceErrorHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// After dial retries reach a refreshed node, its definite-not-run errors keep
// their own three-attempt cap instead of retrying until the command limit.
func TestRedisMixedDialThenBusyKeepsThreeAttemptCap(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	defer func() { _ = client.Close() }()
	busy := errors.New("BUSY script")
	hook := &sequenceErrorHook{errs: []error{refusedDial(), refusedDial(), refusedDial(), refusedDial(), busy}}
	client.AddHook(hook)
	r := &Redis{client: client, prefix: "kinds:", retention: time.Minute}
	start := time.Now()
	_, err := r.run(context.Background(), "renew", "id", "127.0.0.1:1", "1", time.Minute)
	elapsed := time.Since(start)
	require.ErrorIs(t, err, ErrTransient)
	require.ErrorContains(t, err, "BUSY")
	require.Equal(t, int32(7), hook.attempts.Load())
	require.Less(t, elapsed, transitionCommandLimit/2)
}
