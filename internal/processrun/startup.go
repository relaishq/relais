package processrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"syscall"
	"time"
)

// Startup has no live-call lease to retain. Allow twice a twenty-minute store
// recovery, independently of the much shorter live-call outage budget. This
// budget is per open: worker/control open two stores, so startup can take 80 min.
const startupStoreBudget = 40 * time.Minute

// A closing member and a TLS mismatch can both yield EOF/reset/broken pipe.
// Retry each consecutive run of these ambiguous disconnects for two minutes:
// tolerate a member shutting down without hiding a TLS error for 40 min.
// Any other retryable error resets the disconnect window.
const startupStoreDisconnectWindow = 2 * time.Minute

// Constructors close failed clients. Reopen them to rediscover seed addresses
// and slot maps, without changing retries of commands on established calls.
func openStartupStore[T any](ctx context.Context, kind, addresses string, open func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, startupStoreBudget)
	defer cancel()
	var zero T
	var disconnectStarted time.Time
	delay := time.Second
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, fmt.Errorf("%s store startup: %w", kind, err)
		}
		attemptCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		store, err := open(attemptCtx)
		stop()
		if err == nil {
			return store, nil
		}
		reason := startupStoreReason(err)
		if ctx.Err() != nil {
			return zero, fmt.Errorf("%s store startup: %w (last error: %q)", kind, ctx.Err(), reason)
		}
		disconnected := startupStoreDisconnected(err)
		elapsed := time.Duration(0)
		if disconnected {
			if disconnectStarted.IsZero() {
				disconnectStarted = time.Now()
			}
			elapsed = time.Since(disconnectStarted)
		} else {
			disconnectStarted = time.Time{}
		}
		if !startupStoreTransient(err, elapsed) {
			return zero, startupStoreFailure(kind, err)
		}
		wait := delay
		if disconnected {
			wait = min(wait, startupStoreDisconnectWindow-elapsed)
		}
		log.Printf("store startup retry store=%s addresses=%q attempt=%d delay=%s reason=%q", kind, addresses, attempt, wait, reason)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, fmt.Errorf("%s store startup: %w (last error: %q)", kind, ctx.Err(), reason)
		case <-timer.C:
			if disconnected && time.Since(disconnectStarted) >= startupStoreDisconnectWindow {
				return zero, startupStoreFailure(kind, err)
			}
		}
		delay = min(2*delay, 10*time.Second)
	}
}

// Preserve error matching without printing untrusted text through %w.
type startupStoreCause struct{ err error }

func (startupStoreCause) Error() string   { return "open failed" }
func (e startupStoreCause) Unwrap() error { return e.err }

func startupStoreFailure(kind string, err error) error {
	return fmt.Errorf("%s store startup: %w (reason %q)", kind, startupStoreCause{err}, startupStoreReason(err))
}

// Quote after truncating so untrusted replies cannot inject log lines.
func startupStoreReason(err error) string {
	reason := err.Error()
	if len(reason) > 256 {
		reason = reason[:256]
	}
	return reason
}

func startupStoreDisconnected(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

func startupStoreTransient(err error, elapsed time.Duration) bool {
	if startupStoreDisconnected(err) {
		return elapsed < startupStoreDisconnectWindow
	}
	// A dial/TLS-handshake or constructor attempt can expire while the outer
	// startup context still has time. Cancellation is handled by the caller.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && (op.Op == "dial" || op.Op == "read" || op.Op == "write") {
		return op.Timeout() || errors.Is(err, syscall.ECONNREFUSED)
	}
	// Session-store errors wrap server responses; inspect the original reply.
	for errors.Unwrap(err) != nil {
		err = errors.Unwrap(err)
	}
	for _, prefix := range []string{"MOVED ", "ASK ", "TRYAGAIN ", "CLUSTERDOWN ", "LOADING ", "BUSY ", "MASTERDOWN ", "ERR max number of clients reached", "redis cluster discovery: empty CLUSTER SLOTS reply"} {
		if strings.HasPrefix(err.Error(), prefix) {
			return true
		}
	}
	return false
}
