package callharness

import (
	"context"
	"github.com/relais/pkg/controlplane"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
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

func TestRecordOperatorDrainPreservesCallerMove(t *testing.T) {
	rec := newRecorder()
	started := rec.start.Add(time.Second)
	rec.move(moveRecord{from: 0, to: 1, start: started, end: started.Add(time.Millisecond)})
	// The observer must keep the actual API times and worker IDs.
	c := &Call{rec: rec}
	c.RecordExternalMove(controlplane.MoveResult{From: "1", To: "2", Start: started.Add(time.Second), End: started.Add(time.Second + time.Millisecond)})
	if len(rec.moves) != 2 || rec.moves[1].from != 1 || rec.moves[1].to != 2 || !rec.moves[1].start.Equal(started.Add(time.Second)) {
		t.Fatalf("wrong drain observation: %+v", rec.moves)
	}
}
