// This fake transport answers the caller's mailbox protocol without a service.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/operator"
)

func main() {
	if len(os.Args) != 1 {
		os.Exit(2)
	}
	for _, entry := range os.Environ() {
		if entry != "PATH=/usr/bin:/bin" && entry != "LANG=C" {
			os.Exit(2)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		os.Exit(2)
	}
	var fixture struct {
		Manifest operatorcaller.Manifest `json:"manifest"`
		Trace    string                  `json:"trace"`
		Unknown  bool                    `json:"unknown"`
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(executable), "adapter-fixture.json"))
	if err != nil || json.Unmarshal(raw, &fixture) != nil {
		os.Exit(2)
	}
	trace, err := os.OpenFile(fixture.Trace, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	defer trace.Close()
	codec := operatortransport.NewCodec(os.Stdin, os.Stdout)
	created := map[string]bool{}
	for {
		frame, err := codec.Read()
		if err != nil {
			return
		}
		var args operatorcaller.MailboxArgs
		if frame.Request == nil || json.Unmarshal(frame.Request.Args, &args) != nil {
			os.Exit(2)
		}
		if json.NewEncoder(trace).Encode(frame) != nil {
			os.Exit(2)
		}
		found := true
		value := operatorcaller.MailboxValue{Found: &found}
		certainty := operator.Succeeded
		switch frame.Request.Operation {
		case operator.MailboxRead:
			switch {
			case strings.HasSuffix(args.Key, "/transport.json"):
				value.Data, _ = json.Marshal(fixture.Manifest)
			case strings.HasSuffix(args.Key, "/caller-token.json"):
				value.Data, _ = json.Marshal(map[string]string{"bearerToken": strings.Repeat("t", 64)})
			case strings.HasSuffix(args.Key, "/hangup.json"):
				value.Data, _ = json.Marshal(map[string]any{"schemaVersion": 1, "runId": fixture.Manifest.RunID})
			default:
				found = false
			}
		case operator.MailboxCreate:
			value = operatorcaller.MailboxValue{State: operatorcaller.Committed}
			if created[args.Key] {
				value.State = operatorcaller.Conflict
			}
			created[args.Key] = true
			if fixture.Unknown {
				certainty = operator.Unknown
			}
		default:
			os.Exit(2)
		}
		payload, _ := json.Marshal(value)
		zero := 0
		if codec.Write(operator.Frame{Type: "result", Session: frame.Session, Result: &operator.Reply{ID: frame.Request.ID, Transport: certainty, ExitCode: &zero, Payload: payload}}) != nil {
			return
		}
	}
}
