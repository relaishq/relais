package main

import (
	"errors"
	"log"
	"net/netip"

	"github.com/pion/logging"

	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
)

// media is the media path the page calls: a media worker, alone on the
// media address or behind a relay that owns it.
type media struct {
	worker *mediaworker.Worker
	relay  *relay.Relay // nil without -relay
}

// startMedia starts the media path on addr. Without a relay the worker's
// own socket is on addr. With one, the relay owns addr, and the worker binds
// only a private loopback socket behind it, with an in-memory session-owner
// store between them.
func startMedia(addr netip.AddrPort, withRelay bool, loggerFactory logging.LoggerFactory) (*media, error) {
	if !withRelay {
		worker, err := mediaworker.New(mediaworker.Config{ListenAddr: addr.String(), LoggerFactory: loggerFactory})
		if err != nil {
			return nil, err
		}
		log.Printf("echo-demo: media worker on udp %s (the host candidate in every answer)", worker.MediaAddr())

		return &media{worker: worker}, nil
	}

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

	return &media{worker: worker, relay: r}, nil
}

// Close hangs up every call, then stops the relay.
func (m *media) Close() error {
	err := m.worker.Close()
	if m.relay != nil {
		err = errors.Join(err, m.relay.Close())
	}

	return err
}
