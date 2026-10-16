package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

type dataPolicyWorker struct {
	*fakeWorker
	ended  string
	checks int
}

func (w *dataPolicyWorker) CreateSession(ctx context.Context, offer string) (string, string, error) {
	id, _, err := w.fakeWorker.CreateSession(ctx, offer)
	return id, "m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n", err
}
func (w *dataPolicyWorker) CheckSessionMove(string) error {
	w.checks++
	return mediaworker.ErrDataSessionNotMovable
}
func (w *dataPolicyWorker) EndSessionReason(id, reason string) error {
	w.ended = reason
	return w.EndSession(id)
}

func TestDataSessionPolicies(t *testing.T) {
	t.Run("move definitive and no target required", func(t *testing.T) {
		p, a, b, _ := setup(t)
		w := &dataPolicyWorker{fakeWorker: a}
		p.workers["a"].worker = w
		id, _, err := p.Create(context.Background(), "offer", "a")
		require.NoError(t, err)
		p.workers["b"].draining = true
		_, err = p.Move(context.Background(), id, "")
		require.ErrorIs(t, err, mediaworker.ErrDataSessionNotMovable)
		require.False(t, uncertainExport(err))
		require.True(t, a.runs(id))
		require.False(t, b.runs(id))
		require.Empty(t, p.workers["a"].pending)
	})
	t.Run("drain ends with no target", func(t *testing.T) {
		p, a, _, _ := setup(t)
		w := &dataPolicyWorker{fakeWorker: a}
		p.workers["a"].worker = w
		id, _, err := p.Create(context.Background(), "offer", "a")
		require.NoError(t, err)
		p.workers["b"].draining = true
		results, err := p.Drain(context.Background(), "a")
		require.NoError(t, err)
		require.Len(t, results, 1)
		require.Equal(t, "ended", results[0].Kind)
		require.False(t, a.runs(id))
		require.Equal(t, "reconnect_required", w.ended)
	})
	for _, target := range []bool{false, true} {
		t.Run(map[bool]string{false: "takeover no standby", true: "takeover standby"}[target], func(t *testing.T) {
			p, a, b, r := setup(t)
			ctx := context.Background()
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			require.NoError(t, p.store.PutState(ctx, lease, []byte(`{"Version":9,"State":{"Version":9,"Data":true}}`)))
			if !target {
				p.workers["b"].draining = true
			}
			outcome := p.takeoverLocked(ctx, p.workers["a"], p.calls[id], lease, time.Now())
			require.NotNil(t, outcome)
			require.True(t, outcome.Lost)
			require.Equal(t, "data_session_reconnect_required", outcome.LossCause)
			require.Contains(t, outcome.Error, mediaworker.ErrDataSessionReconnectRequired.Error())
			require.False(t, b.runs(id))
			require.Contains(t, r.forgotten, id)
			_ = a
		})
	}
}
