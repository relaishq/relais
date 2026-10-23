package workerapp

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/netip"
	"os"
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
	name, listen, leg, public, control, httpAddr, agentName := f.name, f.listen, f.leg, f.public, f.control, f.httpAddr, f.agentName
	cacheOff, cryptoTrace, split, version := f.cacheOff, f.cryptoTrace, f.split, f.version
	drainTimeout, taskBudget, moveReserve := f.drainTimeout, f.taskBudget, f.moveReserve
	if err := flags.Parse(args); err != nil {
		return &flagError{err}
	}
	storeConfig, limits := f.storeConfig, f.limits
	if *version {
		return buildinfo.PrintVersion(os.Stdout, "relais-worker")
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
	validationLimits := f.snapshotLimits()
	if err := validationLimits.Validate(); err != nil {
		return err
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
	cfg := mediaworker.Config{SplitCheckpoints: *split, Agent: agentConfig, SnapshotLimits: limits, ListenAddr: *listen, FrameCache: frames, DisableFrameCache: *cacheOff, Relay: &mediaworker.RelayConfig{Addr: legAddr, PublicAddr: mediaAddr, Owners: store, StoreOutageBudget: storeConfig.OutageBudget}}
	if *cryptoTrace {
		cfg.PacketTrace = func(packet mediaworker.PacketTrace) {
			data, err := json.Marshal(packet)
			if err == nil {
				log.Printf("SRTP_EGRESS %s", data)
			}
		}
	}
	worker, err := mediaworker.New(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = worker.Close() }()
	udpbuffer.Log(log.Printf, "worker "+*name+" media", worker.SocketBuffers())
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
	quiesceWorker(drainCtx, *control, *name, worker, *taskBudget, *moveReserve)
	if err := drainWorker(drainCtx, *control, *name, worker.SessionCount); err != nil {
		log.Printf("worker %s: drain failed; closing remaining sessions: %v", *name, err)
	}
	stopDrain()
	stopHTTP()
	return <-done
}

type configError struct{}

func (*configError) Error() string { return "worker requires -name and -control" }
