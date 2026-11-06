package main

import (
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestControlClockFlagRegistersOnlyPrivateOptInRoute(t *testing.T) {
	oldFlags, oldArgs := flag.CommandLine, os.Args
	t.Cleanup(func() { flag.CommandLine, os.Args = oldFlags, oldArgs })
	for _, enabled := range []bool{false, true} {
		flag.CommandLine = flag.NewFlagSet("control-clock", flag.ContinueOnError)
		os.Args = []string{"control-clock", "-version"}
		if enabled {
			os.Args = append(os.Args, "-clock-prober")
		}
		require.NoError(t, run())
		option := flag.CommandLine.Lookup("clock-prober")
		require.NotNil(t, option)
		require.Equal(t, enabled, option.Value.(flag.Getter).Get())
		plane := controlplane.New(nil, sessionstore.NewMemory())
		handler := controlHandler(plane, nil, false, nil, testWorkerClient(), enabled)
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
