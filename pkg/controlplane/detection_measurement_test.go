package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type measuredHeartbeats struct {
	mu             sync.Mutex
	plane          *Plane
	last           time.Time
	maxGap         time.Duration
	falseTakeovers int
}

func (h *measuredHeartbeats) Heartbeat(addr netip.AddrPort) error {
	now := time.Now()
	err := h.plane.Heartbeat(addr)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.last.IsZero() {
		h.maxGap = max(h.maxGap, now.Sub(h.last))
	}
	h.last = now
	if errors.Is(err, mediaworker.ErrRejoinRequired) {
		h.falseTakeovers++
	}
	return err
}

// Opt-in observational experiment, not a claim that zero short-run events
// establish production safety. Run while the host has its ordinary shared load.
func TestHeartbeatDetectionMeasurement(t *testing.T) {
	seconds, _ := strconv.Atoi(os.Getenv("RELAIS_DETECTION_SECONDS"))
	if seconds <= 0 {
		t.Skip("set RELAIS_DETECTION_SECONDS for the live detection experiment")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var observations []*measuredHeartbeats
	var deadlines []time.Duration
	for _, ms := range []int{150, 200, 400} {
		plane := NewWithConfig(nil, sessionstore.NewMemory(), Config{DeadAfter: time.Duration(ms) * time.Millisecond})
		done := make(chan error, 1)
		go func() { done <- plane.Run(ctx) }()
		t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
		for i := range 2 {
			w, err := mediaworker.New(mediaworker.Config{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, w.Close()) })
			// Register the real transport without installing the default callback.
			require.NoError(t, plane.Register(fmt.Sprint(i), w.LocalAddr(), &noAutomaticHeartbeat{w}))
			observer := &measuredHeartbeats{plane: plane}
			w.StartHeartbeats(observer)
			observations = append(observations, observer)
			deadlines = append(deadlines, time.Duration(ms)*time.Millisecond)
		}
	}
	started := time.Now()
	<-time.After(time.Duration(seconds) * time.Second)
	elapsed := time.Since(started)
	for i, h := range observations {
		h.mu.Lock()
		t.Logf("DETECTION_MEASUREMENT dead_after=%s worker=%d observation=%s false_takeovers=%d per_worker_hour=%.3f max_heartbeat_gap=%s", deadlines[i], i%2, elapsed, h.falseTakeovers, float64(h.falseTakeovers)/elapsed.Hours(), h.maxGap)
		h.mu.Unlock()
	}
}

// Embedding Worker interface hides the optional StartHeartbeats capability.
type noAutomaticHeartbeat struct{ Worker }
