package controlplane

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/netip"
	"time"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/metrics"
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
	w.lastHeartbeat = time.Now()
	return HeartbeatReply{}, nil
}

// ProcessHandler serves WHIP and the private heartbeat API. Bind it only on
// trusted loopback: these prototype control endpoints have no authentication.
func (p *Plane) ProcessHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.ProcessHandler(p.metricSamples))
	mux.Handle("/", p.Handler())
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
