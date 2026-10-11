package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/require"
)

func planeMetricsBody(p *Plane) string {
	r := httptest.NewRecorder()
	p.ProcessHandler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return r.Body.String()
}

func checkpointPolicyFromScrape(t *testing.T, p *Plane, policy string) float64 {
	t.Helper()
	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(strings.NewReader(planeMetricsBody(p)))
	require.NoError(t, err)
	family := families["relais_checkpoint_envelope_events_total"]
	require.NotNil(t, family)
	for _, metric := range family.Metric {
		if len(metric.Label) == 1 && metric.Label[0].GetName() == "policy" && metric.Label[0].GetValue() == policy {
			return metric.Counter.GetValue()
		}
	}
	t.Fatalf("missing checkpoint policy %q", policy)
	return 0
}

func TestControlMetricsCountMoveRollbackLossAndFailedDrain(t *testing.T) {
	p, a, b, _ := setup(t)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	a.resumeError, b.resumeError = true, true
	_, err = p.Move(ctx, id, "b")
	require.Error(t, err)
	body := planeMetricsBody(p)
	require.Contains(t, body, "relais_control_moves_total 0\n")
	require.Contains(t, body, "relais_control_move_errors_total 1\n")
	require.Contains(t, body, "relais_control_losses_total 1\n")
	_, err = p.Drain(ctx, "unknown")
	require.Error(t, err)
	require.Contains(t, planeMetricsBody(p), "relais_control_drain_errors_total 1\n")
}

func TestControlMetricsCountTerminalTakeoverLoss(t *testing.T) {
	p, _, _, _ := setup(t)
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(context.Background(), id)
	require.NoError(t, err)
	p.takeover(context.Background(), p.workers["a"], lease, time.Now()) // missing snapshot
	body := planeMetricsBody(p)
	require.Contains(t, body, "relais_control_takeovers_total 0\n")
	require.Contains(t, body, "relais_control_takeover_errors_total 1\n")
	require.Contains(t, body, "relais_control_losses_total 1\n")
}

func TestControlMetricsCountRolledBackMove(t *testing.T) {
	p, _, b, _ := setup(t)
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	b.resumeError = true
	result, err := p.Move(context.Background(), id, "b")
	require.Error(t, err)
	require.True(t, result.Result.RolledBack)
	body := planeMetricsBody(p)
	require.Contains(t, body, "relais_control_move_errors_total 1\n")
	require.Contains(t, body, "relais_control_losses_total 0\n")
}

func TestControlMetricsCountRetainedMoveCompletion(t *testing.T) {
	p, _, _, _ := setup(t)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	p.store = &retainedMoveStore{Store: p.store}
	_, err = p.Move(ctx, id, "b")
	require.Error(t, err)
	source := p.workers["a"]
	pending := source.pending[id]
	require.NotNil(t, pending)
	p.takeover(ctx, source, pending.lease, time.Now())
	body := planeMetricsBody(p)
	require.Contains(t, body, "relais_control_moves_total 1\n")
	require.Contains(t, body, "relais_control_move_errors_total 1\n")
	require.Contains(t, body, "relais_control_losses_total 0\n")
}

func TestControlMetricsStayOffSignalingHandler(t *testing.T) {
	p, _, _, _ := setup(t)
	response := httptest.NewRecorder()
	p.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusNotFound, response.Code)
	require.Contains(t, planeMetricsBody(p), "relais_control_calls_total 0\n")
}

func TestControlMetricsUncertainExportIsOneMove(t *testing.T) {
	for _, outcome := range []string{"success", "loss"} {
		t.Run(outcome, func(t *testing.T) {
			p, a, b, _ := setup(t)
			p.workers["a"].worker = &uncertainExportWorker{fakeWorker: a}
			p.workers["b"].worker = &takeoverWorker{fakeWorker: b}
			ctx := context.Background()
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			if outcome == "success" {
				lease, getErr := p.store.Get(ctx, id)
				require.NoError(t, getErr)
				require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
			}
			_, err = p.Move(ctx, id, "b")
			moves, failures := 0, 1
			if outcome == "success" {
				require.NoError(t, err)
				require.True(t, b.runs(id))
				moves, failures = 1, 0
			} else {
				require.Error(t, err)
			}
			body := planeMetricsBody(p)
			require.Contains(t, body, fmt.Sprintf("relais_control_moves_total %d\n", moves))
			require.Contains(t, body, fmt.Sprintf("relais_control_move_errors_total %d\n", failures))
			require.Contains(t, body, fmt.Sprintf("relais_control_losses_total %d\n", failures))
			require.Contains(t, body, "relais_control_takeovers_total 0\n")
			require.Contains(t, body, "relais_control_takeover_errors_total 0\n")
		})
	}
}

type gatedUncertainExportWorker struct {
	*uncertainExportWorker
	started chan struct{}
	gate    chan struct{}
}

func (w *gatedUncertainExportWorker) ExportSession(id string) ([]byte, error) {
	close(w.started)
	<-w.gate
	return w.uncertainExportWorker.ExportSession(id)
}

func TestControlMetricsUncertainExportConcurrentTransferCountsOneMoveError(t *testing.T) {
	p, a, b, _ := setup(t)
	w := &gatedUncertainExportWorker{
		uncertainExportWorker: &uncertainExportWorker{fakeWorker: a},
		started:               make(chan struct{}),
		gate:                  make(chan struct{}),
	}
	p.workers["a"].worker = w
	t.Cleanup(func() {
		select {
		case <-w.gate:
		default:
			close(w.gate)
		}
	})
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, moveErr := p.Move(ctx, id, "b")
		done <- moveErr
	}()
	select {
	case <-w.started:
	case <-time.After(5 * time.Second):
		t.Fatal("move did not reach export")
	}
	// Another controller transfers ownership before uncertain-export recovery
	// reads the lease. Recovery must release its pending entry without loss.
	_, err = p.store.Transfer(ctx, lease, b.addr, time.Minute)
	require.NoError(t, err)
	close(w.gate)
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("move did not finish")
	}
	require.ErrorIs(t, err, context.DeadlineExceeded)
	body := planeMetricsBody(p)
	require.Contains(t, body, "relais_control_moves_total 0\n")
	require.Contains(t, body, "relais_control_move_errors_total 1\n")
	require.Contains(t, body, "relais_control_losses_total 0\n")
	require.Contains(t, body, "relais_control_takeovers_total 0\n")
	require.Contains(t, body, "relais_control_takeover_errors_total 0\n")
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.Empty(t, status.Takeovers)
	require.Len(t, status.Calls, 1)
	require.Equal(t, "b", status.Calls[0].Owner)
}

func TestControlMetricsRetainedUncertainExportCountsOnlyTerminalMove(t *testing.T) {
	for _, outcome := range []string{"success", "loss"} {
		t.Run(outcome, func(t *testing.T) {
			p, a, b, _ := setup(t)
			p.workers["a"].worker = &uncertainExportWorker{fakeWorker: a}
			p.workers["b"].worker = &takeoverWorker{fakeWorker: b}
			ctx := context.Background()
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			if outcome == "success" {
				lease, getErr := p.store.Get(ctx, id)
				require.NoError(t, getErr)
				require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
			}
			store := &recoveryErrorStore{Store: p.store, step: "transfer", fail: true}
			p.store = store
			_, err = p.Move(ctx, id, "b")
			require.Error(t, err)
			require.Contains(t, planeMetricsBody(p), "relais_control_move_errors_total 0\n", "pending recovery has no terminal outcome")
			source := p.workers["a"]
			pending := source.pending[id]
			require.NotNil(t, pending)
			store.fail = false
			p.takeover(ctx, source, pending.lease, time.Now())
			require.Empty(t, source.pending)
			moves, failures := 0, 1
			if outcome == "success" {
				moves, failures = 1, 0
				require.True(t, b.runs(id))
			}
			body := planeMetricsBody(p)
			require.Contains(t, body, fmt.Sprintf("relais_control_moves_total %d\n", moves))
			require.Contains(t, body, fmt.Sprintf("relais_control_move_errors_total %d\n", failures))
			require.Contains(t, body, fmt.Sprintf("relais_control_losses_total %d\n", failures))
			require.Contains(t, body, "relais_control_takeovers_total 0\n")
			require.Contains(t, body, "relais_control_takeover_errors_total 0\n")
		})
	}
}

func TestControlMetricsRecoveryOutcomeSurvivesHistoryEviction(t *testing.T) {
	for _, recovery := range []string{"success", "missing snapshot", "vanished lease"} {
		t.Run(recovery, func(t *testing.T) {
			p, a, b, _ := setup(t)
			p.workers["a"].worker = &uncertainExportWorker{fakeWorker: a}
			p.workers["b"].worker = &takeoverWorker{fakeWorker: b}
			ctx := context.Background()
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			if recovery == "success" {
				require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
			}
			store := &recoveryErrorStore{Store: p.store, step: "transfer", fail: true}
			p.store = store
			_, err = p.Move(ctx, id, "b")
			require.Error(t, err)
			store.fail = false
			if recovery == "vanished lease" {
				require.NoError(t, p.store.Release(ctx, lease))
			}
			source := p.workers["a"]
			c := p.calls[id]
			c.mu.Lock()
			outcome := p.takeoverLocked(ctx, source, c, lease, time.Now())
			c.mu.Unlock()
			// Simulate other calls completing before the caller consumes the
			// returned outcome. Its accounting must survive status eviction.
			p.mu.Lock()
			for i := 0; i < recentTakeoverLimit+1; i++ {
				p.recordTakeover(MoveResult{ID: fmt.Sprintf("other-%d", i), Kind: "takeover"})
			}
			p.mu.Unlock()
			status, err := p.Status(ctx)
			require.NoError(t, err)
			require.Len(t, status.Takeovers, recentTakeoverLimit)
			for _, event := range status.Takeovers {
				require.NotEqual(t, id, event.ID)
			}
			require.NotNil(t, outcome, "terminal accounting must not depend on status history")
			require.Equal(t, id, outcome.ID)
			moves, failures := 0, 1
			if recovery == "success" {
				require.False(t, outcome.Lost)
				require.Empty(t, outcome.Error)
				moves, failures = 1, 0
			} else {
				require.True(t, outcome.Lost)
				require.NotEmpty(t, outcome.Error)
			}
			body := planeMetricsBody(p)
			require.Contains(t, body, fmt.Sprintf("relais_control_moves_total %d\n", moves))
			require.Contains(t, body, fmt.Sprintf("relais_control_move_errors_total %d\n", failures))
			require.Contains(t, body, fmt.Sprintf("relais_control_losses_total %d\n", failures))
			require.Contains(t, body, "relais_control_takeovers_total 0\n")
			require.Contains(t, body, "relais_control_takeover_errors_total 0\n")
		})
	}
}

type failedReleaseRelay struct{ *fakeRelay }

func (*failedReleaseRelay) ReleaseSession(string, netip.AddrPort) (int, error) {
	return 0, errors.New("injected release failure")
}

func TestControlMetricsFinalReleaseFailureIsOneMoveError(t *testing.T) {
	p, _, b, r := setup(t)
	p.relay = &failedReleaseRelay{fakeRelay: r}
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	_, err = p.Move(context.Background(), id, "b")
	require.ErrorContains(t, err, "release caller hold")
	require.True(t, b.runs(id), "adoption still completed")
	body := planeMetricsBody(p)
	require.Contains(t, body, "relais_control_moves_total 0\n")
	require.Contains(t, body, "relais_control_move_errors_total 1\n")
	require.Contains(t, body, "relais_control_losses_total 0\n")
}
