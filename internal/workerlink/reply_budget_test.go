package workerlink

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
)

func TestSlowWriterBoundsRetainedReplyBytes(t *testing.T) {
	reader, send := io.Pipe()
	writer := &blockedReplyWriter{entered: make(chan struct{}), release: make(chan struct{})}
	var retained atomic.Int64
	completed := make(chan struct{}, MaxConcurrent)
	peer := New(reader, writer, func() { _ = reader.Close() }, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n, _ := w.Write(make([]byte, 512<<10))
		retained.Add(int64(n))
		completed <- struct{}{}
	}))
	defer func() { _ = send.Close(); _ = reader.Close(); close(writer.release); peer.Close() }()
	go func() { _ = peer.Run(context.Background()) }()
	for id := uint64(1); id <= MaxConcurrent; id++ {
		if err := Write(send, Message{Kind: "request", ID: id, Timeout: int64(privateapi.Timeout), Method: "POST", Path: "/export"}); err != nil {
			t.Fatal(err)
		}
	}
	for range MaxConcurrent {
		select {
		case <-completed:
		case <-time.After(2 * time.Second):
			t.Fatal("handler stalled")
		}
	}
	if got := retained.Load(); got > 4*privateapi.MaxBody {
		t.Fatalf("slow writer retained %d bytes, limit %d", got, 4*privateapi.MaxBody)
	}
}
func TestRefusalDoesNotBlockCommandReader(t *testing.T) {
	reader, send := io.Pipe()
	writer := &blockedReplyWriter{entered: make(chan struct{}), release: make(chan struct{})}
	handlers := make(chan struct{})
	started := make(chan struct{}, MaxConcurrent)
	peer := New(reader, writer, func() { _ = reader.Close() }, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { started <- struct{}{}; <-handlers }))
	defer func() { _ = send.Close(); _ = reader.Close(); close(writer.release); close(handlers); peer.Close() }()
	go func() { _ = peer.Run(context.Background()) }()
	for id := uint64(1); id <= MaxConcurrent; id++ {
		if err := Write(send, Message{Kind: "request", ID: id, Timeout: int64(privateapi.Timeout), Method: "POST", Path: "/export"}); err != nil {
			t.Fatal(err)
		}
	}
	for range MaxConcurrent {
		<-started
	}
	if err := Write(send, Message{Kind: "request", ID: 65, Timeout: int64(privateapi.Timeout), Method: "POST", Path: "/end"}); err != nil {
		t.Fatal(err)
	}
	<-writer.entered
	read := make(chan error, 1)
	go func() { read <- Write(send, Message{Kind: "ping"}) }()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("refusal writer blocked the read loop")
	}
}
func TestBudgetStartsAtFirstRequestChunk(t *testing.T) {
	reader, send := io.Pipe()
	receive, writer := io.Pipe()
	peer := New(reader, writer, func() { _ = reader.Close(); _ = writer.Close() }, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("expired chunked command applied")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer func() { peer.Close(); _ = send.Close(); _ = receive.Close() }()
	go func() { _ = peer.Run(context.Background()) }()
	m := Message{Kind: "request", ID: 1, Timeout: int64(300 * time.Millisecond), Method: "POST", Path: "/sessions", More: true, Body: []byte("a")}
	if err := Write(send, m); err != nil {
		t.Fatal(err)
	}
	time.Sleep(450 * time.Millisecond)
	m.More = false
	m.Body = []byte("b")
	if err := Write(send, m); err != nil {
		t.Fatal(err)
	}
	reply, err := Read(receive)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Error != "not_applied" {
		t.Fatalf("expired chunks got %+v", reply)
	}
}
