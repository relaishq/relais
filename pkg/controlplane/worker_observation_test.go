package controlplane

import (
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func TestWorkerObservationFailedReadsKeepOriginalGrace(t *testing.T) {
	now := time.Now()
	o := newWorkerObservation(now, false)
	require.True(t, o.observeStatus(mediaworker.WorkerStatus{}, true, true))
	for range 3 {
		require.False(t, o.observeStatus(mediaworker.WorkerStatus{}, true, false))
	}
	require.False(t, o.knowledge().eligible)
	require.Equal(t, workerPoolPending, o.knowledge().poolClass)
	require.False(t, o.detect(now.Add(400*time.Millisecond-time.Nanosecond), 400*time.Millisecond).recover)
	detected := o.detect(now.Add(400*time.Millisecond), 400*time.Millisecond)
	require.True(t, detected.recover)
	require.True(t, detected.newDeath)
	require.Equal(t, workerPoolUnreachable, o.knowledge().poolClass)
}

func TestWorkerObservationSlowStatusKeepsHealthyHeartbeats(t *testing.T) {
	now := time.Now()
	o := newWorkerObservation(now, false)
	for i := 1; i <= 12; i++ {
		at := now.Add(time.Duration(i) * 100 * time.Millisecond)
		_, changed, err := o.heartbeat(workerHeartbeat{remote: true, restart: true}, at)
		require.NoError(t, err)
		require.False(t, changed)
		// Initial status can complete after heartbeats have already arrived.
		o.observeStatus(mediaworker.WorkerStatus{}, true, i == 1)
		require.True(t, o.knowledge().eligible)
		require.False(t, o.detect(at.Add(99*time.Millisecond), 400*time.Millisecond).recover)
	}
}

func TestWorkerObservationHeartbeatResolvesUnknownOnce(t *testing.T) {
	now := time.Now()
	o := newWorkerObservation(now, false)
	o.observeStatus(mediaworker.WorkerStatus{}, true, true)
	_, changed, err := o.heartbeat(workerHeartbeat{remote: true, restart: true, token: "prior"}, now.Add(time.Millisecond))
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, o.knowledge().eligible)
	_, changed, err = o.heartbeat(workerHeartbeat{remote: true, restart: true, token: "prior"}, now.Add(2*time.Millisecond))
	require.NoError(t, err)
	require.False(t, changed)
	_, _, err = o.heartbeat(workerHeartbeat{remote: true, restart: true, token: "other"}, now)
	require.ErrorContains(t, err, "stale rejoin token")
}

func TestWorkerObservationRecoveryAndLocalRejoin(t *testing.T) {
	now := time.Now()
	o := newWorkerObservation(now, false)
	o.markDraining()
	require.True(t, o.detect(now.Add(time.Second), time.Second).newDeath)
	require.False(t, o.detect(now.Add(2*time.Second), time.Second).recover, "active recovery is not duplicated")
	require.False(t, o.knowledge().retryPlanned, "active recovery cannot start planned retries")
	_, _, err := o.heartbeat(workerHeartbeat{}, now)
	require.ErrorContains(t, err, "takeover in progress")
	o.recoveryFinished(false)
	retry := o.detect(now.Add(2*time.Second), time.Second)
	require.True(t, retry.recover)
	require.False(t, retry.newDeath)
	o.recoveryFinished(true)
	require.False(t, o.canReplace(1))
	require.True(t, o.canReplace(0))
	_, _, err = o.heartbeat(workerHeartbeat{}, now.Add(3*time.Second))
	require.ErrorIs(t, err, mediaworker.ErrRejoinRequired)
	require.True(t, o.knowledge().dead)
	_, _, err = o.heartbeat(workerHeartbeat{}, now.Add(3*time.Second))
	require.NoError(t, err)
	require.False(t, o.knowledge().dead)
	require.Equal(t, workerPoolDraining, o.knowledge().poolClass)
	require.False(t, o.canReplace(0))
}

func TestWorkerObservationRemoteRejoinTokens(t *testing.T) {
	now := time.Now()
	o := newWorkerObservation(now, false)
	var previous string
	for cycle := 1; cycle <= 2; cycle++ {
		at := now.Add(time.Duration(cycle) * time.Second)
		require.True(t, o.detect(at, 400*time.Millisecond).newDeath)
		_, _, err := o.heartbeat(workerHeartbeat{remote: true}, at)
		require.ErrorContains(t, err, "takeover in progress")
		o.recoveryFinished(true)
		challenge, changed, err := o.heartbeat(workerHeartbeat{remote: true, token: previous}, at)
		require.NoError(t, err)
		require.False(t, changed)
		require.NotEmpty(t, challenge.Token)
		require.NotEqual(t, previous, challenge.Token)
		for _, token := range []string{"", "wrong"} {
			reply, _, err := o.heartbeat(workerHeartbeat{remote: true, token: token}, at)
			require.NoError(t, err)
			require.Equal(t, challenge, reply)
			require.True(t, o.knowledge().dead)
		}
		for range 2 {
			reply, _, err := o.heartbeat(workerHeartbeat{remote: true, token: challenge.Token}, at)
			require.NoError(t, err)
			require.Empty(t, reply.Token)
		}
		_, _, err = o.heartbeat(workerHeartbeat{remote: true, token: "wrong"}, at)
		require.ErrorContains(t, err, "stale rejoin token")
		previous = challenge.Token
	}
}

func TestWorkerObservationPoolPrecedenceAndReplacement(t *testing.T) {
	now := time.Now()
	o := newWorkerObservation(now, true)
	o.markDraining()
	o.observeStatus(mediaworker.WorkerStatus{}, true, true)
	require.Equal(t, workerPoolPending, o.knowledge().poolClass)
	o.detect(now.Add(time.Second), time.Second)
	require.Equal(t, workerPoolPending, o.knowledge().poolClass, "relay pending precedes death")
	require.True(t, o.relayRegistered())
	require.False(t, o.relayRegistered())
	require.Equal(t, workerPoolUnreachable, o.knowledge().poolClass)
	o.recoveryFinished(true)
	o.resetDrain()
	require.True(t, o.knowledge().dead, "drain reset retains fencing")
	require.True(t, o.canReplace(0))
	replacement := newWorkerObservation(now.Add(time.Second), false)
	require.True(t, replacement.knowledge().eligible)
	require.Equal(t, workerPoolReady, replacement.knowledge().poolClass)
	_, _, err := replacement.heartbeat(workerHeartbeat{remote: true, token: "old-token"}, now)
	require.ErrorContains(t, err, "stale rejoin token")
}

func TestWorkerObservationStatusDrainOrdering(t *testing.T) {
	o := newWorkerObservation(time.Now(), false)
	o.markDraining()
	o.observeStatus(mediaworker.WorkerStatus{}, false, true)
	require.True(t, o.knowledge().eligible, "initial status retains assignment semantics")
	o.observeStatus(mediaworker.WorkerStatus{Draining: true}, false, false)
	o.observeStatus(mediaworker.WorkerStatus{}, false, false)
	require.Equal(t, workerPoolDraining, o.knowledge().poolClass, "later merge cannot clear drain")
	o.observeStatus(mediaworker.WorkerStatus{}, true, true)
	require.Equal(t, workerPoolPending, o.knowledge().poolClass, "unknown precedes draining")
	require.False(t, o.knowledge().dead)
}

func TestWorkerObservationPriorHeartbeatTokenExpiresWithDeathCycle(t *testing.T) {
	now := time.Now()
	o := newWorkerObservation(now, false)
	_, _, err := o.heartbeat(workerHeartbeat{remote: true, restart: true, token: "prior-completed-handshake"}, now)
	require.NoError(t, err)
	_, _, err = o.heartbeat(workerHeartbeat{remote: true, restart: true, token: "different-token"}, now)
	require.ErrorContains(t, err, "stale rejoin token")
	require.True(t, o.detect(now.Add(time.Second), 400*time.Millisecond).newDeath)
	o.recoveryFinished(true)
	reply, _, err := o.heartbeat(workerHeartbeat{remote: true, restart: true, token: "prior-completed-handshake"}, now)
	require.NoError(t, err)
	require.NotEmpty(t, reply.Token)
	require.NotEqual(t, "prior-completed-handshake", reply.Token)
	require.True(t, o.knowledge().dead)
}

func TestWorkerObservationStatusCannotRejoinDeadRegistration(t *testing.T) {
	now := time.Now()
	o := newWorkerObservation(now, false)
	o.detect(now.Add(time.Second), time.Second)
	o.recoveryFinished(true)
	o.observeStatus(mediaworker.WorkerStatus{}, false, false)
	require.False(t, o.knowledge().eligible)
	require.False(t, o.knowledge().relayEligible)
	require.Equal(t, workerCreateRecover, o.knowledge().createDisposition)
	require.True(t, o.canReplace(0))
}

func TestWorkerObservationLiveHeartbeatAnchorsGraceAtAcceptance(t *testing.T) {
	for _, remote := range []bool{false, true} {
		o := newWorkerObservation(time.Unix(1, 0), false)
		before := time.Now()
		_, changed, err := o.heartbeat(workerHeartbeat{remote: remote}, time.Time{})
		after := time.Now()
		require.NoError(t, err)
		require.False(t, changed)
		require.False(t, o.detect(before.Add(400*time.Millisecond-time.Nanosecond), 400*time.Millisecond).recover)
		require.True(t, o.detect(after.Add(400*time.Millisecond), 400*time.Millisecond).newDeath)
	}
}
