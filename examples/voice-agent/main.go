// voice-agent is a push-to-talk worker. Only task handlers receive the API key.
package main

import (
	"fmt"
	"os"

	"github.com/relaishq/relais/examples/voice-agent/voice"
	"github.com/relaishq/relais/pkg/workerapp"
)

func main() {
	if err := voice.ValidateWarmMode(os.Getenv("RELAIS_VOICE_WARM")); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "voice-agent:", err)
		os.Exit(2)
	}
	workerapp.Main(workerapp.Options{DefaultAgent: voice.Name, Agents: []workerapp.Registration{voice.Registration(voice.NewOpenAI(os.Getenv("OPENAI_API_KEY")))}})
}
