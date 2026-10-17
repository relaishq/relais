package callharness

import (
	"context"
	"testing"

	"github.com/relaishq/relais/examples/voice-agent/voice"
	"github.com/relaishq/relais/internal/taskrun"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestSpeechSingleChunkCursorSkipsProvider(t *testing.T) {
	for _, kind := range []string{voice.TranscribeKind, voice.ReplyKind, voice.SpeechKind} {
		// An invalid request/key would fail if any of these single-result tasks ran.
		run := taskrun.New(agent.TaskSpec{}, "accepted", 1, nil)
		require.NoError(t, voice.Registration(voice.NewOpenAI("bad\nkey")).Tasks[kind](context.Background(), run))
	}
}
