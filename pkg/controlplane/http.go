package controlplane

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"mime"
	"net/http"

	"github.com/relaishq/relais/internal/privateapi"
	agentv2 "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// DrainResult preserves every per-call outcome even if part of a drain fails.
// Error summarizes the failures; successful moves remain available to callers.
type DrainResult struct {
	Moves []MoveResult `json:"moves"`
	Error string       `json:"error,omitempty"`
}

// RemoteErrors identifies move failures across the operator HTTP boundary.
// Common store/context causes and transport uncertainty are handled by privateapi.
var RemoteErrors = func() map[string]error {
	codes := maps.Clone(mediaworker.RemoteErrors)
	maps.Copy(codes, relay.RemoteErrors)
	maps.Copy(codes, map[string]error{
		"unknown_call": ErrUnknownCall, "move_in_progress": ErrMoveInProgress, "no_target": ErrNoTarget,
		"ended": sessionstore.ErrEnded,
	})
	return codes
}()

// MoveFailure retains the planned outcome alongside the request failure.
type MoveFailure struct {
	Error  string     `json:"error"`
	Codes  []string   `json:"codes"`
	Result MoveResult `json:"result"`
}

// Handler is the local WHIP-style signaling and coordination endpoint.
// Worker names are accepted as the optional worker query parameter on offers
// and the to query parameter on moves. An omitted name uses load balancing.
func (p *Plane) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /calls", func(w http.ResponseWriter, r *http.Request) {
		ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || ct != "application/sdp" {
			http.Error(w, "Content-Type must be application/sdp", http.StatusUnsupportedMediaType)
			return
		}

		offer, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			http.Error(w, "cannot read offer", http.StatusBadRequest)
			return
		}

		id, answer, err := p.Create(r.Context(), string(offer), r.URL.Query().Get("worker"))
		if err != nil {
			writePlacementError(w, err)
			return
		}

		w.Header().Set("Content-Type", "application/sdp")
		w.Header().Set("Location", "/calls/"+id)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, answer)
	})
	mux.HandleFunc("DELETE /calls/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := p.End(r.Context(), r.PathValue("id")); err != nil {
			writeError(w, err)
			return
		}

		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /calls/{id}/agent/control", func(w http.ResponseWriter, r *http.Request) {
		var req agentv2.ControlMessage
		if !privateapi.Read(w, r, &req) {
			return
		}
		receipt, err := p.AgentControl(r.Context(), r.PathValue("id"), req)
		if err != nil {
			privateapi.Error(w, err, RemoteErrors)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if !receipt.Checkpointed {
			w.WriteHeader(http.StatusAccepted)
		}
		writeJSON(w, receipt)
	})
	mux.HandleFunc("POST /calls/{id}/move", func(w http.ResponseWriter, r *http.Request) {
		result, err := p.Move(r.Context(), r.PathValue("id"), r.URL.Query().Get("to"))
		if err != nil {
			if result.ID != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(errorStatus(err))
				writeJSON(w, MoveFailure{Error: err.Error(), Codes: privateapi.ErrorCodes(err, RemoteErrors), Result: result})
				return
			}
			writeError(w, err)
			return
		}
		writeJSON(w, result)
	})
	mux.HandleFunc("POST /workers/{name}/drain", func(w http.ResponseWriter, r *http.Request) {
		result, err := p.Drain(r.Context(), r.PathValue("name"))
		if err != nil && len(result) == 0 {
			writeError(w, err)
			return
		}

		reply := DrainResult{Moves: result}
		if err != nil {
			reply.Error = err.Error()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMultiStatus)
		}
		writeJSON(w, reply)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		status, err := p.Status(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, status)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func writePlacementError(w http.ResponseWriter, err error) {
	if errors.Is(err, mediaworker.ErrDraining) || errors.Is(err, ErrNoTarget) {
		code := "no_target"
		if errors.Is(err, mediaworker.ErrDraining) {
			code = "worker_draining"
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		}{code, "worker unavailable; retry placement", true})
		return
	}
	writeError(w, err)
}

func writeError(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), errorStatus(err))
}

func errorStatus(err error) int {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrUnknownCall), errors.Is(err, sessionstore.ErrNotFound), errors.Is(err, mediaworker.ErrUnknownSession):
		code = http.StatusNotFound
	case errors.Is(err, ErrMoveInProgress), errors.Is(err, ErrNoTarget), errors.Is(err, sessionstore.ErrLeaseLost):
		code = http.StatusConflict
	case errors.Is(err, mediaworker.ErrUnsupportedOffer):
		code = http.StatusBadRequest
	}
	return code
}
