// Package movediag records opt-in move diagnostics on disposable process runs.
// It records metadata only: never URLs, session IDs, Redis arguments or bodies.
package movediag

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"
)

const Endpoint = "/private/move-diagnostics"
const traceHeader = "X-Relais-Move-Trace"
const phaseHeader = "X-Relais-Move-Phase"
const limit = 8192

type key struct{}
type scope struct{ ID, Phase string }

type Event struct {
	TraceID           string        `json:"trace_id"`
	Phase             string        `json:"phase"`
	Kind              string        `json:"kind"`
	Operation         string        `json:"operation"`
	Process           string        `json:"process,omitempty"`
	Start             time.Time     `json:"start"`
	End               time.Time     `json:"end"`
	Duration          time.Duration `json:"duration_ns"`
	RequestBytes      int           `json:"request_payload_bytes,omitempty"`
	ResponseBytes     int           `json:"response_payload_bytes,omitempty"`
	Connections       int           `json:"connections,omitempty"`
	ReusedConnections int           `json:"reused_connections,omitempty"`
	Failed            bool          `json:"failed,omitempty"`
}
type Snapshot struct {
	Events  []Event `json:"events"`
	Dropped int     `json:"dropped"`
}
type Recorder struct {
	mu       sync.Mutex
	snapshot Snapshot
}

var Default Recorder

func Enabled() bool                 { return os.Getenv("RELAIS_MOVE_DIAGNOSTICS") == "1" }
func ID(ctx context.Context) string { s, _ := ctx.Value(key{}).(scope); return s.ID }
func Phase(ctx context.Context, phase string) context.Context {
	s, _ := ctx.Value(key{}).(scope)
	if s.ID == "" {
		return ctx
	}
	s.Phase = phase
	return context.WithValue(ctx, key{}, s)
}

// Copy retains the destination's cancellation/deadline. Diagnostics must not
// change existing context-free RPC or detached recovery semantics.
func Copy(dst, src context.Context) context.Context {
	if src == nil {
		return dst
	}
	s, _ := src.Value(key{}).(scope)
	if s.ID == "" {
		return dst
	}
	return context.WithValue(dst, key{}, s)
}
func Begin(ctx context.Context) context.Context {
	if !Enabled() {
		return ctx
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return ctx
	}
	return context.WithValue(ctx, key{}, scope{hex.EncodeToString(token[:]), "prepare"})
}
func NewEvent(ctx context.Context, kind, operation string) Event {
	s, _ := ctx.Value(key{}).(scope)
	if s.ID == "" {
		return Event{}
	}
	return Event{TraceID: s.ID, Phase: s.Phase, Kind: kind, Operation: operation, Start: time.Now()}
}
func (r *Recorder) Finish(e Event, failed bool) {
	if e.TraceID == "" {
		return
	}
	e.End = time.Now()
	e.Duration = e.End.Sub(e.Start)
	e.Failed = failed
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.snapshot.Events) == limit {
		r.snapshot.Dropped++
		return
	}
	r.snapshot.Events = append(r.snapshot.Events, e)
}
func (r *Recorder) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Snapshot{Events: append([]Event(nil), r.snapshot.Events...), Dropped: r.snapshot.Dropped}
}
func Inject(req *http.Request) {
	s, _ := req.Context().Value(key{}).(scope)
	if s.ID != "" {
		req.Header.Set(traceHeader, s.ID)
		req.Header.Set(phaseHeader, s.Phase)
	}
}
func validPhase(s string) bool {
	switch s {
	case "prepare", "barrier", "export", "transfer", "resume", "release", "recovery":
		return true
	}
	return false
}

// Handler is installed only on the process binaries' existing private listener.
// Collection happens after the measured move; recording does no file/network I/O.
func (rec *Recorder) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == Endpoint {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(rec.Snapshot())
			return
		}
		id, phase := r.Header.Get(traceHeader), r.Header.Get(phaseHeader)
		if token, err := hex.DecodeString(id); err == nil && len(token) == 16 && validPhase(phase) {
			r = r.WithContext(context.WithValue(r.Context(), key{}, scope{id, phase}))
		}
		next.ServeHTTP(w, r)
	})
}
func Handler(next http.Handler) http.Handler {
	if !Enabled() {
		return next
	}
	return Default.Handler(next)
}
