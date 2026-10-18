package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPoolRefusalsStayLiveWhenSampleCannotRefresh(t *testing.T) {
	p, _, _, _ := setup(t)
	p.config.WorkerCallCap = 1
	ctx := context.Background()
	require.NoError(t, p.SamplePool(ctx))
	before := p.PoolSnapshot()
	_, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	_, _, err = p.Create(ctx, "offer", "a")
	require.ErrorIs(t, err, ErrAtCapacity)
	store := &flakyListStore{Store: p.store}
	store.fail.Store(true)
	p.store = store
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.True(t, status.Pool.Stale)
	require.Equal(t, before.SampledAt, status.Pool.SampledAt)
	require.EqualValues(t, 1, status.Pool.Refusals["at_capacity"])
	require.Contains(t, planeMetricsBody(p), `relais_control_refusals_total{reason="at_capacity"} 1`)
}

func TestMissingPoolObservationOmitsDerivedGauges(t *testing.T) {
	p, _, _, _ := setup(t)
	body := planeMetricsBody(p)
	for _, name := range []string{"demand", "reservations", "over_cap_calls", "workers", "sample_timestamp_seconds", "sample_age_seconds"} {
		require.NotContains(t, body, "\nrelais_control_pool_"+name)
	}
	require.Contains(t, body, "relais_control_pool_sample_valid 0")
	require.Contains(t, body, `relais_control_refusals_total{reason="at_capacity"} 0`)
}

func TestPoolSampleExplainsNeverAnsweredWorkers(t *testing.T) {
	p, _, _, _ := setup(t)
	p.BeginBootstrap([]string{"missing"})
	reply := httptest.NewRecorder()
	p.Handler().ServeHTTP(reply, httptest.NewRequest(http.MethodGet, "/status", nil))
	require.Equal(t, 200, reply.Code)
	var status struct{ Pool map[string]any }
	require.NoError(t, json.Unmarshal(reply.Body.Bytes(), &status))
	require.Nil(t, status.Pool["demand"])
	require.Equal(t, float64(1), status.Pool["never_answered_workers"])
	require.Contains(t, planeMetricsBody(p), "relais_control_pool_never_answered_workers 1")
}
