package mediaworker

import (
	"testing"

	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestSplitConstructorChecksLegacyCadenceAndRegistrationFit(t *testing.T) {
	factory := agenthost.Factory(agenthost.Registration{Name: "restored/v1", Factory: func(agent.FactoryArgs) agent.Agent {
		t.Fatal("startup constructed an application agent")
		return nil
	}})
	for _, tc := range []struct {
		name   string
		limits SnapshotLimits
		kind   error
		texts  []string
	}{
		{"legacy-rate", SnapshotLimits{BytesPerSecond: 70000}, agent.ErrCapacity, []string{"legacy session profile", "78848", "raise -agent-snapshot-bytes-per-second"}},
		{"legacy-fit", SnapshotLimits{MaxBytes: 6144, BytesPerSecond: 70000}, agent.ErrStateTooLarge, []string{`legacy session profile registration "restored/v1"`, "needs 6932 encoded bytes", "cap 6144", "short by 788 bytes"}},
		{"split-fit", SnapshotLimits{BurstBytes: 20000}, agent.ErrCapacity, []string{`split session profile registration "restored/v1"`, "split burst cannot hold two maximum transactions"}},
		{"defaults", SnapshotLimits{}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The selected legacy factory stays opaque; this registration is only
			// used for adoption. New must validate it before opening the socket.
			cfg := Config{SplitCheckpoints: true, SnapshotLimits: tc.limits, Agent: AgentConfig{Registrations: map[string]legacy.Factory{"restored/v1": factory}}}
			want := ValidateSnapshotProfiles(cfg)
			w, err := New(cfg)
			if w != nil {
				require.NoError(t, w.Close())
			}
			if tc.kind == nil {
				require.NoError(t, want)
				require.NoError(t, err)
				return
			}
			require.Nil(t, w)
			require.ErrorIs(t, err, tc.kind)
			require.EqualError(t, err, want.Error())
			for _, text := range tc.texts {
				require.ErrorContains(t, err, text)
			}
		})
	}
}
