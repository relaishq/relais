package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
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
	proxy.Transport = fault.transport
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
	fault.Server = httptest.NewUnstartedServer(proxy)
	_ = fault.Listener.Close()
	fault.Listener = listener
	fault.Start()
	return fault, nil
}
func (p *uncertainProxy) Arm()       { p.armed.Store(true) }
func (p *uncertainProxy) Count() int { return int(p.count.Load()) }

func (p *uncertainProxy) Close() { p.Server.Close(); p.transport.CloseIdleConnections() }
