package workerapp

import (
	"flag"
	"io"
	"os"
	"time"

	"github.com/relaishq/relais/internal/processrun"
	"github.com/relaishq/relais/pkg/buildinfo"
	"github.com/relaishq/relais/pkg/mediaworker"
)

type workerFlags struct {
	storeConfig                                             processrun.StoreConfig
	limits                                                  mediaworker.SnapshotLimits
	bridgeOutput                                            mediaworker.BridgeOutputLimits
	name, listen, leg, public, control, httpAddr, agentName *string
	cacheOff, cryptoTrace, split, version                   *bool
	drainTimeout, taskBudget, moveReserve                   *time.Duration
}

// The registration preflight and Run use the same flag declarations. New boolean
// flags therefore cannot consume the split switch as an apparent string value.
func newWorkerFlags(options Options) (*flag.FlagSet, *workerFlags) {
	f := &workerFlags{}
	flags := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	f.storeConfig.Flags(flags)
	f.name = flags.String("name", processrun.Env("RELAIS_WORKER_NAME", ""), "unique worker name")
	f.listen = flags.String("media", processrun.Env("RELAIS_WORKER_MEDIA", "127.0.0.1:0"), "private UDP address")
	f.leg = flags.String("relay-leg", processrun.Env("RELAIS_RELAY_LEG", ""), "relay private UDP address")
	f.public = flags.String("relay-media", processrun.Env("RELAIS_RELAY_MEDIA", ""), "relay advertised public UDP address")
	f.control = flags.String("control", processrun.Env("RELAIS_CONTROL_URL", ""), "control-plane HTTP URL for heartbeats")
	f.httpAddr = flags.String("http", processrun.Env("RELAIS_WORKER_HTTP", "127.0.0.1:0"), "private worker HTTP address")
	f.cacheOff = flags.Bool("frame-cache-off", false, "disable cache writes and takeover replay (PLI remains enabled)")
	f.cryptoTrace = flags.Bool("srtp-diagnostics", false, "log sent SRTP/SRTCP ciphertext metadata for fault investigations")
	defaultAgent := options.DefaultAgent
	if defaultAgent == "" {
		defaultAgent = "echo"
	}
	f.agentName = flags.String("agent", defaultAgent, "audio agent: echo, demo or a registered name")
	f.split = flags.Bool("agent-split-checkpoints", false, "use v10 component checkpoints for new calls (upgrade every participant first)")
	flags.IntVar(&f.limits.MaxBytes, "agent-max-snapshot-bytes", 0, "encoded snapshot byte cap (0: legacy 7168, split hot 6144)")
	flags.IntVar(&f.limits.BytesPerSecond, "agent-snapshot-bytes-per-second", 0, "payload bytes/s (0: legacy 81920, split 70000)")
	flags.IntVar(&f.limits.BurstBytes, "agent-snapshot-burst-bytes", 0, "payload burst bytes (0: legacy 28672, split 40960)")
	flags.IntVar(&f.limits.MoveBytesPerSession, "agent-move-bytes-per-session", 1<<20, "v2 move memory bytes per session")
	flags.Int64Var(&f.limits.MoveBytesTotal, "agent-move-bytes-total", 64<<20, "v2 move memory bytes per worker including export scratch")
	flags.IntVar(&f.bridgeOutput.ActorReserveBytes, "bridge-actor-reserve-bytes", 0, "bridge mailbox headroom for actor requests/results (0: 163840)")
	flags.IntVar(&f.bridgeOutput.ObserverMailboxBytes, "bridge-observer-mailbox-bytes", 0, "bridge aggregate pending results and terminal reserve bytes (0: 20480)")
	flags.IntVar(&f.bridgeOutput.AdapterMoveOnlyBytes, "bridge-adapter-move-only-bytes", 0, "bridge adapter move-only state bytes (0: 11264)")
	flags.IntVar(&f.bridgeOutput.ObserverRequestBytes, "bridge-observer-request-bytes", 0, "bridge aggregate task request bytes (0: 16384)")
	flags.IntVar(&f.bridgeOutput.ExportHeadroomBytes, "bridge-export-headroom-bytes", 0, "bridge export safety headroom (0: 209715)")
	flags.DurationVar(&f.bridgeOutput.AdmissionTimeout, "bridge-output-admission", 0, "bridge admission deadline from ingress (0: 50ms)")
	flags.IntVar(&f.bridgeOutput.PendingPackets, "bridge-output-packets", 0, "bridge pending packet slots (0: 32)")
	flags.IntVar(&f.bridgeOutput.PendingPayloadBytes, "bridge-output-payload-bytes", 0, "bridge pending payload bytes (0: 38400)")
	flags.IntVar(&f.bridgeOutput.AudioBytes, "bridge-audio-bytes", 0, "bridge retained audio bytes inside mailbox (0: 262144)")
	flags.DurationVar(&f.bridgeOutput.AudioDuration, "bridge-audio-duration", 0, "bridge retained audio duration (0: 4s)")
	flags.IntVar(&f.bridgeOutput.ReceiptRanges, "bridge-receipt-ranges", 0, "bridge receipt exception ranges (0: 8)")
	flags.IntVar(&f.bridgeOutput.ReceiptBytes, "bridge-receipt-bytes", 0, "bridge encoded exception bytes (0: 512)")
	f.drainTimeout = flags.Duration("drain-timeout", 5*time.Second, "SIGTERM drain deadline before closing")
	f.taskBudget = flags.Duration("agent-drain-task-budget", 2*time.Second, "v2 handler quiescence cap (0 through 2s); requires a measured move reserve")
	f.moveReserve = flags.Duration("agent-drain-move-reserve", 0, "measured time reserved for control drain; unset skips handler quiescence")
	f.version = buildinfo.VersionFlag(flags)
	return flags, f
}

// SplitCheckpoints reads the worker's split switch for choosing registrations
// before Main runs. It uses Run's flag parser and does not consume args. Invalid
// flags return false; Run remains responsible for reporting validation errors.
func SplitCheckpoints(args []string) bool {
	flags, f := newWorkerFlags(Options{})
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		return false
	}
	return *f.split
}

// SnapshotLimitsForArgs resolves the operator budgets through the worker's own
// parser. Reports use this to name the same profile and overrides as Run.
func SnapshotLimitsForArgs(args []string) (mediaworker.SnapshotLimits, error) {
	flags, f := newWorkerFlags(Options{})
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		return mediaworker.SnapshotLimits{}, err
	}
	return f.snapshotLimits(), nil
}

func (f *workerFlags) snapshotLimits() mediaworker.SnapshotLimits {
	if *f.split {
		return f.limits.SplitDefaults()
	}
	return f.limits.LegacyDefaults()
}
