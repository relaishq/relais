package workerlink

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
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

func TestReplyBudgetWaitsForWriter(t *testing.T) {
	for _, outcome := range []string{"capacity", "deadline", "link-close"} {
		t.Run(outcome, func(t *testing.T) { testReplyBudgetWait(t, outcome) })
	}
}
func testReplyBudgetWait(t *testing.T, outcome string) {
	reader, send := io.Pipe()
	writer := &budgetReplyWriter{release: make(chan struct{}), small: make(chan Message, 1)}
	written := make(chan struct{}, 4)
	entered, finished := make(chan struct{}), make(chan struct{})
	peer := New(reader, writer, func() { _ = reader.Close(); writer.unblock() }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/small" {
			close(entered)
			_, _ = w.Write([]byte("applied"))
			close(finished)
			return
		}
		_, _ = w.Write(make([]byte, privateapi.MaxBody))
		written <- struct{}{}
	}))
	defer func() { peer.Close(); _ = send.Close() }()
	go func() { _ = peer.Run(context.Background()) }()
	for id := uint64(1); id <= 4; id++ {
		if err := Write(send, Message{Kind: "request", ID: id, Timeout: int64(privateapi.Timeout), Method: "POST", Path: "/large"}); err != nil {
			t.Fatal(err)
		}
		<-written
	}
	budget := privateapi.Timeout
	if outcome == "deadline" {
		budget = 600 * time.Millisecond
	}
	started := time.Now()
	if err := Write(send, Message{Kind: "request", ID: 5, Timeout: int64(budget), Method: "POST", Path: "/small"}); err != nil {
		t.Fatal(err)
	}
	<-entered
	select {
	case <-finished:
		t.Fatal("applied command discarded its reply instead of waiting for budget")
	case <-time.After(200 * time.Millisecond):
	}
	if outcome == "link-close" {
		peer.Close()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("link close did not interrupt reply budget wait")
		}
		return
	}
	if outcome == "deadline" {
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("reply budget wait exceeded command deadline")
		}
		if time.Since(started) < 500*time.Millisecond {
			t.Fatal("reply failed before the command budget expired")
		}
	}
	writer.unblock()
	select {
	case reply := <-writer.small:
		if reply.Kind == "reply" && reply.ID == 5 {
			if outcome == "deadline" {
				if reply.Error != "uncertain" {
					t.Fatalf("expired applied command got %+v", reply)
				}
				return
			}
			if reply.Error != "" || string(reply.Body) != "applied" {
				t.Fatalf("applied command lost its reply: %+v", reply)
			}
			return
		}
		t.Fatalf("unexpected small reply: %+v", reply)
	case <-time.After(2 * time.Second):
		t.Fatal("waiting command never received its reply")
	}
}

// The consumer releases transport backpressure without decoding the large
// bodies; only the small command's framed outcome is observed.
type budgetReplyWriter struct {
	release chan struct{}
	once    sync.Once
	small   chan Message
}

func (w *budgetReplyWriter) unblock() { w.once.Do(func() { close(w.release) }) }
func (w *budgetReplyWriter) Write(data []byte) (int, error) {
	<-w.release
	if bytes.Contains(data, []byte(`"id":5`)) {
		var m Message
		if err := json.Unmarshal(data, &m); err != nil {
			return 0, err
		}
		w.small <- m
	}
	return len(data), nil
}
