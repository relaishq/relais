package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/pion/logging"
	"github.com/relais/pkg/mediaworker"
)

// maxMoveLog bounds the moves the demo remembers per call.
const maxMoveLog = 100

// system is the demo's media side: two media workers, "A" and "B", sharing
// one UDP socket. Calls start on A. A call can move to the other worker
// mid-call (a planned handover) with POST /calls/{id}/move; the browser
// keeps the same address, keys and connection throughout.
type system struct {
	socket  *mediaworker.Socket
	workers []namedWorker

	mu    sync.Mutex
	moves map[string][]moveEntry // by session ID
}

type namedWorker struct {
	name   string
	worker *mediaworker.Worker
}

// moveEntry is one move as the server saw it. Durations are milliseconds.
type moveEntry struct {
	N           int     `json:"n"`
	At          string  `json:"at"`
	From        string  `json:"from"`
	To          string  `json:"to"`
	OK          bool    `json:"ok"`
	Error       string  `json:"error,omitempty"`
	DurationMs  float64 `json:"durationMs"`
	DrainMs     float64 `json:"drainMs"`
	ExportMs    float64 `json:"exportMs"`
	ResumeMs    float64 `json:"resumeMs"`
	StateBytes  int     `json:"stateBytes"`
	HeldPackets int     `json:"heldPackets"`
	// RolledBack: the new owner could not resume the call, so the old owner
	// resumed it; the call goes on where it was.
	RolledBack bool `json:"rolledBack,omitempty"`
}

// callStatus is GET /calls/{id}.
type callStatus struct {
	SessionID string `json:"sessionId"`
	Owner     string `json:"owner"`
	// DecryptFailures counts the browser's SRTP/SRTCP packets the current
	// owner could not decrypt since it took the call.
	DecryptFailures uint64      `json:"ownerDecryptFailures"`
	Moves           []moveEntry `json:"moves"`
}

func startSystem(listenAddr string, loggerFactory logging.LoggerFactory) (*system, error) {
	socket, err := mediaworker.ListenSocket(mediaworker.SocketConfig{ListenAddr: listenAddr, LoggerFactory: loggerFactory})
	if err != nil {
		return nil, err
	}
	sys := &system{socket: socket, moves: make(map[string][]moveEntry)}
	for _, name := range []string{"A", "B"} {
		worker, err := socket.NewWorker(mediaworker.Config{LoggerFactory: loggerFactory})
		if err != nil {
			return nil, errors.Join(err, sys.close())
		}
		sys.workers = append(sys.workers, namedWorker{name: name, worker: worker})
	}

	return sys, nil
}

// MediaAddr is the shared socket's address, the host candidate in every
// answer.
func (s *system) MediaAddr() string {
	return s.socket.LocalAddr().String()
}

func (s *system) close() error {
	var errs []error
	for _, w := range s.workers {
		errs = append(errs, w.worker.Close())
	}

	return errors.Join(append(errs, s.socket.Close())...)
}

// register adds the call endpoints:
//
//	POST   /calls            SDP offer -> answer; the call starts on worker A
//	DELETE /calls/{id}       hangs up on whichever worker owns the call
//	POST   /calls/{id}/move  moves the call to the other worker
//	GET    /calls/{id}       current owner and the move log
func (s *system) register(mux *http.ServeMux) {
	signaling := s.socket.SignalingHandler(s.workers[0].worker)
	mux.Handle("POST "+mediaworker.CallsPath, signaling)
	mux.HandleFunc("DELETE "+mediaworker.CallsPath+"/{id}", func(rw http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		delete(s.moves, r.PathValue("id"))
		s.mu.Unlock()
		signaling.ServeHTTP(rw, r)
	})
	mux.HandleFunc("POST "+mediaworker.CallsPath+"/{id}/move", s.handleMove)
	mux.HandleFunc("GET "+mediaworker.CallsPath+"/{id}", s.handleStatus)
}

func (s *system) handleMove(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	from := s.nameOf(s.socket.Owner(id))
	if from == "" {
		http.Error(rw, "unknown call", http.StatusNotFound)

		return
	}
	to := s.workers[0]
	if from == to.name {
		to = s.workers[1]
	}

	start := time.Now()
	result, err := s.socket.Handover(id, to.worker, mediaworker.ResumeOptions{})
	entry := moveEntry{
		At:          start.UTC().Format(time.RFC3339Nano),
		From:        from,
		To:          to.name,
		OK:          err == nil,
		DurationMs:  millis(result.Duration),
		DrainMs:     millis(result.Drain),
		ExportMs:    millis(result.Export),
		ResumeMs:    millis(result.Resume),
		StateBytes:  result.StateBytes,
		HeldPackets: result.HeldPackets,
		RolledBack:  result.RolledBack,
	}
	if err != nil {
		entry.Error = err.Error()
		log.Printf("echo-demo: move call %s from %s to %s FAILED: %v", id, from, to.name, err)
	} else {
		log.Printf("echo-demo: moved call %s from %s to %s in %.2f ms (state %d B, %d packets held)",
			id, from, to.name, entry.DurationMs, entry.StateBytes, entry.HeldPackets)
	}

	s.mu.Lock()
	entry.N = len(s.moves[id]) + 1
	s.moves[id] = append(s.moves[id], entry)
	if n := len(s.moves[id]); n > maxMoveLog {
		s.moves[id] = s.moves[id][n-maxMoveLog:]
	}
	s.mu.Unlock()

	status := http.StatusOK
	if err != nil {
		status = http.StatusConflict
	}
	writeJSON(rw, status, entry)
}

func (s *system) handleStatus(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	owner := s.socket.Owner(id)
	if owner == nil {
		http.Error(rw, "unknown call", http.StatusNotFound)

		return
	}
	failures, _ := owner.SessionDecryptFailures(id)

	s.mu.Lock()
	moves := append([]moveEntry{}, s.moves[id]...)
	s.mu.Unlock()

	writeJSON(rw, http.StatusOK, callStatus{SessionID: id, Owner: s.nameOf(owner), DecryptFailures: failures, Moves: moves})
}

func (s *system) nameOf(worker *mediaworker.Worker) string {
	for _, w := range s.workers {
		if w.worker == worker {
			return w.name
		}
	}

	return ""
}

func writeJSON(rw http.ResponseWriter, status int, value any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(status)
	if err := json.NewEncoder(rw).Encode(value); err != nil {
		log.Printf("echo-demo: write JSON: %v", err)
	}
}

func millis(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
