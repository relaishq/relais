package loadgen

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRelayLossFinishLeavesReportingAlive(t *testing.T) {
	parent, stop := context.WithCancel(context.Background())
	defer stop()
	mux := http.NewServeMux()
	ctx, f := registerRelayLossFinish(parent, mux)
	defer f.cancel()
	if f.sendError(context.Canceled) == nil {
		t.Fatal("unexpected cancellation erased")
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/relay-loss-finish", nil))
	if w.Code != 204 || ctx.Err() != context.Canceled || parent.Err() != nil {
		t.Fatal(w.Code, ctx.Err(), parent.Err())
	}
	if f.sendError(errors.Join(context.Canceled, context.Canceled)) != nil {
		t.Fatal("intentional audio/video cancellation retained")
	}
	real := errors.New("real send failure")
	joined := errors.Join(context.Canceled, real)
	if !errors.Is(f.sendError(joined), real) {
		t.Fatal("real failure erased")
	}
}
