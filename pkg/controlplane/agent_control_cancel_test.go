package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

// The caller has not run any instructions after lockControl returns: this is
// exactly the old cancellation-publication gap, without a timing test hook.
func TestControlCancellationPublishedWithHolder(t *testing.T) {
	c := &call{}
	require.NoError(t, c.lockControl(context.Background()))
	defer c.unlockControl()
	c.controlMu.Lock()
	active, cancel := c.controlActive, c.controlCancel
	c.controlMu.Unlock()
	require.True(t, active)
	require.NotNil(t, cancel, "takeover must be able to cancel every active holder")
	cancel()
}

type refusingWorker struct{ *fakeWorker }

func (w *refusingWorker) AgentControl(context.Context, string, agent.ControlMessage) (agent.ControlReceipt, error) {
	return agent.ControlReceipt{}, agent.ErrBackpressure
}

// Adapt the reviewer's untyped-worker probe to the now-typed refusal path.
func TestControlWorkerRefusalEnvelope(t *testing.T) {
	p, base, _, _ := setup(t)
	p.workers["a"].worker = &refusingWorker{fakeWorker: base}
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	server := httptest.NewServer(p.Handler())
	defer server.Close()
	response, err := server.Client().Post(server.URL+"/calls/"+id+"/agent/control", "application/json", strings.NewReader(`{"id":"1","sequence":1,"name":"stop"}`))
	require.NoError(t, err)
	defer response.Body.Close()
	var failure struct{ Code string }
	require.NoError(t, json.NewDecoder(response.Body).Decode(&failure))
	require.Equal(t, http.StatusConflict, response.StatusCode)
	require.Equal(t, "agent_backpressure", failure.Code)
}
