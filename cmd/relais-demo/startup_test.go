package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

func TestPageCallCreationAcrossLaunchRestart(t *testing.T) {
	for _, scenario := range []string{"normal", "camera", "ice"} {
		t.Run(scenario, func(t *testing.T) {
			var created atomic.Int32
			control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/status":
					privateapi.Write(w, controlplane.Status{})
				case r.Method == http.MethodPost && r.URL.Path == "/calls":
					created.Add(1)
					w.Header().Set("Location", "/calls/test")
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte("answer"))
				case r.Method == http.MethodDelete && r.URL.Path == "/calls/test":
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			}))
			defer control.Close()
			beforeID, err := newLaunchID()
			require.NoError(t, err)
			afterID, err := newLaunchID()
			require.NoError(t, err)
			require.NotEqual(t, beforeID, afterID)
			on := &demo{launchID: beforeID, ctx: context.Background(), control: control.URL, client: control.Client(), relayBuffer: "on"}
			off := &demo{launchID: afterID, ctx: context.Background(), control: control.URL, client: control.Client(), relayBuffer: "off"}
			before, after := on.handler(http.NotFoundHandler()), off.handler(http.NotFoundHandler())
			var restarted atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/test/restart" {
					restarted.Store(true)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if restarted.Load() {
					after.ServeHTTP(w, r)
				} else {
					before.ServeHTTP(w, r)
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, "node", "testdata/startup-race.cjs", server.URL, scenario).CombinedOutput()
			require.NoError(t, err, "%s", output)
			want := int32(0)
			if scenario == "normal" {
				want = 1
			}
			require.Equal(t, want, created.Load(), "mismatched launch must not reach control")
			t.Logf("%s", output)
		})
	}
}

func TestCallCreationRequiresLaunchIdentity(t *testing.T) {
	var created atomic.Int32
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		created.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer control.Close()
	d := &demo{control: control.URL, launchID: "current-launch"}
	for _, id := range []string{"", "previous-launch", d.launchID} {
		req := httptest.NewRequest(http.MethodPost, "http://localhost/calls", nil)
		req.Header.Set("X-Relais-Demo-Launch-ID", id)
		response := httptest.NewRecorder()
		d.handler(http.NotFoundHandler()).ServeHTTP(response, req)
		if id == d.launchID {
			require.Equal(t, http.StatusCreated, response.Code)
		} else {
			require.Equal(t, http.StatusConflict, response.Code)
			require.Contains(t, response.Body.String(), "demo launch changed")
		}
	}
	require.Equal(t, int32(1), created.Load())
	// A launcher without an identity cannot accept an empty client identity.
	d.launchID = ""
	response := httptest.NewRecorder()
	d.handler(http.NotFoundHandler()).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "http://localhost/calls", nil))
	require.Equal(t, http.StatusConflict, response.Code)
	require.Equal(t, int32(1), created.Load())
}
