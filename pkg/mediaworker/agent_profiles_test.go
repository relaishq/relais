package mediaworker

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/pion/logging"
	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestSplitConstructorChecksCadenceAndSplitRegistrationFit(t *testing.T) {
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
		{"legacy-fit-warning", SnapshotLimits{MaxBytes: 6144, BytesPerSecond: 70000}, nil, nil},
		{"split-fit", SnapshotLimits{BurstBytes: 20000}, agent.ErrCapacity, []string{`split session profile registration "restored/v1"`, "split burst cannot hold two maximum transactions"}},
		{"defaults", SnapshotLimits{}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The selected legacy factory stays opaque; this registration is only
			// used for adoption. New must validate split admission before opening the socket.
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

func TestSplitStartupWarnsOncePerIncompatibleLegacyRegistration(t *testing.T) {
	for _, tc := range []struct {
		name, need, cap, shortfall string
		limits                     SnapshotLimits
		declared                   agent.Limits
	}{
		{"split-sized-declaration", "9660", "7168", "2492", SnapshotLimits{}, agent.Limits{MaxStateBytes: 3072, MaxHistoryBytes: 8192}},
		{"explicit-cap", "6932", "6144", "788", SnapshotLimits{MaxBytes: 6144, BytesPerSecond: 70000}, agent.Limits{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger := &bridgeWarningLogger{}
			registrations := map[string]legacy.Factory{}
			for _, name := range []string{"first/v1", "second/v1"} {
				registrations[name] = agenthost.Factory(agenthost.Registration{Name: name, Bounds: agenthost.Bounds(tc.declared), Factory: func(agent.FactoryArgs) agent.Agent {
					t.Fatal("startup constructed an application agent")
					return nil
				}})
			}
			w, err := New(Config{SplitCheckpoints: true, SnapshotLimits: tc.limits, Agent: AgentConfig{Registrations: registrations}, LoggerFactory: bridgeWarningFactory{LoggerFactory: logging.NewDefaultLoggerFactory(), logger: logger}})
			require.NoError(t, err)
			require.NoError(t, w.Close())
			logger.mu.Lock()
			defer logger.mu.Unlock()
			require.Len(t, logger.warnings, 2)
			for name := range registrations {
				matches := 0
				for _, warning := range logger.warnings {
					if !strings.Contains(warning, `legacy calls with registration "`+name+`" may not be adoptable by this worker`) {
						continue
					}
					matches++
					require.Contains(t, warning, "needs "+tc.need+" encoded bytes")
					require.Contains(t, warning, "cap "+tc.cap+" is short by "+tc.shortfall+" bytes")
				}
				require.Equal(t, 1, matches, "one warning per registration")
			}
		})
	}
}

func TestLegacyRestoreMeasuresActualSizeOnlyOnSplitWorkers(t *testing.T) {
	original := &v2UnitAgent{state: agent.State{Version: 1, Bytes: []byte("accepted")}}
	reg := agenthost.Registration{Name: "restore-fit/v1", Bounds: agenthost.Bounds{MaxStateBytes: 2048}, Factory: func(agent.FactoryArgs) agent.Agent { return original }}
	source, err := New(Config{Agent: AgentConfig{Factory: agenthost.Factory(reg)}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	call, peer := dialDTLSCaller(t, source)
	defer func() { _ = peer.Close() }()
	data, err := source.ExportSession(call.id)
	require.NoError(t, err)
	snap, err := decodeSnapshot(data)
	require.NoError(t, err)
	require.Nil(t, snap.split)
	durable := *snap
	durable.AgentMoveOnly = nil
	encoded, err := json.Marshal(durable)
	require.NoError(t, err)
	actual := len(encoded) + snapshotEnvelopeBytes
	require.Less(t, actual, source.cfg.SnapshotLimits.MaxBytes)
	// The declared estimate does not fit even though these admitted bytes do.
	require.Error(t, validateLegacyRegistrationFit(agenthost.Factory(reg)("").(*agenthost.Adapter).Registration, source.cfg.SnapshotLimits))
	for _, tc := range []struct {
		name       string
		split      bool
		registered bool
		cap        int
		refuses    bool
	}{
		{"split-registered-oversize", true, true, actual - 1, true},
		{"split-factory-only-oversize", true, false, actual - 1, true},
		{"legacy-same-small-cap", false, false, actual - 1, false},
		{"split-exact-cap", true, true, actual, false},
	} {
		for _, kind := range []legacy.ResumeKind{legacy.PlannedMove, legacy.Takeover} {
			t.Run(fmt.Sprintf("%s/%s", tc.name, kind), func(t *testing.T) {
				created := 0
				application := &v2UnitAgent{}
				targetReg := reg
				targetReg.Factory = func(agent.FactoryArgs) agent.Agent { created++; return application }
				factory := agenthost.Factory(targetReg)
				agents := AgentConfig{Factory: factory}
				if tc.registered {
					agents.Registrations = map[string]legacy.Factory{reg.Name: factory}
				}
				target, err := New(Config{SplitCheckpoints: tc.split, SnapshotLimits: SnapshotLimits{MaxBytes: tc.cap}, Agent: agents})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, target.Close()) })
				opts := ResumeOptions{Kind: kind}
				if kind == legacy.Takeover {
					opts.SequenceMargin = 8192
				}
				_, err = target.ResumeSession(data, opts)
				if !tc.refuses {
					require.NoError(t, err)
					require.Equal(t, 1, created)
					require.Equal(t, 1, application.restores)
					require.NotNil(t, target.session(call.id))
					return
				}
				require.ErrorIs(t, err, agent.ErrIncompatibleAgent)
				require.ErrorIs(t, err, legacy.ErrRestore)
				require.ErrorIs(t, err, agent.ErrStateTooLarge)
				require.ErrorContains(t, err, fmt.Sprintf("legacy snapshot for registration %q is %d encoded bytes (cap %d, short by 1 bytes)", reg.Name, actual, tc.cap))
				require.Zero(t, created)
				require.Zero(t, application.restores)
				require.Zero(t, application.handles)
				require.Zero(t, application.saves)
				require.Zero(t, target.metrics.snapshotDebitBytes.Load())
				require.Nil(t, target.session(call.id))
				require.Zero(t, target.moveBytes.Load())
			})
		}
	}
}
