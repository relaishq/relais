package workerapp

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/relaishq/relais/internal/processrun"
	"github.com/relaishq/relais/internal/udpbuffer"
	"github.com/relaishq/relais/pkg/buildinfo"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/framecache"
	"github.com/relaishq/relais/pkg/mediaworker"
)

// Options registers agents and selects the default for new calls. Restore uses
// the persisted name; every worker in a pool must use the same registrations.
type Options struct {
	Agents       []Registration
	DefaultAgent string
}

// Main runs the worker with os.Args[1:], handles interrupt and SIGTERM signals,
// and drains within -drain-timeout before closing. It reports errors and exits
// with status 1 for startup/runtime errors or 2 for invalid flags. Help and
// version requests exit successfully.
func Main(options Options) {
	ctx, cancel := processrun.Context()
	defer cancel()
	if err := Run(ctx, os.Args[1:], options); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		var parseErr *flagError
		if errors.As(err, &parseErr) {
			os.Exit(2) // FlagSet has already reported the error and usage.
		}
		log.Fatal(err)
	}
}

type flagError struct{ error }

func (e *flagError) Unwrap() error { return e.error }

// Run runs the worker with args (excluding the program name). It does not
// install signal handlers or call os.Exit. Canceling ctx starts the same bounded
// drain as SIGTERM in Main. Run returns flag.ErrHelp after printing help.
func Run(ctx context.Context, args []string, options Options) error {
	flags, f := newWorkerFlags(options)
	name, leg, public, control, httpAddr, agentName := f.name, f.leg, f.public, f.control, f.httpAddr, f.agentName
	cacheOff, split, version := f.cacheOff, f.split, f.version
	drainTimeout, taskBudget, moveReserve := f.drainTimeout, f.taskBudget, f.moveReserve
	if err := flags.Parse(args); err != nil {
		return &flagError{err}
	}
	storeConfig, limits := f.storeConfig, f.limits
	if *f.connect != "inbound" && *f.connect != "dial-out" {
		return fmt.Errorf("-connect must be inbound or dial-out")
	}
	if *version {
		return buildinfo.PrintVersion(os.Stdout, "relais-worker")
	}
	if err := f.bridgeOutput.Validate(); err != nil {
		return err
	}
	if *drainTimeout <= 0 {
		return fmt.Errorf("-drain-timeout must be greater than 0")
	}
	reserveSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "agent-drain-move-reserve" {
			reserveSet = true
		}
	})
	if *taskBudget < 0 || *taskBudget > 2*time.Second {
		return fmt.Errorf("-agent-drain-task-budget must be between 0 and 2s")
	}
	if reserveSet && (*moveReserve <= 0 || *moveReserve >= *drainTimeout) {
		return fmt.Errorf("-agent-drain-move-reserve must be greater than 0 and less than -drain-timeout (%s)", *drainTimeout)
	}
	agentConfig, err := registeredAgents(options, *agentName, limits, *split)
	if err != nil {
		return err
	}
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
	var listener net.Listener
	if *f.connect == "inbound" {
		listener, err = processrun.ListenPrivate(*httpAddr)
	} else {
		// Dial-out opens no listener unless an operator explicitly asks for loopback metrics.
		explicit := false
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "http" {
				explicit = true
			}
		})
		if explicit {
			host, _, splitErr := net.SplitHostPort(*httpAddr)
			ip := net.ParseIP(host)
			if splitErr != nil || ip == nil || !ip.IsLoopback() {
				return fmt.Errorf("dial-out -http must be loopback")
			}
			listener, err = processrun.ListenPrivate(*httpAddr)
		}
	}
	if err != nil {
		return err
	}
	if listener != nil {
		defer func() { _ = listener.Close() }()
	}
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
	cfg := f.mediaConfig(agentConfig, frames, &mediaworker.RelayConfig{DialOut: *f.connect == "dial-out", Addr: legAddr, PublicAddr: mediaAddr, Owners: store, StoreOutageBudget: storeConfig.OutageBudget})
	worker, err := mediaworker.New(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = worker.Close() }()
	udpbuffer.Log(log.Printf, "worker "+*name+" media", worker.SocketBuffers())
	serving, stopHTTP := context.WithCancel(context.Background())
	defer stopHTTP()
	var running sync.WaitGroup
	defer running.Wait()
	defer stopHTTP()
	done := make(chan error, 2)
	if listener != nil {
		handler := agentHandler(worker, *agentName)
		if *f.connect == "dial-out" {
			mux := http.NewServeMux()
			mux.Handle("GET /metrics", handler)
			handler = mux
		}
		running.Go(func() { done <- processrun.Serve(serving, listener, handler) })
	}
	ready := map[string]any{"media": worker.LocalAddr(), "name": *name, "connect": *f.connect}
	if listener != nil {
		ready["http"] = "http://" + listener.Addr().String()
	}
	if *f.connect == "dial-out" {
		d := &controlplane.DialOut{URL: *control, Name: *name, Worker: worker, Handler: agentHandler(worker, *agentName), Ready: make(chan struct{})}
		running.Go(func() { done <- d.Run(serving) })
		running.Go(func() { worker.KeepRelayBinding(serving) })
		select {
		case <-d.Ready:
			ready["media"] = worker.LocalAddr()
		case <-ctx.Done():
			return nil
		case err := <-done:
			return err
		}
	} else {
		worker.StartHeartbeats(&controlplane.RemoteHeartbeats{URL: *control, Name: *name})
	}
	processrun.Ready(ready)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	drainCtx, stopDrain := context.WithTimeout(context.Background(), *drainTimeout)
	quiesceWorker(drainCtx, *control, *name, worker, *taskBudget, *moveReserve)
	if err := drainWorker(drainCtx, *control, *name, worker.SessionCount); err != nil {
		log.Printf("worker %s: drain failed; closing remaining sessions: %v", *name, err)
	}
	stopDrain()
	stopHTTP()
	running.Wait()
	return nil
}

type configError struct{}

func (*configError) Error() string { return "worker requires -name and -control" }

// mediaConfig carries raw limits so each restored session selects its own profile.
func (f *workerFlags) mediaConfig(agentConfig mediaworker.AgentConfig, frames framecache.Store, relayConfig *mediaworker.RelayConfig) mediaworker.Config {
	agentConfig.BridgeOutput = f.bridgeOutput
	cfg := mediaworker.Config{SplitCheckpoints: *f.split, Agent: agentConfig, SnapshotLimits: f.limits, ListenAddr: *f.listen, FrameCache: frames, DisableFrameCache: *f.cacheOff, Relay: relayConfig}
	if *f.cryptoTrace {
		cfg.PacketTrace = func(packet mediaworker.PacketTrace) {
			data, err := json.Marshal(packet)
			if err == nil {
				log.Printf("SRTP_EGRESS %s", data)
			}
		}
	}
	return cfg
}
