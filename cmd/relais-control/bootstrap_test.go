package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

func TestFixedWorkerConfig(t *testing.T) {
	for _, entry := range []string{"broken", "=http://localhost:42", "w=localhost:42", "w=https://localhost", "w=http://u:p@localhost", "w=http://localhost/path", "w=http://localhost?q=x", "w=http://localhost#x", "bad/name=http://localhost"} {
		_, err := parseWorkers([]string{entry})
		require.Error(t, err, entry)
	}
	_, err := parseWorkers([]string{"w=http://localhost:42", "w=http://localhost:43"})
	require.Error(t, err)
	_, err = parseWorkers([]string{"w=http://localhost:42/", "v=http://localhost:42"})
	require.Error(t, err)
	got, err := parseWorkers([]string{"w=http://127.0.0.1:42", "v=http://[::1]:43"})
	require.NoError(t, err)
	require.Len(t, got, 2)
}

// Real control, relay and worker binaries share a dedicated Redis namespace.
// This is deliberately outside -short: it builds binaries and negotiates calls.
func TestFixedPoolTolerantBootstrapProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("real fixed-pool bootstrap processes")
	}
	addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
	if addr == "" {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			t.Fatal("dedicated Redis required")
		}
		t.Skip("dedicated Redis not configured")
	}
	require.NoError(t, redisendpoint.ValidateDev(addr))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	bin := t.TempDir()
	for _, name := range []string{"relais-control", "relais-relay", "relais-worker"} {
		cmd := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(bin, name), "./cmd/"+name)
		cmd.Dir = "../.."
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	for _, lateRelay := range []bool{false, true} {
		name := "worker-late"
		if lateRelay {
			name = "relay-late"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			manager := &clusterprocess.Manager{}
			defer manager.Stop(false)
			env := append(os.Environ(), "RELAIS_REDIS_ADDR="+addr, "RELAIS_REDIS_PREFIX=bootstrap:"+filepath.Base(dir)+":"+name+":", "RELAIS_SESSIONSTORE_KEY="+strings.Repeat("00", 32))
			start := func(name string, args ...string) *clusterprocess.Child {
				child, err := manager.Start(dir, name, env, args...)
				require.NoError(t, err)
				bounded, stop := context.WithTimeout(ctx, 8*time.Second)
				defer stop()
				_, err = child.Ready(bounded)
				require.NoError(t, err)
				return child
			}
			freeTCP := func() string { a, err := clusterprocess.FreeTCP(); require.NoError(t, err); return a }
			freeUDP := func() string { a, err := clusterprocess.FreeUDP(); require.NoError(t, err); return a }
			relayHTTP, controlHTTP, workerHTTP, lateHTTP := freeTCP(), freeTCP(), freeTCP(), freeTCP()
			leg, media := freeUDP(), freeUDP()
			startRelay := func() {
				start("relay", filepath.Join(bin, "relais-relay"), "-http", relayHTTP, "-leg", leg, "-media", media)
			}
			startWorker := func(name, httpAddr string) {
				start("worker-"+name, filepath.Join(bin, "relais-worker"), "-name", name, "-http", httpAddr, "-media", "127.0.0.1:0", "-relay-leg", leg, "-relay-media", media, "-control", "http://"+controlHTTP, "-frame-cache-off")
			}
			if !lateRelay {
				startRelay()
			}
			startWorker("a", workerHTTP)
			control := start("control", filepath.Join(bin, "relais-control"), "-http", controlHTTP, "-relay", "http://"+relayHTTP, "-worker", "a=http://"+workerHTTP, "-worker", "b=http://"+lateHTTP, "-worker-call-cap", "1")
			readStatus := func() (controlplane.Status, error) {
				var s controlplane.Status
				err := privateapi.Do(ctx, http.DefaultClient, "http://"+controlHTTP, http.MethodGet, "/status", nil, &s, nil)
				return s, err
			}
			if lateRelay {
				s, err := readStatus()
				require.NoError(t, err)
				require.False(t, s.Ready)
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+controlHTTP+"/calls", strings.NewReader("offer"))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/sdp")
				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				require.Equal(t, 409, resp.StatusCode)
				require.NoError(t, resp.Body.Close())
				startRelay()
			}
			require.Eventually(t, func() bool { s, err := readStatus(); return err == nil && s.Ready && len(s.Workers) == 1 }, 8*time.Second, 20*time.Millisecond)
			h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: "http://" + controlHTTP + "/calls", RelayAddr: netip.MustParseAddrPort(media)}})
			require.NoError(t, err)
			defer h.Close()
			first, err := h.Dial(ctx, callharness.CallOptions{Worker: -1})
			require.NoError(t, err)
			s, err := readStatus()
			require.NoError(t, err)
			require.Len(t, s.Calls, 1)
			require.Equal(t, "a", s.Calls[0].Owner)
			require.Equal(t, 1, s.Pool.Pending)
			require.Nil(t, s.Pool.Demand)
			startWorker("b", lateHTTP)
			require.Eventually(t, func() bool {
				s, err := readStatus()
				return err == nil && len(s.Workers) == 2 && s.Pool.Pending == 0 && s.Pool.Demand != nil
			}, 8*time.Second, 20*time.Millisecond)
			second, err := h.Dial(ctx, callharness.CallOptions{Worker: -1})
			require.NoError(t, err)
			s, err = readStatus()
			require.NoError(t, err)
			require.Len(t, s.Calls, 2)
			require.Equal(t, 2, *s.Pool.Demand)
			for _, call := range s.Calls {
				if call.ID == second.SessionID() {
					require.Equal(t, "b", call.Owner)
				}
			}
			body, err := json.Marshal(s.Pool)
			require.NoError(t, err)
			require.Contains(t, string(body), `"refusals"`)
			_, err = first.Hangup(ctx)
			require.NoError(t, err)
			_, err = second.Hangup(ctx)
			require.NoError(t, err)
			bounded, stop := context.WithTimeout(ctx, time.Second)
			defer stop()
			require.NoError(t, control.WaitLog(bounded, "worker bootstrap name=b attempt="))
			require.NoError(t, control.WaitLog(bounded, "relay bootstrap attempt="))
		})
	}
}
