package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
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
