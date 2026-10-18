package voice

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// RequestTrace contains numeric request metadata, never request or response data.
// Its JSON fields are the entire log allow-list; observer timestamps are excluded.
// Zero spans mean the hook did not run. BodyRead includes consumption until EOF
// or Close, including an early close. Think is WroteRequest to first response byte.
type RequestTrace struct {
	// StartedAt and FinishedAt are process-local trace boundaries for observers.
	// They are never logged or persisted and must not be compared across workers.
	StartedAt  time.Time     `json:"-"`
	FinishedAt time.Time     `json:"-"`
	Turn       agent.Turn    `json:"turn"`
	Stage      string        `json:"stage"`
	DNS        time.Duration `json:"dns_ns"`
	Connect    time.Duration `json:"connect_ns"`
	TLS        time.Duration `json:"tls_ns"`
	Reused     bool          `json:"reused"`
	Idle       bool          `json:"idle"`
	IdleTime   time.Duration `json:"idle_ns"`
	Think      time.Duration `json:"think_ns"`
	BodyRead   time.Duration `json:"body_read_ns"`
	Protocol   string        `json:"protocol"`
	Status     int           `json:"status"`
}

func (r RequestTrace) Line() string { b, _ := json.Marshal(r); return "voice_request " + string(b) }

type requestIdentity struct {
	turn  agent.Turn
	stage string
}
type requestIdentityKey struct{}

func speechIndex(step string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(step, "speech-"))
	return n
}
func requestContext(ctx context.Context, turn agent.Turn, stage string) context.Context {
	safe := "unknown"
	switch stage {
	case "transcribe", "reply", "warm", "warm-transcribe", "warm-reply", "warm-speech":
		safe = stage
	default:
		for i := 0; i < MaxSegments; i++ {
			if stage == "speech-"+strconv.Itoa(i) {
				safe = stage
				break
			}
		}
	}
	return context.WithValue(ctx, requestIdentityKey{}, requestIdentity{turn, safe})
}

type requestTrace struct {
	mu                             sync.Mutex
	row                            RequestTrace
	dns, tls, written, first, body time.Time
	connects                       map[string]time.Time
	once                           sync.Once
	report                         func(RequestTrace)
}

func (p *OpenAI) traceRequest(req *http.Request) (*http.Request, *requestTrace) {
	identity, _ := req.Context().Value(requestIdentityKey{}).(requestIdentity)
	if identity.stage == "" {
		identity.stage = "unknown"
	}
	r := &requestTrace{connects: make(map[string]time.Time), row: RequestTrace{Turn: identity.turn, Stage: identity.stage, StartedAt: time.Now()}, report: func(row RequestTrace) {
		log.Print(row.Line())
		if p.Trace != nil {
			p.Trace(row)
		}
	}}
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { r.mu.Lock(); defer r.mu.Unlock(); r.dns = time.Now() },
		DNSDone: func(httptrace.DNSDoneInfo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if !r.dns.IsZero() {
				r.row.DNS += time.Since(r.dns)
			}
		},
		ConnectStart: func(network, address string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.connects[network+"/"+address] = time.Now()
		},
		ConnectDone: func(network, address string, _ error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if at, ok := r.connects[network+"/"+address]; ok {
				r.row.Connect += time.Since(at)
				delete(r.connects, network+"/"+address)
			}
		},
		TLSHandshakeStart: func() { r.mu.Lock(); defer r.mu.Unlock(); r.tls = time.Now() },
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if !r.tls.IsZero() {
				r.row.TLS += time.Since(r.tls)
			}
		},
		GotConn: func(i httptrace.GotConnInfo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.row.Reused, r.row.Idle, r.row.IdleTime = i.Reused, i.WasIdle, i.IdleTime
		},
		WroteRequest: func(httptrace.WroteRequestInfo) { r.mu.Lock(); defer r.mu.Unlock(); r.written = time.Now() },
		GotFirstResponseByte: func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.first = time.Now()
			if !r.written.IsZero() {
				r.row.Think = max(0, r.first.Sub(r.written))
			}
		},
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace)), r
}
func (r *requestTrace) finish() {
	r.once.Do(func() {
		r.mu.Lock()
		r.row.FinishedAt = time.Now()
		if !r.body.IsZero() {
			r.row.BodyRead = r.row.FinishedAt.Sub(r.body)
		}
		row := r.row
		r.mu.Unlock()
		r.report(row)
	})
}
func (r *requestTrace) response(response *http.Response) io.ReadCloser {
	r.mu.Lock()
	r.row.Status = response.StatusCode
	switch response.ProtoMajor {
	case 1:
		r.row.Protocol = "h1"
	case 2:
		r.row.Protocol = "h2"
	}
	r.body = time.Now()
	r.mu.Unlock()
	return &tracedBody{ReadCloser: response.Body, trace: r}
}

type tracedBody struct {
	io.ReadCloser
	trace *requestTrace
}

func (b *tracedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.trace.finish()
	}
	return n, err
}
func (b *tracedBody) Close() error { err := b.ReadCloser.Close(); b.trace.finish(); return err }
