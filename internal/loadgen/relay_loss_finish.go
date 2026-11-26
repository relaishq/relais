package loadgen

import (
	"context"
	"net/http"
	"sync/atomic"
)

type relayLossFinish struct {
	requested atomic.Bool
	cancel    context.CancelFunc
}

func registerRelayLossFinish(parent context.Context, mux *http.ServeMux) (context.Context, *relayLossFinish) {
	ctx, cancel := context.WithCancel(parent)
	f := &relayLossFinish{cancel: cancel}
	mux.HandleFunc("POST /relay-loss-finish", func(w http.ResponseWriter, _ *http.Request) {
		f.requested.Store(true)
		cancel()
		w.WriteHeader(http.StatusNoContent)
	})
	return ctx, f
}

// A graceful finish clears only intentional cancellation, including the joined
// audio/video cancellations. A joined real send error remains evidence.
func (f *relayLossFinish) sendError(err error) error {
	if f.requested.Load() && onlyCancelled(err) {
		return nil
	}
	return err
}
func onlyCancelled(err error) bool {
	if err == nil || err == context.Canceled {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			if !onlyCancelled(e) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return wrapped.Unwrap() != nil && onlyCancelled(wrapped.Unwrap())
	}
	return false
}
