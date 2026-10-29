// relais-gates runs one operator session over JSON Lines on stdin/stdout.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/buildinfo"
	"github.com/relaishq/relais/pkg/operator"
)

func main() { os.Exit(run()) }
func run() int {
	hello := operator.Hello{Protocol: operator.ProtocolVersion, Version: buildinfo.Version, Commit: buildinfo.Commit, ScenarioSchema: 1, ResultSchema: 1}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: relais-gates session [-spool DIR]|caller|rehearse|version")
		return 2
	}
	switch os.Args[1] {
	case "version":
		if len(os.Args) != 2 {
			return 2
		}
		if operatortransport.NewCodec(os.Stdin, os.Stdout).Write(operator.Frame{Type: "hello", Hello: &hello}) != nil {
			return 1
		}
		return 0
	case "rehearse":
		return rehearse(os.Args[2:])
	case "caller-host":
		if len(os.Args) != 2 {
			return 2
		}
		host, err := operatorcaller.CollectHost(context.Background())
		if err != nil {
			return 1
		}
		if json.NewEncoder(os.Stdout).Encode(host) != nil {
			return 1
		}
		return 0
	case "caller":
		flags := flag.NewFlagSet("caller", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		config := flags.String("config", "", "caller configuration")
		mailbox := flags.String("mailbox", "", "scoped directory mailbox")
		load := flags.String("load", "", "verified relais-load executable")
		adapter := flags.String("adapter", "", "absolute mailbox adapter executable")
		spool := flags.String("spool", "", "agreed mailbox blob spool")
		expected := flags.String("identity", "", "preallocated identity JSON")
		if flags.Parse(os.Args[2:]) != nil || flags.NArg() != 0 {
			return 2
		}
		raw, err := os.ReadFile(*config)
		if err != nil {
			operatorcaller.WriteFailure(os.Stderr, err)
			return 1
		}
		var c operatorcaller.BridgeConfig
		if observe.Decode(raw, &c, observe.Contract{"identity", "bind", "certificateFile", "loadSHA256"}) != nil {
			fmt.Fprintln(os.Stderr, "caller-incomplete: caller-config-invalid")
			return 1
		}
		if *expected != "" {
			var i operatorcaller.Identity
			if observe.Decode([]byte(*expected), &i, nil) != nil || i != c.Identity {
				fmt.Fprintln(os.Stderr, "caller-incomplete: caller-identity-differs")
				return 1
			}
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		var box operatorcaller.Store
		if (*mailbox == "") == (*adapter == "") {
			return 2
		}
		if *adapter != "" {
			var root *os.Root
			if *spool != "" {
				root, err = os.OpenRoot(*spool)
				if err != nil {
					return 1
				}
				defer root.Close()
			}
			peer, e := operatortransport.Start(ctx, *adapter, nil)
			if e != nil {
				return 1
			}
			defer func() { cancel(); _ = peer.Close(); _ = peer.Wait() }()
			box = &operatorcaller.ProtocolMailbox{Peer: peer, Session: c.Identity.CallerID, Spool: root, End: time.Now().Add(20 * time.Minute)}
		} else {
			directory, e := operatorcaller.OpenDirectory(*mailbox)
			if e != nil {
				operatorcaller.WriteFailure(os.Stderr, e)
				return 1
			}
			defer directory.Close()
			box = directory
		}
		if err := operatorcaller.RunBridge(ctx, c, box, *load); err != nil {
			operatorcaller.WriteFailure(os.Stderr, err)
			return 1
		}
		return 0
	case "session":
		flags := flag.NewFlagSet("session", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		directory := flags.String("spool", "", "agreed local blob spool")
		if flags.Parse(os.Args[2:]) != nil || flags.NArg() != 0 {
			return 2
		}
		var root *os.Root
		if *directory != "" {
			var err error
			root, err = os.OpenRoot(*directory)
			if err != nil {
				fmt.Fprintln(os.Stderr, "operator-spool-unavailable")
				return 1
			}
			defer root.Close()
		}

		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if operatorgates.Serve(ctx, operatortransport.NewPipe(os.Stdin, os.Stdout), hello, nil, root) != nil {
			fmt.Fprintln(os.Stderr, "operator-session-incomplete")
			return 1
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, "unknown command")
		return 2
	}
}
