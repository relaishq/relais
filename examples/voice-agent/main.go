// voice-agent is a push-to-talk worker. Only task handlers receive the API key.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/relaishq/relais/examples/voice-agent/voice"
	"github.com/relaishq/relais/pkg/workerapp"
)

func main() {
	if err := voice.ValidateWarmMode(os.Getenv("RELAIS_VOICE_WARM")); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "voice-agent:", err)
		os.Exit(2)
	}
	workerapp.Main(workerapp.Options{DefaultAgent: voice.Name, Agents: []workerapp.Registration{voice.Registration(voice.NewOpenAI(os.Getenv("OPENAI_API_KEY")), splitCheckpoints(os.Args[1:]))}})
}

// Match Go boolean flag spelling and last-value semantics without consuming the
// worker's arguments. workerapp remains responsible for validation and errors.
func splitCheckpoints(args []string) bool {
	enabled := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || !strings.HasPrefix(arg, "-") || arg == "-" {
			break
		}
		name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
		key, value, hasValue := strings.Cut(name, "=")
		if key == "agent-split-checkpoints" {
			enabled = true
			if hasValue {
				enabled, _ = strconv.ParseBool(value)
			}
			continue
		}
		// These worker flags are boolean. All remaining valid flags take a value.
		switch key {
		case "frame-cache-off", "srtp-diagnostics", "version", "h", "help", "redis-cluster":
		default:
			if !hasValue {
				i++
			}
		}
	}
	return enabled
}
