package workerlink

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
)

func pipePeers(t *testing.T, handler http.Handler) (*Peer, *Peer) {
	t.Helper()
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	stop := func() { _ = ar.Close(); _ = aw.Close(); _ = br.Close(); _ = bw.Close() }
	a := New(ar, aw, stop, nil)
	b := New(br, bw, stop, handler)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); a.Close(); b.Close() })
	go func() { _ = a.Run(ctx) }()
	go func() { _ = b.Run(ctx) }()
	return a, b
}
func TestCommandsShareChunkedStream(t *testing.T) {
	peer, _ := pipePeers(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-r.Context().Done()
			return
		}
		_, _ = io.Copy(w, r.Body)
	}))
	client := &http.Client{Transport: peer, Timeout: 3 * time.Second}
	var wg sync.WaitGroup
	for _, size := range []int{0, 32, ChunkSize + 1, 512 << 10} {
		wg.Go(func() {
			body := bytes.Repeat([]byte{'x'}, size)
			req, _ := http.NewRequest(http.MethodPost, "http://worker/echo", bytes.NewReader(body))
			resp, err := client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			got, err := io.ReadAll(resp.Body)
			if err != nil || !bytes.Equal(body, got) {
				t.Errorf("size=%d reply mismatch: %v", size, err)
			}
		})
	}
	wg.Wait()
}
func TestLinkFailureNeverRedelivers(t *testing.T) {
	accepted := make(chan struct{})
	peer, worker := pipePeers(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(accepted)
		workerGone := time.NewTimer(time.Second)
		defer workerGone.Stop()
		<-workerGone.C
	}))
	client := &http.Client{Transport: peer}
	done := make(chan error, 1)
	go func() {
		_, err := client.Post("http://worker/sessions", "application/json", bytes.NewBufferString("{}"))
		done <- err
	}()
	<-accepted
	worker.Close()
	err := <-done
	if err == nil || errors.Is(err, ErrNotSent) {
		t.Fatalf("accepted command must be uncertain: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://worker/status", nil)
	_, err = peer.RoundTrip(req)
	if !errors.Is(err, ErrNotSent) {
		t.Fatalf("closed stream must prove not sent: %v", err)
	}
}
func TestCommandDeadlineReachesHandler(t *testing.T) {
	observed := make(chan time.Time, 1)
	peer, _ := pipePeers(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		deadline, _ := r.Context().Deadline()
		observed <- deadline
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://worker/slow", nil)
	_, err := peer.RoundTrip(req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline lost: %v", err)
	}
	deadline := <-observed
	if want, _ := ctx.Deadline(); deadline.Before(want.Add(-time.Millisecond)) || deadline.After(want.Add(100*time.Millisecond)) {
		t.Fatalf("deadline changed: %s vs %s", deadline, want)
	}
}
func TestFrameAndBodyBounds(t *testing.T) {
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], MaxFrame+1)
	if _, err := Read(bytes.NewReader(prefix[:])); err == nil {
		t.Fatal("oversize frame accepted")
	}
	var wire bytes.Buffer
	if err := Write(&wire, Message{Kind: "request", Body: make([]byte, MaxFrame)}); err == nil {
		t.Fatal("oversize encoded chunk accepted")
	}
	peer, _ := pipePeers(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(make([]byte, privateapi.MaxBody+1)) }))
	req, _ := http.NewRequest(http.MethodPost, "http://worker/sessions", nil)
	if _, err := peer.RoundTrip(req); err == nil {
		t.Fatal("oversize reply accepted")
	}
	req, _ = http.NewRequest(http.MethodPost, "http://worker/sessions", bytes.NewReader(make([]byte, privateapi.MaxBody+1)))
	if _, err := peer.RoundTrip(req); !errors.Is(err, ErrNotSent) {
		t.Fatalf("oversize request was sent: %v", err)
	}
}

// HTTP/2 runs over an in-memory connection here. No TCP socket or special
// HTTP/2 library is needed to exercise the standard-library duplex transport.
type pipeListener struct {
	connections chan net.Conn
	done        chan struct{}
	once        sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.connections:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *pipeListener) Addr() net.Addr { return pipeAddress{} }

type pipeAddress struct{}

func (pipeAddress) Network() string { return "pipe" }
func (pipeAddress) String() string  { return "worker-link" }
func TestHTTP2LinkIsFullDuplex(t *testing.T) {
	listener := &pipeListener{connections: make(chan net.Conn), done: make(chan struct{})}
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	result := make(chan error, 1)
	server := &http.Server{Protocols: protocols, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Error("link is not HTTP/2")
			return
		}
		if err := WriteValue(w, "ready", struct{}{}); err != nil {
			result <- err
			return
		}
		peer := New(r.Body, w, func() { _ = r.Body.Close() }, nil)
		go func() { _ = peer.Run(r.Context()) }()
		req, _ := http.NewRequest(http.MethodPost, "http://worker/sessions", bytes.NewBufferString("create"))
		resp, err := peer.RoundTrip(req)
		if err == nil {
			var body []byte
			body, err = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) != "created" {
				err = errors.New("wrong tunneled reply")
			}
		}
		result <- err
		peer.Close()
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	transport := Transport()
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		a, b := net.Pipe()
		select {
		case listener.connections <- a:
			return b, nil
		case <-ctx.Done():
			_ = a.Close()
			_ = b.Close()
			return nil, ctx.Err()
		}
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://control/private/workers/a/link", reader)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var ready struct{}
	if err := ReadValue(resp.Body, "ready", &ready); err != nil {
		t.Fatal(err)
	}
	worker := New(resp.Body, writer, cancel, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "create" {
			t.Error("wrong tunneled request")
		}
		_, _ = w.Write([]byte("created"))
	}))
	go func() { _ = worker.Run(ctx) }()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestSilentLinkClosesDespiteBlockedWriter(t *testing.T) {
	reader, remoteWriter := io.Pipe()
	remoteReader, writer := io.Pipe()
	peer := New(reader, writer, func() { _ = reader.Close(); _ = writer.Close() }, nil)
	defer func() { peer.Close(); _ = remoteReader.Close(); _ = remoteWriter.Close() }()
	done := make(chan error, 1)
	go func() { done <- peer.Run(context.Background()) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("blocked ping writer prevented link loss detection")
	}
}

func TestChunkedValuePreservesMaximumWireBody(t *testing.T) {
	body := append([]byte{'"'}, bytes.Repeat([]byte{'x'}, privateapi.MaxBody-2)...)
	body = append(body, '"')
	var stream bytes.Buffer
	if err := WriteValue(&stream, "hello", json.RawMessage(body)); err != nil {
		t.Fatal(err)
	}
	var got json.RawMessage
	if err := ReadValue(&stream, "hello", &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, got) {
		t.Fatal("maximum wire body changed during chunk assembly")
	}
}

func TestChunkAssemblySupportsConcurrencyLimit(t *testing.T) {
	var stream bytes.Buffer
	budget := int64(privateapi.Timeout)
	for chunk := range 3 {
		for id := uint64(1); id <= MaxConcurrent; id++ {
			if err := Write(&stream, Message{Kind: "request", ID: id, Timeout: budget, Method: http.MethodPost, Path: "/sessions", Body: []byte{byte(chunk)}, More: chunk < 2}); err != nil {
				t.Fatal(err)
			}
		}
	}
	accepted := make(chan []byte, MaxConcurrent)
	peer := New(&stream, io.Discard, func() {}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		accepted <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := peer.Run(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("valid concurrent chunks rejected: %v", err)
	}
	for range MaxConcurrent {
		select {
		case body := <-accepted:
			if !bytes.Equal(body, []byte{0, 1, 2}) {
				t.Fatal("chunk body changed")
			}
		case <-time.After(time.Second):
			t.Fatal("assembled command was not served")
		}
	}
}
