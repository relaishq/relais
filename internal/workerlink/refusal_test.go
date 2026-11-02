package workerlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
)

func TestBusyWorkerRefusesOnlyUnappliedCommand(t *testing.T) {
	accepted := make(chan struct{}, MaxConcurrent)
	release := make(chan struct{})
	defer close(release)
	peer, _ := pipePeers(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepted <- struct{}{}
		<-release // Export/end can finish after the caller's deadline.
		w.WriteHeader(http.StatusNoContent)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	results := make(chan error, MaxConcurrent)
	for range MaxConcurrent {
		go func() {
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://worker/slow", nil)
			_, err := peer.RoundTrip(req)
			results <- err
		}()
	}
	for range MaxConcurrent {
		select {
		case <-accepted:
		case <-time.After(2 * time.Second):
			t.Fatal("worker did not fill")
		}
	}
	for range MaxConcurrent {
		if err := <-results; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline: %v", err)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, "http://worker/overflow", nil)
	_, err := peer.RoundTrip(req)
	if !errors.Is(err, ErrNotSent) || errors.Is(err, privateapi.ErrUncertain) {
		t.Fatalf("busy command must be definitely unapplied: %v", err)
	}
	select {
	case <-peer.Done():
		t.Fatal("capacity refusal closed the link")
	default:
	}
}

func TestOversizedReplyIsUncertainForOnlyOneCommand(t *testing.T) {
	peer, _ := pipePeers(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/large" {
			_, _ = w.Write(make([]byte, privateapi.MaxBody+1))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req, _ := http.NewRequest(http.MethodPost, "http://worker/large", nil)
	_, err := peer.RoundTrip(req)
	if !errors.Is(err, privateapi.ErrUncertain) || errors.Is(err, ErrNotSent) {
		t.Fatalf("applied oversized reply must be uncertain: %v", err)
	}
	req, _ = http.NewRequest(http.MethodGet, "http://worker/status", nil)
	resp, err := peer.RoundTrip(req)
	if err != nil {
		t.Fatalf("oversized reply killed another command: %v", err)
	}
	_ = resp.Body.Close()
}

func TestZeroDeadlineRefusesOnlyOneCommand(t *testing.T) {
	reader, send := io.Pipe()
	receive, writer := io.Pipe()
	peer := New(reader, writer, func() { _ = reader.Close(); _ = writer.Close() }, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("zero budget reached handler") }))
	defer func() { peer.Close(); _ = send.Close(); _ = receive.Close() }()
	go func() { _ = peer.Run(context.Background()) }()
	if err := Write(send, Message{Kind: "request", ID: 7, Method: http.MethodPost, Path: "/sessions"}); err != nil {
		t.Fatal(err)
	}
	got, err := Read(receive)
	if err != nil || got.Kind != "reply" || got.ID != 7 || got.Error != "not_applied" {
		t.Fatalf("missing per-command refusal: %+v %v", got, err)
	}
	select {
	case <-peer.Done():
		t.Fatal("zero budget closed link")
	default:
	}
}

func TestAfterSendLinkLossIsUncertain(t *testing.T) {
	accepted := make(chan struct{})
	peer, worker := pipePeers(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(accepted); <-time.After(300 * time.Millisecond) }))
	done := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://worker/export", nil)
		_, err := peer.RoundTrip(req)
		done <- err
	}()
	<-accepted
	worker.Close()
	err := <-done
	if !errors.Is(err, privateapi.ErrUncertain) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotSent) {
		t.Fatalf("after-send outcome misclassified: %v", err)
	}
}

// Rewrite only the old absolute deadline as seen on a host one hour ahead.
// A relative budget must survive this simulated wall-clock offset unchanged.
func TestCommandBudgetSurvivesClockSkew(t *testing.T) {
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	rewrittenReader, rewrittenWriter := io.Pipe()
	stop := func() {
		_ = ar.Close()
		_ = aw.Close()
		_ = br.Close()
		_ = bw.Close()
		_ = rewrittenReader.Close()
		_ = rewrittenWriter.Close()
	}
	defer stop()
	go func() {
		for {
			m, err := Read(br)
			if err != nil {
				return
			}
			var wire bytes.Buffer
			if Write(&wire, m) != nil {
				return
			}
			var header [4]byte
			_, _ = io.ReadFull(&wire, header[:])
			var fields map[string]any
			_ = json.Unmarshal(wire.Bytes(), &fields)
			if d, ok := fields["deadline"].(float64); ok {
				fields["deadline"] = d - float64(time.Hour)
			}
			raw, _ := json.Marshal(fields)
			if json.Unmarshal(raw, &m) != nil {
				return
			}
			if Write(rewrittenWriter, m) != nil {
				return
			}
		}
	}()
	peer := New(ar, aw, stop, nil)
	worker := New(rewrittenReader, bw, stop, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("applied")) }))
	go func() { _ = peer.Run(context.Background()) }()
	go func() { _ = worker.Run(context.Background()) }()
	req, _ := http.NewRequest(http.MethodPost, "http://worker/sessions", nil)
	resp, err := peer.RoundTrip(req)
	if err != nil {
		t.Fatalf("clock skew dropped command: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, []byte("applied")) {
		t.Fatal("wrong reply")
	}
}

type blockedReplyWriter struct {
	entered, release chan struct{}
	once             sync.Once
}

func (w *blockedReplyWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}
func TestReplyReleasesSlotBeforeWriting(t *testing.T) {
	reader, send := io.Pipe()
	writer := &blockedReplyWriter{entered: make(chan struct{}), release: make(chan struct{})}
	releaseHandlers := make(chan struct{})
	extra := make(chan struct{})
	peer := New(reader, writer, func() { _ = reader.Close() }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/busy":
			<-releaseHandlers
		case "/extra":
			close(extra)
		}
	}))
	defer func() {
		_ = send.Close()
		_ = reader.Close()
		close(writer.release)
		close(releaseHandlers)
		peer.Close()
	}()
	go func() { _ = peer.Run(context.Background()) }()
	for id := uint64(1); id <= MaxConcurrent; id++ {
		path := "/busy"
		if id == MaxConcurrent {
			path = "/reply"
		}
		if err := Write(send, Message{Kind: "request", ID: id, Timeout: int64(privateapi.Timeout), Method: http.MethodPost, Path: path}); err != nil {
			t.Fatal(err)
		}
	}
	<-writer.entered
	_ = Write(send, Message{Kind: "request", ID: 65, Timeout: int64(privateapi.Timeout), Method: http.MethodPost, Path: "/extra"})
	select {
	case <-extra:
	case <-time.After(time.Second):
		t.Fatal("reply writer retained worker slot")
	}
}

func TestCommandBudgetExcludesWriterQueueWait(t *testing.T) {
	observed := make(chan time.Duration, 1)
	peer, _ := pipePeers(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, _ := r.Context().Deadline()
		observed <- time.Until(deadline)
		w.WriteHeader(http.StatusNoContent)
	}))
	peer.writeMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://worker/sessions", nil)
		_, err := peer.RoundTrip(req)
		done <- err
	}()
	time.Sleep(350 * time.Millisecond)
	peer.writeMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if budget := <-observed; budget > 1250*time.Millisecond {
		t.Fatalf("writer queue time was not deducted: %s", budget)
	}
}

func TestBidirectionalChunksWithSameRequestID(t *testing.T) {
	var wire bytes.Buffer
	budget := int64(privateapi.Timeout)
	// A reply to our command and the peer's own command can share an ID.
	for _, m := range []Message{{Kind: "reply", ID: 1, Status: 200, Body: []byte("a"), More: true}, {Kind: "request", ID: 1, Timeout: budget, Method: "POST", Path: "/leg-address"}, {Kind: "reply", ID: 1, Status: 200, Body: []byte("b")}} {
		if err := Write(&wire, m); err != nil {
			t.Fatal(err)
		}
	}
	accepted := make(chan struct{}, 1)
	peer := New(&wire, io.Discard, func() {}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { accepted <- struct{}{} }))
	if err := peer.Run(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("bidirectional IDs collided: %v", err)
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("peer command not served")
	}
}
