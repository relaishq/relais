package main

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFaultHookDoesNotReceiveProviderKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-provider-secret")
	t.Setenv("RELAIS_VOICE_WARM", "auth")
	d := &demo{faultHook: scriptHook(t, `[ -z "${OPENAI_API_KEY+x}" ] || exit 1
[ -z "${RELAIS_VOICE_WARM+x}" ] || exit 1
printf '%s\n' '`+validKillReceipt+`'`)}
	_, err := d.runHook(context.Background(), "kill", "a", "call")
	require.NoError(t, err)
}

func TestWorkerProviderKeyScope(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-provider-secret")
	t.Setenv("RELAIS_VOICE_WARM", "auth")
	for _, name := range []string{"echo", "demo", "custom/v1", "voice-agent/v1"} {
		env := workerProcessEnv(config{Agent: name}, processEnv("", "127.0.0.1:19386", "test:", false))
		has, warm := false, false
		for _, entry := range env {
			has = has || strings.HasPrefix(entry, "OPENAI_API_KEY=")
			warm = warm || entry == "RELAIS_VOICE_WARM=auth"
		}
		require.Equal(t, name == "voice-agent/v1", has, name)
		require.Equal(t, name == "voice-agent/v1", warm, name)
	}
}
