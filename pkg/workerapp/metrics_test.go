package workerapp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Exercise the production entrypoint, before any store or media connection.
func TestMetricsListenerRejectsNonPrivateBindings(t *testing.T) {
	t.Setenv("RELAIS_REDIS_ADDR", "")
	for _, address := range []string{":0", "0.0.0.0:0", "[::]:0", "8.8.8.8:0", "localhost:0"} {
		t.Run(address, func(t *testing.T) {
			args := []string{"-http", address}
			args = append(args, "-name", "0", "-control", "http://127.0.0.1:1", "-relay-leg", "127.0.0.1:2", "-relay-media", "127.0.0.1:3")
			require.ErrorContains(t, Run(context.Background(), args, Options{}), "private HTTP listener")
		})
	}
}
