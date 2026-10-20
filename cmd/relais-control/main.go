package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/relaishq/relais/internal/processrun"
	"github.com/relaishq/relais/pkg/buildinfo"
	"github.com/relaishq/relais/pkg/controlplane"
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
	var testWorkers testWorkerAllowlist
	flag.Var(&testWorkers, "test-register-worker", "enable private test registration for IP=HTTP-first-last,UDP-first-last (repeat per host)")
	var registered workers
	flag.Var(&registered, "worker", "name=http://worker-address (repeat for each worker)")
	workerCallCap := flag.Int("worker-call-cap", 0, "hard calls plus incoming reservations per worker (0 = unlimited)")
	version := buildinfo.VersionFlag(flag.CommandLine)
	flag.Parse()
	if *version {
		return buildinfo.PrintVersion(os.Stdout, "relais-control")
	}
	if *relayURL == "" || len(registered) < 1 {
		return errors.New("control requires -relay and at least one -worker")
	}
	if *workerCallCap < 0 {
		return errors.New("worker-call-cap must be nonnegative")
	}
	fixed, err := parseWorkers(registered)
	if err != nil {
		return err
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
	plane := controlplane.NewWithConfig(r, store, controlplane.Config{FrameCache: frames, StoreOutageBudget: storeConfig.OutageBudget, WorkerCallCap: *workerCallCap})
	names := make([]string, 0, len(fixed))
	for _, entry := range fixed {
		names = append(names, entry.name)
	}
	plane.BeginBootstrap(names)
	if err := plane.EnableRestart(); err != nil {
		return err
	}
	// Serve heartbeats during registration. A slow later worker must not make
	// an earlier healthy registration age past DeadAfter before Run starts.
	handler := controlHandler(plane, r, *demoRegister, testWorkers, testWorkerClient())
	var handlers sync.WaitGroup
	tracked := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		handler.ServeHTTP(w, request)
	})
	served := make(chan struct{})
	serveErr := make(chan error, 1)
	go func() { defer close(served); serveErr <- processrun.Serve(ctx, listener, tracked) }()
	defer func() { cancel(); <-served }()
	managed := make([]controlplane.FixedWorker, 0, len(fixed))
	for _, worker := range fixed {
		managed = append(managed, controlplane.FixedWorker{Name: worker.name, URL: worker.endpoint})
	}
	done := make(chan struct{})
	managedErr := make(chan error, 1)
	managedCtx, stopManaged := context.WithCancel(context.WithoutCancel(ctx))
	go func() { defer close(done); managedErr <- plane.RunManaged(managedCtx, managed) }()
	defer func() {
		plane.BeginShutdown()
		cancel()
		<-served
		handlers.Wait()
		stopManaged()
		<-done
		releaseCtx, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = plane.ReleaseControl(releaseCtx)
	}()
	firstRelay := make(chan struct{})
	registrationDone := make(chan struct{})
	go func() { defer close(registrationDone); registerRestartedRelay(ctx, plane, r, firstRelay) }()
	defer func() { cancel(); <-registrationDone }()
	select {
	case <-firstRelay:
	case <-ctx.Done():
	}
	// Listening is available even when another control holds the lease or a
	// dependency is down. GET /status is authoritative for mutation readiness.
	startup := time.NewTimer(1200 * time.Millisecond)
	defer startup.Stop()
awaitReady:
	for {
		if state, statusErr := plane.Status(ctx); statusErr == nil && state.Ready {
			break
		}
		select {
		case <-ctx.Done():
			break awaitReady
		case <-startup.C:
			break awaitReady
		case <-time.After(10 * time.Millisecond):
		}
	}
	processrun.Ready(map[string]any{"http": "http://" + listener.Addr().String()})
	select {
	case <-ctx.Done():
	case err = <-serveErr:
	case err = <-managedErr:
	}
	plane.BeginShutdown()
	cancel()
	<-served
	handlers.Wait()
	stopManaged()
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
func registerRestartedRelay(ctx context.Context, plane *controlplane.Plane, r *controlplane.RemoteRelay, first chan<- struct{}) {
	var registry controlplane.RelayWorkerRegistry
	healthy := false
	lastInstance, lastRegistered := "", 0
	for attempt, delay := 1, 100*time.Millisecond; ; attempt++ {
		if ctx.Err() != nil {
			return
		}
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		err := registry.Sync(bounded, plane, r)
		cancel()
		instance, registered := registry.Instance(), registry.Registered()
		if err != nil || !healthy || instance != lastInstance || registered != lastRegistered {
			log.Printf("relay bootstrap attempt=%d instance=%s workers=%d result=%s", attempt, instance, registered, bootstrapResult(err))
		}
		healthy, lastInstance, lastRegistered = err == nil, instance, registered
		if attempt == 1 {
			close(first)
		}
		if err != nil {
			delay = min(delay*2, 5*time.Second)
		} else {
			delay = 100 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-plane.AuthorityChanges():
			timer.Stop()
			delay = 100 * time.Millisecond
		case <-timer.C:
		}
	}
}
