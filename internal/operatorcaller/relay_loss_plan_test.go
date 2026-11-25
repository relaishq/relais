package operatorcaller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func TestRelayLossPlanIsExplicitAndRegistersFourCalls(t *testing.T) {
	m := fixtureManifest()
	m.Gate, m.Phase = operator.RelayHostLoss, RelayLoss
	m.Plan = NewRelayLossPlan(60, m.PublicSHA)
	at := time.UnixMilli(m.CreatedAt)
	require.NoError(t, m.Validate(m.Identity, at))
	raw, err := json.Marshal(m.Plan)
	require.NoError(t, err)
	require.Equal(t, `{"version":1,"phase":"relay-loss","jobs":1,"calls":4,"workerCallCap":2,"durationSeconds":60,"publicSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, string(raw))
	for _, sessions := range [][]string{{"one", "two", "three", "four"}, {"one"}, {"one", "two"}, {"one", "two", "three", "four", "five"}} {
		r, err := Register(m.Plan, m.RunID, sessions, fixtureBurst(at), fixtureHost(at), at, nil)
		if len(sessions) == 4 {
			require.NoError(t, err)
			require.Equal(t, sessions, r.Sessions)
		} else {
			require.Error(t, err)
		}
	}
	for _, gate := range []operator.Gate{operator.Rollout, operator.Load, operator.RelayDeploy} {
		wrong := m
		wrong.Gate = gate
		require.Error(t, wrong.Validate(wrong.Identity, at), "R plan must not widen another gate")
	}
	wrong := fixtureManifest()
	wrong.Gate = operator.RelayHostLoss
	require.Error(t, wrong.Validate(wrong.Identity, at), "R gate requires its own phase")
	require.Error(t, NewPlan(RelayLoss, 60, m.PublicSHA).Validate(), "the existing constructor must not infer R")
}

func TestRelayLossCallerArgsDoNotEnableDistributedEvents(t *testing.T) {
	m := fixtureManifest()
	e := m.Endpoints
	e.Bind, e.TokenFile = "192.0.2.1:0", "caller.token"
	args, err := CallerArgs(NewRelayLossPlan(60, m.PublicSHA), e)
	require.NoError(t, err)
	require.Contains(t, args, "-relay-loss-caller")
	require.Contains(t, args, "-callers=4")
	require.Contains(t, args, "-event-retention=15s")
	for _, a := range args {
		require.False(t, strings.HasPrefix(a, "-distributed") || strings.HasPrefix(a, "-clock-drift") || strings.HasPrefix(a, "-frontend-"))
	}
	for _, phase := range []Phase{Survivor, AdmissionBoundary} {
		old, err := CallerArgs(NewPlan(phase, 60, m.PublicSHA), e)
		require.NoError(t, err)
		require.NotContains(t, old, "-relay-loss-caller")
		require.Contains(t, old, "-distributed")
	}
}
