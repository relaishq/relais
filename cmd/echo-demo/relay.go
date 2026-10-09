package main

import (
	"errors"
	"log"
	"net/http"
	"net/netip"

	"github.com/pion/logging"

	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
)

// mediaSystem is the demo's media side: by default two workers sharing one
// socket, between which a call can move (system, handover.go); with -relay,
// one worker behind a relay (relayed).
type mediaSystem interface {
	register(mux *http.ServeMux)
	close() error
}

// startMedia starts the media side on addr and logs what runs where.
func startMedia(addr netip.AddrPort, withRelay bool, loggerFactory logging.LoggerFactory) (mediaSystem, error) {
	if withRelay {
		return startRelayed(addr, loggerFactory)
	}
	sys, err := startSystem(addr.String(), loggerFactory)
	if err != nil {
		return nil, err
	}
	log.Printf("echo-demo: media workers A and B on udp %s (the host candidate in every answer)", sys.MediaAddr())

	return sys, nil
}

// relayed is the -relay media side: a relay owns the media address, and one
// media worker binds only a private loopback socket behind it, with an
// in-memory session-owner store between them. Moving a call between workers
// through the relay is later work, so POST /calls/{id}/move answers 501.
type relayed struct {
	relay  *relay.Relay
	worker *mediaworker.Worker
}

func startRelayed(addr netip.AddrPort, loggerFactory logging.LoggerFactory) (*relayed, error) {
	owners := sessionstore.NewMemory()
	r, err := relay.New(relay.Config{
		PublicAddr:    addr.String(),
		WorkerAddr:    "127.0.0.1:0",
		Owners:        owners,
		LoggerFactory: loggerFactory,
	})
	if err != nil {
		return nil, err
	}
	worker, err := mediaworker.New(mediaworker.Config{
		ListenAddr:    "127.0.0.1:0",
		LoggerFactory: loggerFactory,
		Relay: &mediaworker.RelayConfig{
			Addr:       r.WorkerAddr(),
			PublicAddr: r.PublicAddr(),
			Owners:     owners,
		},
	})
	if err != nil {
		return nil, errors.Join(err, r.Close())
	}
	r.AddWorker(worker.LocalAddr())
	log.Printf("echo-demo: relay on udp %s (the host candidate in every answer), relay leg on udp %s",
		r.PublicAddr(), r.WorkerAddr())
	log.Printf("echo-demo: media worker behind the relay on private udp %s", worker.LocalAddr())

	return &relayed{relay: r, worker: worker}, nil
}

// register adds the call endpoints:
//
//	POST   /calls            SDP offer -> answer
//	DELETE /calls/{id}       hangs up
//	POST   /calls/{id}/move  501: not supported through the relay yet
func (s *relayed) register(mux *http.ServeMux) {
	signaling := s.worker.SignalingHandler()
	mux.Handle("POST "+mediaworker.CallsPath, signaling)
	mux.Handle("DELETE "+mediaworker.CallsPath+"/{id}", signaling)
	mux.HandleFunc("POST "+mediaworker.CallsPath+"/{id}/move", func(rw http.ResponseWriter, _ *http.Request) {
		writeJSON(rw, http.StatusNotImplemented, moveEntry{
			Error: "moving a call between workers through the relay is not supported yet; run the demo without -relay",
		})
	})
}

// close hangs up every call, then stops the relay.
func (s *relayed) close() error {
	return errors.Join(s.worker.Close(), s.relay.Close())
}
