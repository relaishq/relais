package clockprobe

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"
)

const probeTimeout = 750 * time.Millisecond
const maxBodyBytes = 1024

// Run joins all target loops before returning. Request failures are records;
// a full log or write failure stops recording. Callers keep signaling available.
func Run(ctx context.Context, targets Targets, log *Log) error {
	transport := &http.Transport{
		Proxy: nil,
		// Fresh connections also prevent Transport's implicit GET retry when a
		// reused connection fails. Each tick makes exactly one attempt.
		DisableKeepAlives:      true,
		DialContext:            (&net.Dialer{Timeout: probeTimeout}).DialContext,
		TLSHandshakeTimeout:    probeTimeout,
		ResponseHeaderTimeout:  probeTimeout,
		MaxResponseHeaderBytes: 4 << 10,
	}
	defer transport.CloseIdleConnections()
	return run(ctx, targets, log.Append, probeClient(transport))
}

func probeClient(transport http.RoundTripper) *http.Client {
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func run(ctx context.Context, targets Targets, appendRecord func(Record) error, client *http.Client) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errors := make(chan error, 1)
	var workers sync.WaitGroup
	for _, target := range targets {
		workers.Go(func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for ctx.Err() == nil {
				if err := appendRecord(probe(ctx, client, target)); err != nil {
					select {
					case errors <- err:
					default:
					}
					cancel()
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		})
	}
	workers.Wait()
	select {
	case err := <-errors:
		return err
	default:
		return nil
	}
}

func probe(ctx context.Context, client *http.Client, target Peer) Record {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	r := Record{Target: target.Target, Result: RequestFailed}
	var frontendSend atomic.Int64
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) {
		// Exclude connection setup, including TLS, but precede request writes.
		frontendSend.Store(time.Now().UnixNano())
	}})
	// Startup validated the origin; the method and non-nil context are fixed.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.Origin+"/clock", nil)
	resp, err := client.Do(req)
	if err != nil {
		r.FrontendSend = frontendSend.Load()
		r.FrontendReceive = time.Now().UnixNano()
		return r
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	r.FrontendSend = frontendSend.Load()
	r.FrontendReceive = time.Now().UnixNano()
	if err != nil || resp.StatusCode != http.StatusOK {
		return r
	}
	r.Result = InvalidSample
	var sample Sample
	if r.FrontendSend == 0 || len(body) > maxBodyBytes || json.Unmarshal(body, &sample) != nil || sample.ReceivedAt.IsZero() || sample.SentAt.IsZero() {
		return r
	}
	received, sent := sample.ReceivedAt.UnixNano(), sample.SentAt.UnixNano()
	if received < 0 || sent < 0 || !time.Unix(0, received).Equal(sample.ReceivedAt) || !time.Unix(0, sent).Equal(sample.SentAt) {
		return r
	}
	r.HostReceive, r.HostSend = received, sent
	if r.HostSend < r.HostReceive || r.FrontendReceive < r.FrontendSend || r.HostSend-r.HostReceive > r.FrontendReceive-r.FrontendSend {
		return r
	}
	r.Result = Success
	return r
}
