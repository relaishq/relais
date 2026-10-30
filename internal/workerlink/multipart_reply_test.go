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
	reader, send := io.Pipe()
	first := make(chan struct{}, 8)
	finish := make(chan struct{})
	errors := make(chan error, 8)
	part := make([]byte, privateapi.MaxBody/2)
	peer := New(reader, io.Discard, func() { _ = reader.Close() }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer func() { peer.Close(); _ = send.Close() }()
	go func() { _ = peer.Run(context.Background()) }()
	for id := uint64(1); id <= 8; id++ {
		if err := Write(send, Message{Kind: "request", ID: id, Timeout: int64(privateapi.Timeout), Method: "POST", Path: "/export"}); err != nil {
			t.Fatal(err)
		}
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
		case <-time.After(3 * time.Second):
			t.Fatal("multipart replies did not finish")
		}
	}
}
