package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"strings"
	"time"

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
	listener, err := processrun.ListenPrivate(*httpAddr)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	store, err := storeConfig.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	r := &controlplane.RemoteRelay{URL: *relayURL}
	frames, err := storeConfig.OpenFrames(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = frames.Close() }()
	plane := controlplane.NewWithConfig(r, store, controlplane.Config{FrameCache: frames})
	// Serve heartbeats during registration. A slow later worker must not make
	// an earlier healthy registration age past DeadAfter before Run starts.
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
	registrationDone := make(chan struct{})
	go func() { defer close(registrationDone); registerRestartedRelay(ctx, plane, r) }()
	defer func() { cancel(); <-registrationDone }()
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

// A new relay process loses its private-leg registry. Re-register live workers
// on each tick, including late registrations and rejoins in the same instance.
func registerRestartedRelay(ctx context.Context, plane *controlplane.Plane, r *controlplane.RemoteRelay) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var registry controlplane.RelayWorkerRegistry
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			attempt, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			if err := registry.Sync(attempt, plane, r); err != nil {
				log.Printf("relay worker registration: %v", err)
			}
			cancel()
		}
	}
}
