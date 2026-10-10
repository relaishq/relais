package main

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/controlplane"
)

// demoHandler is opt-in bootstrap for fresh replacement processes. It does
// not replace registrations, revive drained workers, or change lease policy.
func demoHandler(plane *controlplane.Plane, relay *controlplane.RemoteRelay) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", plane.ProcessHandler())
	var mu sync.Mutex
	mux.HandleFunc("POST /demo/register", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}
		if !privateapi.Read(w, r, &req) {
			return
		}
		u, err := url.Parse(req.URL)
		if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || req.Name == "" {
			http.Error(w, "name and literal loopback worker URL required", http.StatusBadRequest)
			return
		}
		host, _, err := net.SplitHostPort(u.Host)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() {
			http.Error(w, "worker URL must be loopback", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		worker := &controlplane.RemoteWorker{URL: req.URL}
		status, err := worker.Status(r.Context())
		if err == nil {
			err = relay.AddWorker(r.Context(), status.Address)
		}
		if err == nil {
			err = plane.Register(req.Name, status.Address, worker)
		}
		if err != nil {
			privateapi.Error(w, errors.New("demo registration: "+err.Error()), nil)
			return
		}
		privateapi.Write(w, map[string]any{"name": req.Name, "address": status.Address})
	})
	return mux
}
