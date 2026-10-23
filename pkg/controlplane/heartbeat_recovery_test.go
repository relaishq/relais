package controlplane

import (
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func TestRecoveryHeartbeatRecordsLivenessWithoutRejoin(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "remote"}[remote], func(t *testing.T) {
			before := time.Now()
			o := newWorkerObservation(before, false)
			require.True(t, o.detect(before.Add(400*time.Millisecond), 400*time.Millisecond).newDeath)
			at := before.Add(600 * time.Millisecond)
			reply, changed, err := o.heartbeat(workerHeartbeat{remote: remote}, at)
			require.ErrorContains(t, err, "takeover in progress")
			require.Empty(t, reply.Token)
			require.False(t, changed)
			require.Equal(t, at, o.knowledge().lastHeartbeat)
			require.Equal(t, before, o.recoveryHeartbeat())
			require.False(t, o.knowledge().eligible)
			require.False(t, o.knowledge().relayEligible)
			require.False(t, o.knowledge().resumeRetry)
			o.recoveryFinished(true)
			o.preserving = true
			require.False(t, o.knowledge().replacementReady)
			require.False(t, o.canReplace(0))
			o.preserving = false
			reply, _, err = o.heartbeat(workerHeartbeat{remote: remote}, at)
			if remote {
				require.NoError(t, err)
				require.NotEmpty(t, reply.Token)
			} else {
				require.ErrorIs(t, err, mediaworker.ErrRejoinRequired)
			}
			require.False(t, o.knowledge().eligible)
			_, _, err = o.heartbeat(workerHeartbeat{remote: remote, token: reply.Token}, at)
			require.NoError(t, err)
			require.True(t, o.knowledge().eligible)
		})
	}
}
