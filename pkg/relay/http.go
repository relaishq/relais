package relay

import (
	"net/http"
	"net/netip"

	"github.com/relais/internal/privateapi"
)

var RemoteErrors = map[string]error{
	"barrier_timeout": ErrBarrierTimeout, "held": ErrHeld,
	"hold_limit": ErrHoldLimit, "hold_expired": ErrHoldExpired,
}

type RouteRequest struct {
	Repair bool           `json:"repair,omitempty"`
	From   netip.AddrPort `json:"from"`
	To     netip.AddrPort `json:"to"`
}
type WorkerRequest struct {
	Address netip.AddrPort `json:"address"`
}
type ReleaseReply struct {
	Packets int `json:"packets"`
}
type RelayStatus struct {
	Instance string         `json:"instance"`
	Public   netip.AddrPort `json:"public"`
	Private  netip.AddrPort `json:"private"`
	Stats    Stats          `json:"stats"`
}

// PrivateHandler controls a relay on a trusted loopback HTTP listener.
func (r *Relay) PrivateHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /workers", func(w http.ResponseWriter, req *http.Request) {
		var body WorkerRequest
		if !privateapi.Read(w, req, &body) {
			return
		}
		if !body.Address.IsValid() {
			http.Error(w, "invalid worker address", http.StatusBadRequest)
			return
		}
		r.AddWorker(body.Address)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /workers", func(w http.ResponseWriter, req *http.Request) {
		var body WorkerRequest
		if !privateapi.Read(w, req, &body) {
			return
		}
		if !body.Address.IsValid() {
			http.Error(w, "invalid worker address", http.StatusBadRequest)
			return
		}
		r.RemoveWorker(body.Address)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /sessions/{id}/hold", func(w http.ResponseWriter, req *http.Request) {
		var body RouteRequest
		if !privateapi.Read(w, req, &body) {
			return
		}
		if err := r.HoldSession(req.Context(), req.PathValue("id"), body.From); err != nil {
			privateapi.Error(w, err, RemoteErrors)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /sessions/{id}/move", func(w http.ResponseWriter, req *http.Request) {
		var body RouteRequest
		if !privateapi.Read(w, req, &body) {
			return
		}
		if !body.From.IsValid() && !body.Repair {
			http.Error(w, "from is required unless repair is explicit", http.StatusBadRequest)
			return
		}
		if err := r.MoveSession(req.PathValue("id"), body.From, body.To); err != nil {
			privateapi.Error(w, err, RemoteErrors)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /sessions/{id}/release", func(w http.ResponseWriter, req *http.Request) {
		var body RouteRequest
		if !privateapi.Read(w, req, &body) {
			return
		}
		count, err := r.ReleaseSession(req.PathValue("id"), body.To)
		if err != nil {
			privateapi.Error(w, err, RemoteErrors)
			return
		}
		privateapi.Write(w, ReleaseReply{Packets: count})
	})
	mux.HandleFunc("DELETE /sessions/{id}", func(w http.ResponseWriter, req *http.Request) {
		r.ForgetSession(req.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		privateapi.Write(w, RelayStatus{Instance: r.instance, Public: r.PublicAddr(), Private: r.WorkerAddr(), Stats: r.Stats()})
	})
	return mux
}
