package callharness

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

// A maintenance phase shifts the one-second recording maintenance; zero keeps
// the first tick one second after connect (#85).
func TestMaintenancePhaseShiftsRecordingTicks(t *testing.T) {
	for _, phase := range []time.Duration{0, 400 * time.Millisecond} {
		t.Run(phase.String(), func(t *testing.T) {
			polls := make(chan time.Time, 4)
			h := &Harness{workers: &workers{}, external: &ExternalTopology{Status: func(context.Context) (controlplane.Status, error) {
				select {
				case polls <- time.Now():
				default:
				}
				return controlplane.Status{}, nil
			}}}
			rec := newRecorder()
			rec.recording = RecordingOptions{MaintenancePhase: phase}.defaults()
			c := &Call{harness: h, rec: rec, remoteUfrag: "call"}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			started := time.Now()
			go func() { defer close(done); c.maintainRecording(ctx) }()
			defer func() { cancel(); <-done }()
			var first time.Time
			select {
			case first = <-polls:
			case <-time.After(5 * time.Second):
				t.Fatal("no maintenance tick")
			}
			// Timers never fire early; the upper bound only allows scheduling delay.
			require.GreaterOrEqual(t, first.Sub(started), time.Second+phase)
			require.Less(t, first.Sub(started), time.Second+phase+700*time.Millisecond)
		})
	}
	require.Zero(t, RecordingOptions{MaintenancePhase: -time.Second}.defaults().MaintenancePhase)
}
