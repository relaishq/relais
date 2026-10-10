package main

import (
	"flag"
	"log"
	"time"

	"github.com/relais/internal/processrun"
	"github.com/relais/pkg/relay"
)

func run() error {
	var storeConfig processrun.StoreConfig
	storeConfig.Flags(flag.CommandLine)
	public := flag.String("media", processrun.Env("RELAIS_RELAY_MEDIA", "127.0.0.1:0"), "public UDP address")
	leg := flag.String("leg", processrun.Env("RELAIS_RELAY_LEG", "127.0.0.1:0"), "private worker-leg UDP address")
	httpAddr := flag.String("http", processrun.Env("RELAIS_RELAY_HTTP", "127.0.0.1:0"), "private control HTTP address")
	hold := flag.Duration("hold-timeout", 3*time.Second, "abandoned move backstop")
	restoreOff := flag.Bool("route-restore-off", false, "disable persisted-route restore for baseline measurement")
	restoreTimeout := flag.Duration("route-restore-timeout", 5*time.Second, "startup route restore budget; continue with partial results on expiry")
	maxFlows := flag.Int("max-flows", 65536, "maximum confirmed and pending caller routes")
	flag.Parse()
	ctx, cancel := processrun.Context()
	defer cancel()
	store, err := storeConfig.OpenOwners(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	r, err := relay.New(relay.Config{PublicAddr: *public, WorkerAddr: *leg, Owners: store, Routes: store, DisableRouteRestore: *restoreOff, RouteRestoreTimeout: *restoreTimeout, MaxFlows: *maxFlows, HoldTimeout: *hold})
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	listener, err := processrun.ListenPrivate(*httpAddr)
	if err != nil {
		return err
	}
	processrun.Ready(map[string]any{"http": "http://" + listener.Addr().String(), "media": r.PublicAddr(), "leg": r.WorkerAddr()})
	return processrun.Serve(ctx, listener, r.PrivateHandler())
}
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
