package mediaworker

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pion/logging"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type permitRenewStore struct {
	sessionstore.Store
	wait             time.Duration
	fail             error
	entered, release chan struct{}
}

func (s *permitRenewStore) Renew(_ context.Context, lease sessionstore.Lease, _ time.Duration) (sessionstore.Lease, error) {
	if s.entered != nil {
		close(s.entered)
		<-s.release
	}
	time.Sleep(s.wait)
	return lease, s.fail
}
func permitRig(t *testing.T, store *permitRenewStore, ttl time.Duration) (*session, BridgeOutputScope) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	s.worker.log = logging.NewDefaultLoggerFactory().NewLogger("permit-test")
	s.lease = sessionstore.Lease{SessionID: s.id, Epoch: scope.Epoch}
	s.worker.cfg.Relay = &RelayConfig{Owners: store, LeaseTTL: ttl}
	b := s.agent.v2.bridge
	b.durable = cloneBridgeState(b.state)
	b.checkpointSeq = 1
	return s, scope
}
func TestBridgePermitRenewalUsesRequestStartAndNoExtraStoreIO(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &permitRenewStore{wait: 3 * time.Second}
		s, scope := permitRig(t, store, 34*time.Second)
		require.Zero(t, s.worker.BridgePermitRemaining(scope))
		s.worker.renewLease(s)
		require.Equal(t, 2*time.Second, s.worker.BridgePermitRemaining(scope))
		time.Sleep(2 * time.Second)
		require.Zero(t, s.worker.BridgePermitRemaining(scope))
		store.wait = 6 * time.Second
		s.worker.renewLease(s)
		require.Zero(t, s.worker.BridgePermitRemaining(scope), "a delayed success cannot restart five seconds")
		store.wait = 0
		store.fail = errors.New("uncertain")
		s.worker.renewLease(s)
		require.Zero(t, s.worker.BridgePermitRemaining(scope))
	})
}
func TestBridgePermitRevocationInvalidatesInflightRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &permitRenewStore{entered: make(chan struct{}), release: make(chan struct{})}
		s, scope := permitRig(t, store, 2*time.Second)
		done := make(chan struct{})
		go func() { defer close(done); s.worker.renewLease(s) }()
		<-store.entered
		until, err := s.worker.StopBridgePermits(scope)
		require.NoError(t, err)
		require.Equal(t, time.Now().Add(5*time.Second), until)
		close(store.release)
		<-done
		require.Zero(t, s.worker.BridgePermitRemaining(scope))
		time.Sleep(time.Second)
		again, err := s.worker.StopBridgePermits(scope)
		require.NoError(t, err)
		require.Equal(t, until, again, "retries keep grant-stop boundary")
	})
}
func TestBridgePermitLeaseTTLAndScopeAndDurability(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, scope := permitRig(t, &permitRenewStore{}, 2*time.Second)
		s.worker.renewLease(s)
		require.Equal(t, 2*time.Second, s.worker.BridgePermitRemaining(scope))
		wrong := scope
		wrong.Generation++
		require.Zero(t, s.worker.BridgePermitRemaining(wrong))
		s.agent.v2.bridge.durable = nil
		require.Zero(t, s.worker.BridgePermitRemaining(scope), "no provider work before fenced lineage checkpoint")
		s.agent.v2.bridge.durable = cloneBridgeState(s.agent.v2.bridge.state)
		s.fenced.Store(true)
		require.Zero(t, s.worker.BridgePermitRemaining(scope))
	})
}
