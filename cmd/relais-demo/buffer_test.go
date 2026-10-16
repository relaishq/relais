package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

func TestRelayBufferArgumentsAndPageStatus(t *testing.T) {
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/status", r.URL.Path)
		privateapi.Write(w, controlplane.Status{})
	}))
	defer control.Close()
	for _, mode := range []string{"", "on", "off"} {
		t.Run("mode="+mode, func(t *testing.T) {
			var cfg config
			flags := flag.NewFlagSet("relais-demo", flag.ContinueOnError)
			configureFlags(flags, &cfg)
			args := []string{"-http=127.0.0.1:0"}
			if mode != "" {
				args = append(args, "-relay-buffer="+mode)
			}
			require.NoError(t, flags.Parse(args))
			require.NoError(t, validateConfig(cfg))
			if mode == "off" {
				require.Contains(t, relayArgs(cfg), "-buffer-off")
			} else {
				require.NotContains(t, relayArgs(cfg), "-buffer-off")
			}
			d := &demo{ctx: context.Background(), control: control.URL, client: control.Client(), relayBuffer: selectedRelayBuffer(cfg)}
			server := httptest.NewServer(d.handler(http.NotFoundHandler()))
			defer server.Close()
			var status struct {
				RelayBuffer string `json:"relayBuffer"`
			}
			require.NoError(t, privateapi.Do(context.Background(), server.Client(), server.URL, http.MethodGet, "/demo/status", nil, &status, nil))
			want := "on"
			if mode == "off" {
				want = "off"
			}
			require.Equal(t, want, status.RelayBuffer)
		})
	}
}

func TestRelayBufferRejectsInvalidModeBeforeListening(t *testing.T) {
	for _, mode := range []string{"ON", "false", "disabled"} {
		listener, _, err := listenPage(config{HTTP: "127.0.0.1:0", RelayBuffer: mode})
		require.ErrorContains(t, err, "-relay-buffer must be on or off")
		require.Nil(t, listener)
	}
}

func TestRelayBufferFlagRejectsExplicitEmpty(t *testing.T) {
	var cfg config
	flags := flag.NewFlagSet("relais-demo", flag.ContinueOnError)
	configureFlags(flags, &cfg)
	require.ErrorContains(t, flags.Parse([]string{"-relay-buffer="}), "-relay-buffer must be on or off")
}
