package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/controlplane"
)

type workerProcess struct {
	child *clusterprocess.Child
	URL   string
}
type demo struct {
	ctx                        context.Context
	client                     *http.Client
	control, relay, redis, dir string
	agentName                  string
	publicHost                 string     // exact opt-in page IP; empty means loopback only
	launchToken                string     // per-launch LAN mutation capability
	op                         sync.Mutex // serialize actions, including recycled workers
	mu                         sync.Mutex // status can poll during recovery
	workers                    map[string]*workerProcess
	next                       int
	registrationError          string
	unconfirmed                map[string]string
	spawn                      func(context.Context, string) (*workerProcess, error)
}

var errRegistrationUncertain = errors.New("replacement registration unverified")

type demoStatus struct {
	controlplane.Status
	SelectedAgent     string         `json:"selected_agent"`
	RegistrationError string         `json:"registration_error,omitempty"`
	PoolSize          int            `json:"pool_size"`
	ExpectedPoolSize  int            `json:"expected_pool_size"`
	WorkerPIDs        map[string]int `json:"worker_pids"`
	Relay             string         `json:"relay"`
	Redis             string         `json:"redis"`
}

func (d *demo) status(ctx context.Context) (demoStatus, error) {
	var status demoStatus
	err := privateapi.Do(ctx, d.client, d.control, http.MethodGet, "/status", nil, &status.Status, nil)
	if err != nil {
		return status, err
	}
	status.Relay, status.Redis = d.relay, d.redis
	status.SelectedAgent = d.agentName
	if status.SelectedAgent == "" {
		status.SelectedAgent = "echo"
	}
	status.WorkerPIDs = map[string]int{}
	d.mu.Lock()
	defer d.mu.Unlock()
	for name, worker := range d.workers {
		select {
		case <-worker.child.Done():
			continue
		default:
			status.WorkerPIDs[name] = worker.child.PID()
		}
	}
	// Fresh names cannot refer to an earlier worker. A later successful status
	// read can resolve registration uncertainty without stopping a live target.
	for _, w := range status.Workers {
		delete(d.unconfirmed, w.Name)
	}
	warnings := make([]string, 0, len(d.unconfirmed))
	for _, warning := range d.unconfirmed {
		warnings = append(warnings, warning)
	}
	sort.Strings(warnings)
	d.registrationError = strings.Join(warnings, "; ")
	status.RegistrationError = d.registrationError
	status.PoolSize, status.ExpectedPoolSize = len(status.WorkerPIDs), 3
	return status, nil
}
func (d *demo) owner(ctx context.Context, id string) (controlplane.CallStatus, error) {
	status, err := d.status(ctx)
	if err != nil {
		return controlplane.CallStatus{}, err
	}
	if id == "" && len(status.Calls) == 1 {
		return status.Calls[0], nil
	}
	for _, call := range status.Calls {
		if call.ID == id {
			return call, nil
		}
	}
	return controlplane.CallStatus{}, errors.New("specify the id of a live call")
}
func (d *demo) replacement(ctx context.Context, old string) (string, int, error) {
	var name string
	var pid int
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		name, pid, err = d.replaceOnce(ctx, old)
		if err == nil || ctx.Err() != nil || errors.Is(err, errRegistrationUncertain) {
			break
		}
	}
	return name, pid, err
}
func (d *demo) replaceOnce(ctx context.Context, old string) (string, int, error) {
	name := fmt.Sprintf("%d", d.next)
	d.next++
	worker, err := d.spawn(ctx, name)
	if err != nil {
		return name, 0, err
	}
	err = privateapi.Do(ctx, d.client, d.control, http.MethodPost, "/demo/register", map[string]string{"name": name, "url": worker.URL}, nil, nil)
	if err != nil {
		// A lost reply is not proof that registration failed. Confirm with a
		// bounded read even if the action request's deadline has just expired.
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		var status controlplane.Status
		checkErr := privateapi.Do(checkCtx, d.client, d.control, http.MethodGet, "/status", nil, &status, nil)
		cancel()
		registered := false
		for _, w := range status.Workers {
			if w.Name == name {
				registered = true
				break
			}
		}
		if checkErr != nil {
			// Keep the process owned and suppress another spawn while the outcome
			// is unknown; stopping it could create a live phantom registration.
			d.mu.Lock()
			delete(d.workers, old)
			d.workers[name] = worker
			if d.unconfirmed == nil {
				d.unconfirmed = map[string]string{}
			}
			d.unconfirmed[name] = fmt.Sprintf("%s: registration reply: %v; confirmation: %v", name, err, checkErr)
			d.registrationError = d.unconfirmed[name]
			d.mu.Unlock()
			return name, worker.child.PID(), fmt.Errorf("%w: %v", errRegistrationUncertain, checkErr)
		}
		if !registered {
			worker.child.Stop()
			return name, 0, err
		}
	}

	d.mu.Lock()
	delete(d.workers, old)
	d.workers[name] = worker
	d.mu.Unlock()
	return name, worker.child.PID(), nil
}

type actionResult struct {
	Agent          *agentContinuity            `json:"agent_continuity,omitempty"`
	Continuities   map[string]*agentContinuity `json:"-"`
	Kind           string                      `json:"kind"`
	ID             string                      `json:"id"`
	From           string                      `json:"from"`
	To             string                      `json:"to"`
	PID            int                         `json:"pid"`
	Replacement    string                      `json:"replacement,omitempty"`
	ReplacementPID int                         `json:"replacement_pid,omitempty"`
	At             time.Time                   `json:"at"`
	Drain          *controlplane.DrainResult   `json:"drain,omitempty"`
	Error          string                      `json:"error,omitempty"`
}

// Kill waits for the real detector/takeover before adding a fresh target. It
// never tells the control plane a worker died or changes crash recovery.
func (d *demo) kill(ctx context.Context, id string) (actionResult, error) {
	owner, err := d.owner(ctx, id)
	if err != nil {
		return actionResult{}, err
	}
	d.mu.Lock()
	worker := d.workers[owner.Owner]
	d.mu.Unlock()
	if worker == nil {
		return actionResult{}, errors.New("owner is not a launcher-owned process")
	}
	continuity := d.beforeAgent(ctx, owner.ID, owner.Owner)
	result := actionResult{Agent: continuity, Kind: "kill", ID: owner.ID, From: owner.Owner, PID: worker.child.PID(), At: time.Now()}
	if err := worker.child.SignalGroup(syscall.SIGKILL); err != nil {
		return result, err
	}
	// Reap before replacement and leave enough time for the existing detector.
	select {
	case <-worker.child.Done():
	case <-ctx.Done():
		return result, ctx.Err()
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := d.status(ctx)
		if err != nil {
			return result, err
		}
		recovered := false
		for _, w := range status.Workers {
			if w.Name == owner.Owner {
				recovered = w.Dead && !w.Recovering
			}
		}
		for _, call := range status.Calls {
			if call.ID == owner.ID && call.Owner != owner.Owner {
				result.To = call.Owner
			}
		}
		if recovered {
			break
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-ticker.C:
		}
	}
	d.finishAgent(ctx, owner.ID, result.From, result.To, "kill", continuity)
	worker.child.Stop() // closes the killed child's log without signalling a reaped PID
	result.Replacement, result.ReplacementPID, err = d.replacement(ctx, owner.Owner)
	if err != nil {
		return result, err
	}
	if result.To == "" {
		return result, errors.New("call was lost during takeover")
	}
	return result, nil
}
func (d *demo) drain(ctx context.Context, name string) (actionResult, error) {
	d.mu.Lock()
	worker := d.workers[name]
	d.mu.Unlock()
	if worker == nil {
		return actionResult{}, errors.New("worker is not a launcher-owned process")
	}
	continuities := map[string]*agentContinuity{}
	if d.agentName == "demo" {
		status, err := d.status(ctx)
		if err != nil {
			return actionResult{}, err
		}
		for _, call := range status.Calls {
			if call.Owner == name {
				continuities[call.ID] = d.beforeAgent(ctx, call.ID, name)
			}
		}
	}
	result := actionResult{Continuities: continuities, Kind: "drain", From: name, PID: worker.child.PID(), At: time.Now()}
	var reply controlplane.DrainResult
	err := privateapi.Do(ctx, d.client, d.control, http.MethodPost, "/workers/"+url.PathEscape(name)+"/drain", nil, &reply, nil)
	result.Drain = &reply
	if err != nil {
		return result, err
	}
	if reply.Error != "" {
		return result, errors.New(reply.Error)
	}
	if len(reply.Moves) > 0 {
		result.ID, result.To = reply.Moves[0].ID, reply.Moves[0].To
	}
	for _, move := range reply.Moves {
		d.finishAgent(ctx, move.ID, move.From, move.To, "drain", continuities[move.ID])
	}
	result.Agent = continuities[result.ID]
	worker.child.Stop()
	result.Replacement, result.ReplacementPID, err = d.replacement(ctx, name)
	return result, err
}
func (d *demo) handler(files http.Handler) http.Handler {
	target, _ := url.Parse(d.control)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{ResponseHeaderTimeout: 10 * time.Second}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, err.Error(), http.StatusBadGateway)
	}
	mux := http.NewServeMux()
	mux.Handle("/", files)
	mux.Handle("POST /calls", proxy)
	// The Pion external harness reads /status next to its signaling URL.
	mux.Handle("GET /status", proxy)
	mux.Handle("DELETE /calls/{id}", proxy)
	mux.HandleFunc("POST /calls/{id}/move", func(w http.ResponseWriter, r *http.Request) {
		d.op.Lock()
		defer d.op.Unlock()
		if d.agentName == "demo" {
			d.moveAgent(w, r)
		} else {
			proxy.ServeHTTP(w, r)
		}
	})
	mux.HandleFunc("GET /demo/agent/{id}", func(w http.ResponseWriter, r *http.Request) {
		if d.agentName != "demo" {
			http.Error(w, "demo agent not selected", http.StatusNotFound)
			return
		}
		owner, err := d.owner(r.Context(), r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		status, err := d.readAgent(r.Context(), owner.ID, owner.Owner)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		privateapi.Write(w, status)
	})
	mux.HandleFunc("GET /demo/status", func(w http.ResponseWriter, r *http.Request) {
		status, err := d.status(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		privateapi.Write(w, status)
	})
	action := func(kind string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				ID string `json:"id"`
			}
			if r.ContentLength != 0 && !privateapi.Read(w, r, &req) {
				return
			}
			d.op.Lock()
			defer d.op.Unlock()
			// Launcher lifetime, not a disconnected browser, owns replacement work.
			ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
			defer cancel()
			var result actionResult
			var err error
			if kind == "kill" {
				result, err = d.kill(ctx, req.ID)
			} else {
				var owner controlplane.CallStatus
				owner, err = d.owner(ctx, req.ID)
				if err == nil {
					result, err = d.drain(ctx, owner.Owner)
					if result.Drain != nil {
						for _, move := range result.Drain.Moves {
							if move.ID == owner.ID {
								result.ID, result.To = move.ID, move.To
								result.Agent = result.Continuities[move.ID]
								break
							}
						}
					}
				}
			}
			if err != nil {
				result.Error = err.Error()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
			}
			privateapi.Write(w, result)
		}
	}
	mux.HandleFunc("POST /demo/kill", action("kill"))
	mux.HandleFunc("POST /demo/drain", action("drain"))
	mux.HandleFunc("POST /workers/{name}/drain", func(w http.ResponseWriter, r *http.Request) {
		d.op.Lock()
		defer d.op.Unlock()
		ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
		defer cancel()
		result, err := d.drain(ctx, r.PathValue("name"))
		if err != nil {
			result.Error = err.Error()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
		}
		privateapi.Write(w, result)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Refuse DNS rebinding even for read-only pages and matching Origins.
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		allowed := host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]"
		if d.publicHost != "" {
			ip, err := netip.ParseAddr(host)
			selected, selectedErr := netip.ParseAddr(d.publicHost)
			allowed = err == nil && selectedErr == nil && ip.Is4() && ip == selected
		}
		if !allowed {
			http.Error(w, "demo Host refused", http.StatusForbidden)
			return
		}
		if d.publicHost != "" && r.Method != http.MethodGet && (d.launchToken == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Relais-Demo-Token")), []byte(d.launchToken)) != 1) {
			http.Error(w, "demo launch token required", http.StatusForbidden)
			return
		}
		// Block web pages on other origins from controlling local child processes.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			if origin := r.Header.Get("Origin"); origin != "" && origin != scheme+"://"+r.Host {
				http.Error(w, "cross-origin demo action refused", http.StatusForbidden)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
