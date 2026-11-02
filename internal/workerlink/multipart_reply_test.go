package workerlink

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
)

func TestMultipartRepliesDoNotWaitOnEachOther(t *testing.T) {
	// Exercise reply allocation and release independently of link liveness and
	// the two-second wire deadline. Serializing eight maximum replies under
	// -race is not the latency property this regression test checks.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	first := make(chan struct{}, 8)
	finish := make(chan struct{})
	errors := make(chan error, 8)
	part := make([]byte, privateapi.MaxBody/2)
	peer := New(nil, io.Discard, func() {}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write(part)
		if err != nil {
			errors <- err
			return
		}
		first <- struct{}{}
		select {
		case <-finish:
		case <-r.Context().Done():
			errors <- r.Context().Err()
			return
		}
		_, err = w.Write(part)
		errors <- err
	}))
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); peer.writeReplies() }()
	defer func() { peer.Close(); <-writerDone }()
	for id := uint64(1); id <= 8; id++ {
		go func() {
			peer.queueReply(peer.serve(ctx, Message{Kind: "request", ID: id, Method: "POST", Path: "/export"}, deadline))
		}()
	}
	// Four whole replies fit. The old incremental allocator lets all eight
	// partial replies fill the budget while none can finish and release it.
	for range 4 {
		select {
		case <-first:
		case <-time.After(time.Second):
			t.Fatal("initial reply writes did not progress")
		}
	}
	time.Sleep(200 * time.Millisecond)
	close(finish)
	for range 8 {
		select {
		case err := <-errors:
			if err != nil {
				t.Errorf("multipart reply exhausted its deadline waiting on partial reservations: %v", err)
			}
		case <-time.After(9 * time.Second):
			t.Fatal("multipart replies did not finish")
		}
	}
}
