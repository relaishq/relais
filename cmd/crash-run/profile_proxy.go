package main

import (
	"github.com/relais/internal/movediag"
	"github.com/relais/internal/privateapi"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
)

// Driver-only fault: let real resume commit, then discard exactly one reply.
// A malformed successful JSON reply is classified ErrUncertain by the real
// privateapi client. No service test flags or machine-wide settings are added.
type uncertainProxy struct {
	transport *http.Transport
	*httptest.Server
	armed atomic.Bool
	count atomic.Int32
}

func newUncertainProxy(host, endpoint string) (*uncertainProxy, error) {
	target, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, err
	}
	fault := &uncertainProxy{}
	proxy := httputil.NewSingleHostReverseProxy(target)
	fault.transport = &http.Transport{Proxy: nil}
	proxy.Transport = diagnosticProxyTransport{base: fault.transport}
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.Request.Method == http.MethodPost && resp.Request.URL.Path == "/resume" && resp.StatusCode/100 == 2 && fault.armed.CompareAndSwap(true, false) {
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(strings.NewReader("{"))
			resp.ContentLength = 1
			resp.Header.Set("Content-Length", "1")
			fault.count.Add(1)
		}
		return nil
	}
	fault.Server = httptest.NewUnstartedServer(movediag.Default.Handler(proxy))
	_ = fault.Listener.Close()
	fault.Listener = listener
	fault.Start()
	return fault, nil
}
func (p *uncertainProxy) Arm()       { p.armed.Store(true) }
func (p *uncertainProxy) Count() int { return int(p.count.Load()) }

func (p *uncertainProxy) Close() { p.Server.Close(); p.transport.CloseIdleConnections() }

// The fault proxy adds a second TCP leg. Record it separately from the
// controller's logical private RPC, including reading the complete body.
type diagnosticProxyTransport struct{ base http.RoundTripper }

func (t diagnosticProxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	event := movediag.NewEvent(req.Context(), "http_proxy", req.Method+" "+privateapi.DiagnosticOperation(req.URL.Path))
	if event.TraceID == "" {
		return t.base.RoundTrip(req)
	}
	event.RequestBytes = int(max(req.ContentLength, 0))
	var mu sync.Mutex
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		mu.Lock()
		defer mu.Unlock()
		event.Connections++
		if info.Reused {
			event.ReusedConnections++
		}
	}}))
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		mu.Lock()
		movediag.Default.Finish(event, true)
		mu.Unlock()
		return nil, err
	}
	resp.Body = &diagnosticProxyBody{ReadCloser: resp.Body, event: &event, mu: &mu}
	return resp, nil
}

type diagnosticProxyBody struct {
	io.ReadCloser
	event  *movediag.Event
	mu     *sync.Mutex
	failed bool
	once   sync.Once
}

func (b *diagnosticProxyBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	b.event.ResponseBytes += n
	b.failed = b.failed || (err != nil && err != io.EOF)
	b.mu.Unlock()
	return n, err
}
func (b *diagnosticProxyBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.mu.Lock(); defer b.mu.Unlock(); movediag.Default.Finish(*b.event, b.failed || err != nil) })
	return err
}
