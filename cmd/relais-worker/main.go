package main

import (
	"context"
	"flag"
	"log"
	"net/netip"
	"time"

	"github.com/relais/internal/processrun"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/framecache"
	"github.com/relais/pkg/mediaworker"
)

func run() error {
	var storeConfig processrun.StoreConfig
	storeConfig.Flags(flag.CommandLine)
	name := flag.String("name", processrun.Env("RELAIS_WORKER_NAME", ""), "unique worker name")
	listen := flag.String("media", processrun.Env("RELAIS_WORKER_MEDIA", "127.0.0.1:0"), "private UDP address")
	leg := flag.String("relay-leg", processrun.Env("RELAIS_RELAY_LEG", ""), "relay private UDP address")
	public := flag.String("relay-media", processrun.Env("RELAIS_RELAY_MEDIA", ""), "relay advertised public UDP address")
	control := flag.String("control", processrun.Env("RELAIS_CONTROL_URL", ""), "control-plane HTTP URL for heartbeats")
	httpAddr := flag.String("http", processrun.Env("RELAIS_WORKER_HTTP", "127.0.0.1:0"), "private worker HTTP address")
	cacheOff := flag.Bool("frame-cache-off", false, "disable cache writes and takeover replay (PLI remains enabled)")
	agentName := flag.String("agent", "echo", "audio agent: echo or demo")
	drainTimeout := flag.Duration("drain-timeout", 5*time.Second, "SIGTERM drain deadline before closing")
	flag.Parse()
	if *drainTimeout <= 0 {
		return &configError{}
	}
	agentConfig, err := selectAgent(*agentName)
	if err != nil {
		return err
	}
	ctx, cancel := processrun.Context()
	defer cancel()
	legAddr, err := netip.ParseAddrPort(*leg)
	if err != nil {
		return err
	}
	mediaAddr, err := netip.ParseAddrPort(*public)
	if err != nil {
		return err
	}
	if *name == "" || *control == "" {
		return &configError{}
	}
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
	var frames framecache.Store
	if !*cacheOff {
		client, err := storeConfig.OpenFrames(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		frames = client
	}
	worker, err := mediaworker.New(mediaworker.Config{Agent: agentConfig, ListenAddr: *listen, FrameCache: frames, DisableFrameCache: *cacheOff, Relay: &mediaworker.RelayConfig{Addr: legAddr, PublicAddr: mediaAddr, Owners: store}})
	if err != nil {
		return err
	}
	defer func() { _ = worker.Close() }()
	processrun.Ready(map[string]any{"http": "http://" + listener.Addr().String(), "media": worker.LocalAddr(), "name": *name})
	worker.StartHeartbeats(&controlplane.RemoteHeartbeats{URL: *control, Name: *name})
	// Keep this HTTP API and heartbeats alive while the control plane exports
	// sessions. Shutdown of the listener comes after drain, then Close.
	serving, stopHTTP := context.WithCancel(context.Background())
	defer stopHTTP()
	done := make(chan error, 1)
	go func() { done <- processrun.Serve(serving, listener, agentHandler(worker, *agentName)) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	drainCtx, stopDrain := context.WithTimeout(context.Background(), *drainTimeout)
	if err := drainWorker(drainCtx, *control, *name, worker.SessionCount); err != nil {
		log.Printf("worker %s: drain failed; closing remaining sessions: %v", *name, err)
	}
	stopDrain()
	stopHTTP()
	return <-done
}

type configError struct{}

func (*configError) Error() string { return "worker requires -name and -control" }
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
