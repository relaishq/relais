package workerapp

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDevelopmentBridgeFlagGuards(t *testing.T) {
	t.Setenv("RELAIS_BRIDGE_DEV_TOKEN", "")
	for _, tc := range []struct {
		args    []string
		message string
	}{
		{[]string{"-bridge-dev-token=isolated-test-token"}, "-bridge-dev-token requires -bridge-bot-url"},
		{[]string{"-agent=bridge", "-bridge-bot-url=ws://127.0.0.1:9102/bridge", "-bridge-dev-token=isolated-test-token"}, "requires -agent=bridge and -agent-split-checkpoints"},
		{[]string{"-bridge-bot-url=ws://127.0.0.1:9102/bridge", "-bridge-dev-token=isolated-test-token"}, "requires -agent=bridge and -agent-split-checkpoints"},
		{[]string{"-agent=bridge", "-agent-split-checkpoints", "-bridge-bot-url=ws://192.168.1.1:9102/bridge", "-bridge-dev-token=isolated-test-token"}, "literal-loopback"},
		{[]string{"-agent=bridge", "-agent-split-checkpoints", "-bridge-bot-url=ws://127.0.0.1:9102/bridge"}, "development token"},
	} {
		require.ErrorContains(t, Run(context.Background(), tc.args, Options{}), tc.message)
	}
}

func TestDevelopmentBridgeTokenEnvironment(t *testing.T) {
	t.Setenv("RELAIS_BRIDGE_DEV_TOKEN", "isolated-env-test-token")
	flags, f := newWorkerFlags(Options{})
	require.NoError(t, flags.Parse([]string{"-bridge-bot-url=ws://127.0.0.1:9102/bridge"}))
	require.Equal(t, "isolated-env-test-token", f.bridgeDevelopmentToken())
	var help bytes.Buffer
	flags.SetOutput(&help)
	flags.PrintDefaults()
	require.NotContains(t, help.String(), "isolated-env-test-token")
	flags, f = newWorkerFlags(Options{})
	require.NoError(t, flags.Parse([]string{"-bridge-bot-url=ws://127.0.0.1:9102/bridge", "-bridge-dev-token=isolated-flag-test-token"}))
	require.Equal(t, "isolated-flag-test-token", f.bridgeDevelopmentToken())
}

func TestDevelopmentBridgeTokenEnvironmentDoesNotEnableBridge(t *testing.T) {
	clearWorkerTopologyEnvironment(t)
	t.Setenv("RELAIS_BRIDGE_DEV_TOKEN", "isolated-env-test-token")
	flags, f := newWorkerFlags(Options{})
	require.NoError(t, flags.Parse(nil))
	require.Empty(t, f.bridgeDevelopmentToken())
	for _, name := range []string{"echo", "demo"} {
		t.Run(name, func(t *testing.T) {
			err := Run(context.Background(), []string{"-agent=" + name, "-relay-leg=127.0.0.1:2", "-relay-media=127.0.0.1:3"}, Options{})
			require.ErrorAs(t, err, new(*configError), "plain worker must reach normal configuration validation")
		})
	}
}
