package mediaworker

import (
	"net/http"
	"net/netip"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/sessionstore"
)

// RemoteErrors identifies coordination errors across the private HTTP boundary.
var RemoteErrors = map[string]error{
	"unknown_session": ErrUnknownSession, "unsupported_offer": ErrUnsupportedOffer,
	"closed": ErrClosed, "not_established": ErrNotEstablished,
	"sequence_budget": ErrSequenceBudgetExhausted, "session_exists": errSessionExists,
	"srtcp_exhausted": ErrSRTCPIndexExhausted,
}

type CreateRequest struct {
	Offer string `json:"offer"`
}
type CreateReply struct {
	ID     string `json:"id"`
	Answer string `json:"answer"`
}
type ExportReply struct {
	State []byte `json:"state"`
}

// ResumeRequest deliberately excludes Context; the server uses the request's
// deadline and the client honors ResumeOptions.Context.
type ResumeRequest struct {
	State            []byte             `json:"state"`
	Lease            sessionstore.Lease `json:"lease"`
	SequenceMargin   uint16             `json:"sequence_margin"`
	SRTCPIndexMargin uint32             `json:"srtcp_index_margin"`
}
type ResumeReply struct {
	ID string `json:"id"`
}
type WorkerStatus struct {
	Address  netip.AddrPort `json:"address"`
	Sessions int            `json:"sessions"`
	Replay   ReplayStats    `json:"replay"`
}

// PrivateHandler exposes the existing worker operations on trusted loopback.
// Session snapshots contain key material: this API must stay private.
func (w *Worker) PrivateHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", func(rw http.ResponseWriter, r *http.Request) {
		var req CreateRequest
		if !privateapi.Read(rw, r, &req) {
			return
		}
		id, answer, err := w.CreateSession(r.Context(), req.Offer)
		if err != nil {
			privateapi.Error(rw, err, RemoteErrors)
			return
		}
		privateapi.Write(rw, CreateReply{ID: id, Answer: answer})
	})
	mux.HandleFunc("DELETE /sessions/{id}", func(rw http.ResponseWriter, r *http.Request) {
		if err := w.EndSession(r.PathValue("id")); err != nil {
			privateapi.Error(rw, err, RemoteErrors)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /sessions/{id}/export", func(rw http.ResponseWriter, r *http.Request) {
		state, err := w.ExportSession(r.PathValue("id"))
		if err != nil {
			privateapi.Error(rw, err, RemoteErrors)
			return
		}
		privateapi.Write(rw, ExportReply{State: state})
	})
	mux.HandleFunc("POST /resume", func(rw http.ResponseWriter, r *http.Request) {
		var req ResumeRequest
		if !privateapi.Read(rw, r, &req) {
			return
		}
		id, err := w.ResumeSession(req.State, ResumeOptions{Context: r.Context(), Lease: req.Lease, SequenceMargin: req.SequenceMargin, SRTCPIndexMargin: req.SRTCPIndexMargin})
		if err != nil {
			privateapi.Error(rw, err, RemoteErrors)
			return
		}
		privateapi.Write(rw, ResumeReply{ID: id})
	})
	mux.HandleFunc("GET /status", func(rw http.ResponseWriter, _ *http.Request) {
		privateapi.Write(rw, WorkerStatus{Address: w.LocalAddr(), Sessions: w.SessionCount(), Replay: w.ReplayStats()})
	})
	return mux
}

// SessionCount is a synchronized local observation used during process drain.
func (w *Worker) SessionCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.sessions)
}
