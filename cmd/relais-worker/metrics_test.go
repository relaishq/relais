package main

import (
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// Exercise the production entrypoint, before any store or media connection.
func TestMetricsListenerRejectsNonPrivateBindings(t *testing.T) {
	oldFlags, oldArgs := flag.CommandLine, os.Args
	t.Cleanup(func() { flag.CommandLine, os.Args = oldFlags, oldArgs })
	t.Setenv("RELAIS_REDIS_ADDR", "")
	for _, address := range []string{":0", "0.0.0.0:0", "[::]:0", "8.8.8.8:0", "localhost:0"} {
		t.Run(address, func(t *testing.T) {
			flag.CommandLine = flag.NewFlagSet("binding-test", flag.ContinueOnError)
			args := []string{"binding-test", "-http", address}
			args = append(args, "-name", "0", "-control", "http://127.0.0.1:1", "-relay-leg", "127.0.0.1:2", "-relay-media", "127.0.0.1:3")
			os.Args = args
			require.ErrorContains(t, run(), "private HTTP listener")
		})
	}
}
