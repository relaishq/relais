package main

import (
	"errors"
	"flag"
	"log"
	"strings"

	"github.com/relais/internal/processrun"
	"github.com/relais/pkg/controlplane"
)

type workers []string

func (w *workers) String() string     { return strings.Join(*w, ",") }
func (w *workers) Set(s string) error { *w = append(*w, s); return nil }
func run() error {
	var storeConfig processrun.StoreConfig
	storeConfig.Flags(flag.CommandLine)
	relayURL := flag.String("relay", processrun.Env("RELAIS_RELAY_URL", ""), "relay control HTTP URL")
	httpAddr := flag.String("http", processrun.Env("RELAIS_CONTROL_HTTP", "127.0.0.1:0"), "WHIP and private heartbeat HTTP address")
	demoRegister := flag.Bool("demo-register", false, "enable loopback demo replacement registration")
	var registered workers
	flag.Var(&registered, "worker", "name=http://worker-address (repeat for each worker)")
	flag.Parse()
	if *relayURL == "" || len(registered) < 1 {
		return errors.New("control requires -relay and at least one -worker")
	}
	ctx, cancel := processrun.Context()
	defer cancel()
	store, err := storeConfig.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	r := &controlplane.RemoteRelay{URL: *relayURL}
	plane := controlplane.New(r, store)
	// Serve heartbeats during registration. A slow later worker must not make
	// an earlier healthy registration age past DeadAfter before Run starts.
	listener, err := processrun.ListenPrivate(*httpAddr)
	if err != nil {
		return err
	}
	handler := plane.ProcessHandler()
	if *demoRegister {
		handler = demoHandler(plane, r)
	}
	served := make(chan struct{})
	serveErr := make(chan error, 1)
	go func() { defer close(served); serveErr <- processrun.Serve(ctx, listener, handler) }()
	defer func() { cancel(); <-served }()
	for _, entry := range registered {
		name, endpoint, ok := strings.Cut(entry, "=")
		if !ok {
			return errors.New("worker must be name=http://address")
		}
		worker := &controlplane.RemoteWorker{URL: endpoint}
		status, err := worker.Status(ctx)
		if err != nil {
			return err
		}
		if err := r.AddWorker(ctx, status.Address); err != nil {
			return err
		}
		if err := plane.Register(name, status.Address, worker); err != nil {
			return err
		}
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = plane.Run(ctx) }()
	processrun.Ready(map[string]any{"http": "http://" + listener.Addr().String()})
	select {
	case <-ctx.Done():
	case err = <-serveErr:
	}
	cancel()
	<-done
	return err
}
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
