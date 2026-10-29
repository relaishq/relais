package mediaworker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/relaishq/relais/pkg/sessionstore"
)

func TestLinkCommandRejectsStaleEpoch(t *testing.T) {
	s := &session{lease: sessionstore.Lease{Epoch: 4}}
	w := &Worker{sessions: map[string]*session{"call": s}}
	served := 0
	handler := w.LinkHandler(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) { served++; rw.WriteHeader(http.StatusNoContent) }))
	for _, epoch := range []uint64{0, 3, 4, 5} {
		req := httptest.NewRequest(http.MethodPost, "/sessions/call/export", nil)
		if epoch != 0 {
			req.Header.Set("X-Relais-Epoch", fmt.Sprint(epoch))
		}
		reply := httptest.NewRecorder()
		handler.ServeHTTP(reply, req)
		want := http.StatusConflict
		if epoch == 4 {
			want = http.StatusNoContent
		}
		if reply.Code != want {
			t.Fatalf("epoch=%d status=%d expected=%d", epoch, reply.Code, want)
		}
	}
	if served != 1 {
		t.Fatalf("stale command reached handler: %d", served)
	}
	s.fenced.Store(true)
	req := httptest.NewRequest(http.MethodDelete, "/sessions/call", nil)
	req.Header.Set("X-Relais-Epoch", "4")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if served != 1 {
		t.Fatal("fenced session received command")
	}
}

func TestDialOutRefusesSessionBeforeLegBinding(t *testing.T) {
	w := &Worker{cfg: Config{Relay: &RelayConfig{DialOut: true}}}
	if _, _, err := w.CreateSession(context.Background(), "offer"); !errors.Is(err, ErrNotEstablished) {
		t.Fatalf("unbound create accepted: %v", err)
	}
	if _, _, _, err := w.CreateSessionWithLease(context.Background(), "offer"); !errors.Is(err, ErrNotEstablished) {
		t.Fatalf("unbound create with lease accepted: %v", err)
	}
}
