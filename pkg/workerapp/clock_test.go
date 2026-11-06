package workerapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func TestWorkerClockFlagRegistersOnlyPrivateOptInRoute(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		flags, f := newWorkerFlags(Options{})
		var args []string
		if enabled {
			args = []string{"-clock-prober"}
		}
		require.NoError(t, flags.Parse(args))
		require.Equal(t, enabled, *f.clockProber)
		// Building the private HTTP routes needs no media sockets or store.
		handler := agentHandler(&mediaworker.Worker{}, "echo", *f.clockProber)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/clock", nil))
		if !enabled {
			require.Equal(t, http.StatusNotFound, response.Code)
			continue
		}
		require.Equal(t, http.StatusOK, response.Code)
		var sample map[string]string
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &sample))
		require.Len(t, sample, 2)
		for _, field := range []string{"received_at", "sent_at"} {
			_, err := time.Parse(time.RFC3339Nano, sample[field])
			require.NoError(t, err)
			require.Contains(t, sample[field], "Z")
		}
	}
}
