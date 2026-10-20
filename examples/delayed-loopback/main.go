// delayed-loopback is a drop-in relais-worker with a keyless v2 registration.
package main

import (
	"os"
	"strconv"
	"strings"

	"github.com/relaishq/relais/examples/delayed-loopback/history"
	"github.com/relaishq/relais/examples/delayed-loopback/ledger"
	"github.com/relaishq/relais/examples/delayed-loopback/loopback"
	"github.com/relaishq/relais/pkg/workerapp"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "history" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		workerapp.Main(workerapp.Options{DefaultAgent: history.Name, Agents: []workerapp.Registration{history.Registration(splitCheckpoints(os.Args[1:]), false), history.Registration(splitCheckpoints(os.Args[1:]), true)}})
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "ledger" {
		ledger.Main(os.Args[2:])
		return
	}
	workerapp.Main(workerapp.Options{DefaultAgent: loopback.Name, Agents: []workerapp.Registration{loopback.Registration(), ledger.Registration(os.Getenv("DELAYED_LOOPBACK_LEDGER_URL")), ledger.StallRegistration(os.Getenv("DELAYED_LOOPBACK_LEDGER_URL"))}})
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
