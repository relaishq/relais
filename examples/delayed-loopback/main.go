// delayed-loopback is a drop-in relais-worker with a keyless v2 registration.
package main

import (
	"os"

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

func splitCheckpoints(args []string) bool { return workerapp.SplitCheckpoints(args) }
