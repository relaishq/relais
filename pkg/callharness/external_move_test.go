package callharness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestExternalMoveFailedOwnerIsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "failed", http.StatusInternalServerError) }))
	defer server.Close()
	h, err := Start(Options{External: &ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: netip.MustParseAddrPort("127.0.0.1:20000")}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	c := &Call{harness: h, rec: newRecorder(), remoteUfrag: "test-call"}
	if err := c.MoveExternal(context.Background(), 1); err == nil {
		t.Fatal("move unexpectedly succeeded")
	}
	if len(c.rec.moves) != 1 || c.rec.moves[0].from != -1 || c.rec.moves[0].err == nil {
		t.Fatalf("failed move owner is not unknown: %+v", c.rec.moves)
	}
}
