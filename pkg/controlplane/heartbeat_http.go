package controlplane

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/netip"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/metrics"
)

type HeartbeatRequest struct {
	Address netip.AddrPort `json:"address"`
	Token   string         `json:"token,omitempty"`
}
type HeartbeatReply struct {
	Token string `json:"rejoin_token,omitempty"`
}

// remoteHeartbeat never accepts an ordinary heartbeat as a rejoin ACK.
// A token belongs to one registration and one death/recovery cycle. Recovery
// must settle first, then the worker must fence/drop before echoing the token.
func (p *Plane) remoteHeartbeat(name string, req HeartbeatRequest) (HeartbeatReply, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.workers[name]
	if w == nil || w.addr != req.Address {
		return HeartbeatReply{}, errors.New("controlplane: unknown worker heartbeat")
	}
	if w.recovering || w.dead && !w.recovered {
		return HeartbeatReply{}, errors.New("controlplane: worker takeover in progress")
	}
	// Rebuild restored this endpoint and the store remains the only
	// ownership authority. Accept its prior completed heartbeat token once; it
	// belongs to the worker process, while the old controller's cache is gone.
	if p.restart != nil && !w.dead && !w.heartbeatObserved {
		w.acceptedToken = req.Token
		w.heartbeatObserved = true
	}
	if w.dead {
		if w.rejoinToken == "" {
			var token [16]byte
			if _, err := rand.Read(token[:]); err != nil {
				return HeartbeatReply{}, err
			}
			w.rejoinToken = hex.EncodeToString(token[:])
		}
		if req.Token != w.rejoinToken {
			return HeartbeatReply{Token: w.rejoinToken}, nil
		}
		w.dead = false
		w.acceptedToken = w.rejoinToken
		w.rejoinToken = ""
	} else if req.Token != "" && req.Token != w.acceptedToken {
		return HeartbeatReply{}, errors.New("controlplane: stale rejoin token")
	}
	if w.livenessUnknown {
		w.livenessUnknown = false
		p.loadVersion++
	}
	w.lastHeartbeat = time.Now()
	return HeartbeatReply{}, nil
}

// ProcessHandler serves WHIP and the private heartbeat API. Bind it only on
// trusted loopback: these prototype control endpoints have no authentication.
func (p *Plane) ProcessHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.ProcessHandler(p.metricSamples, metrics.CheckpointCollectors()...))
	mux.Handle("/", p.Handler())
	mux.HandleFunc("POST /private/workers/{name}/mark-draining", func(w http.ResponseWriter, r *http.Request) {
		if err := p.MarkDraining(r.PathValue("name")); err != nil {
			if errors.Is(err, ErrNotReady) {
				writeError(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			privateapi.Write(w, privateapi.Failure{Code: "not_found", Message: err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// Reset only draining on the settled dead registration. Keeping it dead
	// preserves the remote rejoin token handshake and fencing of old sessions.
	mux.HandleFunc("POST /private/workers/{name}/replace", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		p.mu.Lock()
		exists := p.workers[name] != nil
		p.mu.Unlock()
		if !exists {
			http.Error(w, "unknown worker", http.StatusNotFound)
			return
		}
		if err := p.resetRecoveredDrain(name); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		privateapi.Write(w, struct{}{})
	})
	mux.HandleFunc("POST /private/workers/{name}/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var req HeartbeatRequest
		if !privateapi.Read(w, r, &req) {
			return
		}
		reply, err := p.remoteHeartbeat(r.PathValue("name"), req)
		if err != nil {
			privateapi.Error(w, err, nil)
			return
		}
		privateapi.Write(w, reply)
	})
	return mux
}
