package controlplane

import (
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

// Fixtures cross the same observation interface as the detector. The caller
// holds the registry lock when background plane work is running.
func expireObservedWorker(t *testing.T, w *registration) {
	t.Helper()
	require.True(t, w.observation.detect(time.Now().Add(time.Hour), time.Minute).recover)
	w.observation.recoveryFinished(false)
}

func rejoinObservedWorker(t *testing.T, w *registration) {
	t.Helper()
	w.observation.recoveryFinished(true)
	_, _, err := w.observation.heartbeat(workerHeartbeat{}, time.Now())
	require.ErrorIs(t, err, mediaworker.ErrRejoinRequired)
	_, _, err = w.observation.heartbeat(workerHeartbeat{}, time.Now())
	require.NoError(t, err)
}
