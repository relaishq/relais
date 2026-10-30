package controlplane

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/sessionstore"
)

func receiveLinkTest[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// Exercise the same HTTP handlers and framed streams without opening listeners.
// Socket/process tests remain separate acceptance evidence.
type handlerTransport struct{ handler http.Handler }

func (tr handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	reader, writer := io.Pipe()
	head := make(chan *http.Response, 1)
	done := make(chan struct{})
	rw := &pipeResponse{header: make(http.Header), writer: writer, head: head, request: req, reader: reader}
	go func() {
		defer close(done)
		defer func() { _ = writer.Close() }()
		request := req.Clone(req.Context())
		request.ProtoMajor = 2
		tr.handler.ServeHTTP(rw, request)
		rw.WriteHeader(http.StatusOK)
	}()
	go func() {
		select {
		case <-req.Context().Done():
			_ = reader.Close()
			_ = writer.Close()
			if req.Body != nil {
				_ = req.Body.Close()
			}
		case <-done:
		}
	}()
	select {
	case resp := <-head:
		return resp, nil
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
}

type pipeResponse struct {
	header  http.Header
	writer  *io.PipeWriter
	reader  *io.PipeReader
	head    chan *http.Response
	request *http.Request
	once    sync.Once
}

func (w *pipeResponse) Header() http.Header { return w.header }
func (w *pipeResponse) Flush()              {}
func (w *pipeResponse) WriteHeader(code int) {
	w.once.Do(func() {
		w.head <- &http.Response{StatusCode: code, Header: w.header.Clone(), Body: w.reader, Request: w.request, ProtoMajor: 2, ProtoMinor: 0}
	})
}
func (w *pipeResponse) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.writer.Write(b)
}

func newPipeAdmissionRig(t *testing.T, relayHandler http.Handler, store sessionstore.Store) *admissionRig {
	t.Helper()
	p := New(nil, store)
	remote := &RemoteRelay{URL: "http://relay", Client: &http.Client{Transport: handlerTransport{relayHandler}}}
	links := p.ConfigureDialOut([]string{"a", "b"}, remote)
	t.Cleanup(links.Close)
	return &admissionRig{p: p, links: links, server: &httptest.Server{URL: "http://control"}, client: &http.Client{Transport: handlerTransport{links.Handler(p.ProcessHandler())}}, addr: netip.MustParseAddrPort("127.0.0.1:17001")}
}

func noContentHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
}

func reportLeg(ctx context.Context, client *http.Client, addr netip.AddrPort) error {
	return privateapi.Do(ctx, client, "http://control", http.MethodPost, "/leg-address", LinkAddress{Address: addr}, nil, RemoteErrors)
}
