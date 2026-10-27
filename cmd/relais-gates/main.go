// relais-gates runs one operator session over JSON Lines on stdin/stdout.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/buildinfo"
	"github.com/relaishq/relais/pkg/operator"
)

func main() { os.Exit(run()) }
func run() int {
	hello := operator.Hello{Protocol: operator.ProtocolVersion, Version: buildinfo.Version, Commit: buildinfo.Commit, ScenarioSchema: 1, ResultSchema: 1}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: relais-gates session [-spool DIR]|version")
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
