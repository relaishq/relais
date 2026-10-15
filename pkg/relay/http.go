package relay

import (
	"net/http"
	"net/netip"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/metrics"
	"github.com/relaishq/relais/pkg/sessionstore"
)

var RemoteErrors = map[string]error{
	"barrier_timeout": ErrBarrierTimeout, "held": ErrHeld,
	"hold_limit": ErrHoldLimit, "hold_expired": ErrHoldExpired,
	"buffer_disabled": ErrBufferDisabled, "replay_in_progress": ErrReplayInProgress,
	"replay_not_ready": ErrReplayNotReady, "replay_target_changed": ErrReplayTargetChanged,
}

type RouteRequest struct {
	Epoch  uint64         `json:"epoch,omitempty"`
	Repair bool           `json:"repair,omitempty"`
	From   netip.AddrPort `json:"from"`
	To     netip.AddrPort `json:"to"`
}
type WorkerRequest struct {
	Address netip.AddrPort `json:"address"`
}

// ReleaseReply reports a hold release. Packets counts held arrivals. A
// release after an exact export also reports Recovered retained input the
// export had not consumed, Filtered held media it had, whether the ring
// could cover the receive window, and how long the release held forwardMu.
type ReleaseReply struct {
	Packets   int           `json:"packets"`
	Recovered int           `json:"recovered,omitempty"`
	Filtered  int           `json:"filtered,omitempty"`
	Complete  bool          `json:"complete,omitempty"`
	LockHeld  time.Duration `json:"lock_held_ns,omitempty"`
}

// ExportReleaseRequest releases a planned hold after an exact export.
type ExportReleaseRequest struct {
	To    netip.AddrPort           `json:"to"`
	Input map[uint32]ConsumedInput `json:"input"`
}
type ReplayRequest struct {
	From    netip.AddrPort    `json:"from"`
	To      netip.AddrPort    `json:"to"`
	Inbound map[uint32]uint64 `json:"inbound"`
}

// ContinuityStatus records acquisition, fencing and socket startup for this
// instance. Durations are measured in the relay process, not inferred by callers.
type ContinuityStatus struct {
	Epoch           uint64        `json:"epoch"`
	ClaimAt         time.Time     `json:"claim_at"`
	Wait            time.Duration `json:"wait_ns"`
	Fence           time.Duration `json:"fence_ns"`
	Activate        time.Duration `json:"activate_ns"`
	BindRestore     time.Duration `json:"bind_restore_ns"`
	BindWait        time.Duration `json:"bind_wait_ns"`
	BindFailures    uint64        `json:"bind_failures"`
	FencingFailures uint64        `json:"fencing_failures"`
	StoreFailures   uint64        `json:"store_failures"`
}
type RelayStatus struct {
	Continuity *ContinuityStatus `json:"continuity,omitempty"`
	Instance   string            `json:"instance"`
	Public     netip.AddrPort    `json:"public"`
	Private    netip.AddrPort    `json:"private"`
	Stats      Stats             `json:"stats"`
}

// PrivateHandler controls a relay on a trusted loopback HTTP listener.
func (r *Relay) PrivateHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.ProcessHandler(r.metricSamples))
	mux.HandleFunc("POST /sessions/{id}/begin-replay", func(w http.ResponseWriter, req *http.Request) {
		var body ReplayRequest
		if !privateapi.Read(w, req, &body) {
			return
		}
		plan, err := r.BeginReplay(req.Context(), req.PathValue("id"), body.From, body.Inbound)
		if err != nil {
			privateapi.Error(w, err, RemoteErrors)
			return
		}
		privateapi.Write(w, plan)
	})
	mux.HandleFunc("POST /sessions/{id}/replay", func(w http.ResponseWriter, req *http.Request) {
		var body ReplayRequest
		if !privateapi.Read(w, req, &body) {
			return
		}
		result, err := r.ReplaySession(req.Context(), req.PathValue("id"), body.To)
		if err != nil {
			privateapi.Error(w, err, RemoteErrors)
			return
		}
		privateapi.Write(w, result)
	})
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
		if err := r.MoveSessionAtEpoch(req.PathValue("id"), body.From, body.To, body.Epoch); err != nil {
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
	mux.HandleFunc("POST /sessions/{id}/move-after-export", func(w http.ResponseWriter, req *http.Request) {
		var body RouteRequest
		if !privateapi.Read(w, req, &body) {
			return
		}
		if !body.From.IsValid() {
			http.Error(w, "from is required", http.StatusBadRequest)
			return
		}
		if err := r.MoveSessionAfterExport(req.PathValue("id"), body.From, body.To, body.Epoch); err != nil {
			privateapi.Error(w, err, RemoteErrors)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /sessions/{id}/release-after-export", func(w http.ResponseWriter, req *http.Request) {
		var body ExportReleaseRequest
		if !privateapi.Read(w, req, &body) {
			return
		}
		if !body.To.IsValid() {
			http.Error(w, "to is required", http.StatusBadRequest)
			return
		}
		reply, err := r.ReleaseAfterExport(req.PathValue("id"), body.To, body.Input)
		if err != nil {
			privateapi.Error(w, err, RemoteErrors)
			return
		}
		privateapi.Write(w, reply)
	})
	mux.HandleFunc("DELETE /sessions/{id}", func(w http.ResponseWriter, req *http.Request) {
		r.ForgetSession(req.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /sessions/{id}/forget-tenure", func(w http.ResponseWriter, req *http.Request) {
		var lease sessionstore.Lease
		if !privateapi.Read(w, req, &lease) {
			return
		}
		if lease.SessionID != req.PathValue("id") {
			http.Error(w, "session mismatch", http.StatusBadRequest)
			return
		}
		if err := r.ForgetSessionTenure(req.Context(), lease); err != nil {
			privateapi.Error(w, err, RemoteErrors)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		privateapi.Write(w, RelayStatus{Continuity: r.cfg.Continuity, Instance: r.instance, Public: r.PublicAddr(), Private: r.WorkerAddr(), Stats: r.Stats()})
	})
	return mux
}
