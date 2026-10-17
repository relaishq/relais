// voice-agent is a push-to-talk worker. Only task handlers receive the API key.
package main

import (
	"os"

	"github.com/relaishq/relais/examples/voice-agent/voice"
	"github.com/relaishq/relais/pkg/workerapp"
)

func main() {
	workerapp.Main(workerapp.Options{DefaultAgent: voice.Name, Agents: []workerapp.Registration{voice.Registration(voice.NewOpenAI(os.Getenv("OPENAI_API_KEY")))}})
}
