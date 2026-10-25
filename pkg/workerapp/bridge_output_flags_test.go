package workerapp

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func TestBridgeOutputProfileFlags(t *testing.T) {
	flags, f := newWorkerFlags(Options{})
	require.NoError(t, flags.Parse([]string{"-bridge-export-headroom-bytes=131072", "-bridge-observer-request-bytes=16384", "-bridge-output-admission=40ms", "-bridge-output-packets=16", "-bridge-output-payload-bytes=19200", "-bridge-audio-bytes=131072", "-bridge-audio-duration=15s", "-bridge-receipt-ranges=4", "-bridge-receipt-bytes=256"}))
	require.Equal(t, mediaworker.BridgeOutputLimits{ActorReserveBytes: 160 << 10, ObserverRequestBytes: 16384, ObserverMailboxBytes: 20480, AdapterMoveOnlyBytes: 11264, ExportHeadroomBytes: 131072, AdmissionTimeout: 40 * time.Millisecond, PendingPackets: 16, PendingPayloadBytes: 19200, AudioBytes: 131072, AudioDuration: 15 * time.Second, ReceiptRanges: 4, ReceiptBytes: 256}, f.bridgeOutput.Defaults())
	require.NoError(t, f.bridgeOutput.Validate())
}
func TestBridgeOutputInvalidProfileRefusedBeforeStartup(t *testing.T) {
	require.ErrorContains(t, Run(context.Background(), []string{"-bridge-output-admission=-1ms"}, Options{}), "invalid bridge output profile")
}

func TestBridgeOutputMeasurementTargetsAreNotWorkerFlags(t *testing.T) {
	for _, name := range []string{"bridge-interruption-target", "bridge-fixture-packet-duration", "bridge-fixture-burst-rate", "bridge-fixture-burst-duration", "bridge-output-latency-target", "bridge-output-recovery-target"} {
		flags, _ := newWorkerFlags(Options{})
		require.Nil(t, flags.Lookup(name), name)
	}
}

func TestBridgeOutputInvalidExportHeadroomRefused(t *testing.T) {
	require.ErrorContains(t, Run(context.Background(), []string{"-bridge-export-headroom-bytes=-1"}, Options{}), "invalid bridge output profile")
}

func TestBridgeOutputInvalidObserverRequestLimitRefused(t *testing.T) {
	require.ErrorContains(t, Run(context.Background(), []string{"-bridge-observer-request-bytes=65536"}, Options{}), "invalid bridge output profile")
}
