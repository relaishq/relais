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
	flags := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	var storeConfig processrun.StoreConfig
	storeConfig.Flags(flags)
	name := flags.String("name", processrun.Env("RELAIS_WORKER_NAME", ""), "unique worker name")
	listen := flags.String("media", processrun.Env("RELAIS_WORKER_MEDIA", "127.0.0.1:0"), "private UDP address")
	leg := flags.String("relay-leg", processrun.Env("RELAIS_RELAY_LEG", ""), "relay private UDP address")
	public := flags.String("relay-media", processrun.Env("RELAIS_RELAY_MEDIA", ""), "relay advertised public UDP address")
	control := flags.String("control", processrun.Env("RELAIS_CONTROL_URL", ""), "control-plane HTTP URL for heartbeats")
	httpAddr := flags.String("http", processrun.Env("RELAIS_WORKER_HTTP", "127.0.0.1:0"), "private worker HTTP address")
	cacheOff := flags.Bool("frame-cache-off", false, "disable cache writes and takeover replay (PLI remains enabled)")
	cryptoTrace := flags.Bool("srtp-diagnostics", false, "log sent SRTP/SRTCP ciphertext metadata for fault investigations")
	defaultAgent := options.DefaultAgent
	if defaultAgent == "" {
		defaultAgent = "echo"
	}
	agentName := flags.String("agent", defaultAgent, "audio agent: echo, demo or a registered name")
	split := flags.Bool("agent-split-checkpoints", false, "use v10 component checkpoints for new calls (upgrade every participant first)")
	limits := mediaworker.SnapshotLimits{}
	flags.IntVar(&limits.MaxBytes, "agent-max-snapshot-bytes", 7168, "v2 whole encrypted snapshot byte cap")
	flags.IntVar(&limits.BytesPerSecond, "agent-snapshot-bytes-per-second", 0, "payload bytes/s (0: legacy 81920, split 80000)")
	flags.IntVar(&limits.BurstBytes, "agent-snapshot-burst-bytes", 0, "payload burst bytes (0: legacy 28672, split 40960)")
	flags.IntVar(&limits.MoveBytesPerSession, "agent-move-bytes-per-session", 1<<20, "v2 move memory bytes per session")
	flags.Int64Var(&limits.MoveBytesTotal, "agent-move-bytes-total", 64<<20, "v2 move memory bytes per worker including export scratch")
	drainTimeout := flags.Duration("drain-timeout", 5*time.Second, "SIGTERM drain deadline before closing")
	taskBudget := flags.Duration("agent-drain-task-budget", 2*time.Second, "v2 handler quiescence cap (0 through 2s); requires a measured move reserve")
	moveReserve := flags.Duration("agent-drain-move-reserve", 0, "measured time reserved for control drain; unset skips handler quiescence")
	version := buildinfo.VersionFlag(flags)
	if err := flags.Parse(args); err != nil {
		return &flagError{err}
	}
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
	validationLimits := limits
	if *split {
		validationLimits = limits.SplitDefaults()
	} else {
		if validationLimits.BytesPerSecond == 0 {
			validationLimits.BytesPerSecond = 81920
		}
		if validationLimits.BurstBytes == 0 {
			validationLimits.BurstBytes = 28672
		}
	}
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
