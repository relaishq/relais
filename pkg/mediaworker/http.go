package mediaworker

import (
	"errors"
	"net/http"
	"net/netip"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/agent"
	agentv2 "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/metrics"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// RemoteErrors identifies coordination errors across the private HTTP boundary.
var RemoteErrors = map[string]error{
	"control_invalid": agentv2.ErrControlInvalid, "control_conflict": agentv2.ErrControlConflict,
	"control_order":     agentv2.ErrControlOrder,
	"control_not_ready": agentv2.ErrControlNotReady,
	"control_rejected":  agentv2.ErrControlRejected,
	"control_stale":     agentv2.ErrControlStale, "control_unsupported": agentv2.ErrControlUnsupported,
	"unknown_session": ErrUnknownSession, "unsupported_offer": ErrUnsupportedOffer,
	"worker_draining": ErrDraining,
	"closed":          ErrClosed, "not_established": ErrNotEstablished,
	"sequence_budget": ErrSequenceBudgetExhausted, "session_exists": errSessionExists,
	"srtcp_exhausted":     ErrSRTCPIndexExhausted,
	"dtls_exhausted":      ErrDTLSSequenceExhausted,
	"checkpoint_clock":    sessionstore.ErrUnsafeCheckpointClock,
	"agent_incompatible":  agentv2.ErrIncompatibleAgent,
	"agent_capacity":      agentv2.ErrCapacity,
	"agent_backpressure":  agentv2.ErrBackpressure,
	"uncertain":           privateapi.ErrUncertain,
	"export_incompatible": ErrExportIncompatible,
	"move_budget":         ErrMoveBudget, "export_not_flushed": ErrExportNotFlushed,
	"agent_version": agent.ErrVersion, "agent_oversize": agent.ErrStateTooLarge, "agent_save": agent.ErrSave, "agent_restore": agent.ErrRestore,
}

type CreateRequest struct {
	Offer string `json:"offer"`
}
type CreateReply struct {
	Lease  sessionstore.Lease `json:"lease"`
	ID     string             `json:"id"`
	Answer string             `json:"answer"`
}
type ExportReply struct {
	State []byte `json:"state"`
}

// ResumeRequest deliberately excludes Context; the server uses the request's
// deadline and the client honors ResumeOptions.Context.
type ResumeRequest struct {
	Kind                  agent.ResumeKind        `json:"resume_kind"`
	InputMayBeDuplicated  bool                    `json:"input_may_be_duplicated"`
	DuplicateWindows      []agent.DuplicateWindow `json:"duplicate_windows"`
	CallerSequenceReserve uint32                  `json:"caller_sequence_reserve"`
	SnapshotAge           time.Duration           `json:"snapshot_age"`
	CheckpointStoredAt    time.Time               `json:"checkpoint_stored_at"`
	CheckpointAge         time.Duration           `json:"checkpoint_age"`
	State                 []byte                  `json:"state"`
	Lease                 sessionstore.Lease      `json:"lease"`
	SequenceMargin        uint16                  `json:"sequence_margin"`
	SRTCPIndexMargin      uint32                  `json:"srtcp_index_margin"`
	RelayReplay           bool                    `json:"relay_replay"`
	RelayInputReplay      bool                    `json:"relay_input_replay,omitempty"`
}
type ResumeReply struct {
	ID string `json:"id"`
}
type WorkerStatus struct {
	Address      netip.AddrPort      `json:"address"`
	Sessions     int                 `json:"sessions"`
	Replay       ReplayStats         `json:"replay"`
	Agents       AgentStats          `json:"agents"`
	AgentSession *AgentSessionStatus `json:"agent_session,omitempty"`
}

// PrivateHandler exposes the existing worker operations on trusted loopback.
// Session snapshots contain key material: this API must stay private.
func (w *Worker) PrivateHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.ProcessHandler(w.metricSamples, metrics.CheckpointCollectors()...))
	mux.HandleFunc("POST /sessions", func(rw http.ResponseWriter, r *http.Request) {
		var req CreateRequest
		if !privateapi.Read(rw, r, &req) {
			return
		}
		id, answer, lease, err := w.CreateSessionWithLease(r.Context(), req.Offer)
		if err != nil {
			workerAPIError(rw, err)
			return
		}
		privateapi.Write(rw, CreateReply{ID: id, Answer: answer, Lease: lease})
	})
	mux.HandleFunc("DELETE /sessions/{id}", func(rw http.ResponseWriter, r *http.Request) {
		if err := w.EndSession(r.PathValue("id")); err != nil {
			workerAPIError(rw, err)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /sessions/{id}/export", func(rw http.ResponseWriter, r *http.Request) {
		state, release, err := w.exportSession(r.PathValue("id"))
		defer release()
		if err != nil {
			workerAPIError(rw, err)
			return
		}
		counted := &exportResponseWriter{ResponseWriter: rw}
		privateapi.Write(counted, ExportReply{State: state})
		for old := w.metrics.exportHTTPPeakBytes.Load(); uint64(counted.bytes) > old; old = w.metrics.exportHTTPPeakBytes.Load() {
			if w.metrics.exportHTTPPeakBytes.CompareAndSwap(old, uint64(counted.bytes)) {
				break
			}
		}
	})
	mux.HandleFunc("POST /sessions/{id}/agent/control", func(rw http.ResponseWriter, r *http.Request) {
		var req agentv2.ControlMessage
		if !privateapi.Read(rw, r, &req) {
			return
		}
		receipt, err := w.AgentControl(r.Context(), r.PathValue("id"), req)
		if err != nil {
			workerAPIError(rw, err)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		if !receipt.Checkpointed {
			rw.WriteHeader(http.StatusAccepted)
		}
		privateapi.Write(rw, receipt)
	})
	mux.HandleFunc("POST /sessions/{id}/keyframe", func(rw http.ResponseWriter, r *http.Request) {
		if err := w.RequestKeyframe(r.Context(), r.PathValue("id")); err != nil {
			privateapi.Error(rw, err, RemoteErrors)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /resume", func(rw http.ResponseWriter, r *http.Request) {
		var req ResumeRequest
		if !privateapi.Read(rw, r, &req) {
			return
		}
		id, err := w.ResumeSession(req.State, ResumeOptions{Kind: req.Kind, InputMayBeDuplicated: req.InputMayBeDuplicated, DuplicateWindows: req.DuplicateWindows, CallerSequenceReserve: req.CallerSequenceReserve, CheckpointAge: req.CheckpointAge, SnapshotAge: req.SnapshotAge, CheckpointStoredAt: req.CheckpointStoredAt, Context: r.Context(), Lease: req.Lease, SequenceMargin: req.SequenceMargin, SRTCPIndexMargin: req.SRTCPIndexMargin, RelayReplay: req.RelayReplay, RelayInputReplay: req.RelayInputReplay})
		if err != nil {
			workerAPIError(rw, err)
			return
		}
		privateapi.Write(rw, ResumeReply{ID: id})
	})
	mux.HandleFunc("GET /status", func(rw http.ResponseWriter, r *http.Request) {
		status := WorkerStatus{Address: w.LocalAddr(), Sessions: w.SessionCount(), Replay: w.ReplayStats(), Agents: w.AgentStats()}
		if id := r.URL.Query().Get("agent-session"); id != "" {
			var err error
			status.AgentSession, err = w.agentSessionStatus(id)
			if err != nil {
				workerAPIError(rw, err)
				return
			}
		}
		privateapi.Write(rw, status)
	})
	return mux
}

// SessionCount is a synchronized local observation used during process drain.
func (w *Worker) SessionCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.sessions)
}

// Agent errors can wrap more than one sentinel. Choose one code explicitly;
// map iteration must not turn an unknown version into a generic restore error.
func workerAPIError(rw http.ResponseWriter, err error) {
	if errors.Is(err, errDTLSCheckpointEpoch) {
		privateapi.Error(rw, ErrNotEstablished, map[string]error{"not_established": ErrNotEstablished})
		return
	}
	if errors.Is(err, privateapi.ErrUncertain) {
		privateapi.Error(rw, err, map[string]error{"uncertain": privateapi.ErrUncertain})
		return
	}
	if errors.Is(err, ErrExportIncompatible) {
		privateapi.Error(rw, err, map[string]error{"export_incompatible": ErrExportIncompatible})
		return
	}
	if errors.Is(err, ErrExportNotFlushed) {
		privateapi.Error(rw, err, map[string]error{"export_not_flushed": ErrExportNotFlushed})
		return
	}
	if errors.Is(err, ErrMoveBudget) {
		privateapi.Error(rw, err, map[string]error{"move_budget": ErrMoveBudget})
		return
	}
	for _, code := range []string{"agent_incompatible", "agent_version", "agent_oversize", "agent_save", "agent_restore"} {
		if sentinel := RemoteErrors[code]; errors.Is(err, sentinel) {
			privateapi.Error(rw, err, map[string]error{code: sentinel})
			return
		}
	}
	privateapi.Error(rw, err, RemoteErrors)
}

// exportResponseWriter observes the response body without retaining key material.
type exportResponseWriter struct {
	http.ResponseWriter
	bytes int
}

func (w *exportResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}
