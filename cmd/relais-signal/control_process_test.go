package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	"github.com/relaishq/relais/internal/processrun"
	"github.com/relaishq/relais/internal/redistestenv"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/workerapp"
	"github.com/stretchr/testify/require"
)

type processControlAgent struct{ IDs []agent.EventID }

func (a *processControlAgent) Handle(_ context.Context, e agent.Event) (agent.Effects, error) {
	if e.Kind == agent.Control {
		a.IDs = append(a.IDs, e.ID)
	}
	return agent.Effects{}, nil
}
func (a *processControlAgent) Save(context.Context) (agent.State, error) {
	b, err := json.Marshal(a.IDs)
	return agent.State{Version: 1, Bytes: b}, err
}
func (a *processControlAgent) Restore(_ context.Context, s agent.State) error {
	return json.Unmarshal(s.Bytes, &a.IDs)
}

// Extend the existing process-worker test pattern with a registered keyless
// Control agent. The child uses the shipped workerapp without a test HTTP shim.
func TestControlWorkerProcess(t *testing.T) {
	if os.Getenv("RELAIS_CONTROL_WORKER_HELPER") != "1" {
		return
	}
	ctx, cancel := processrun.Context()
	defer cancel()
	marker := 0
	for i, arg := range os.Args {
		if arg == "--" {
			marker = i + 1
			break
		}
	}
	if marker == 0 {
		t.Fatal("missing worker args")
	}
	err := workerapp.Run(ctx, os.Args[marker:], workerapp.Options{DefaultAgent: "control-process/v1", Agents: []workerapp.Registration{{Name: "control-process/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &processControlAgent{} }}}})
	require.NoError(t, err)
}
func TestRoutedControlRealProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("standalone signal/control/worker process gate")
	}
	addr := redistestenv.Address(t)
	if addr == "" {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			t.Fatal("dedicated Redis required")
		}
		t.Skip("dedicated Redis not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	for _, name := range []string{"relais-relay", "relais-control", "relais-signal"} {
		cmd := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(dir, name), "./cmd/"+name)
		cmd.Dir = "../.."
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	manager := &clusterprocess.Manager{}
	defer manager.Stop(false)
	// Fresh namespace and synthetic encryption key belong to this test only.
	env := append(os.Environ(), "RELAIS_REDIS_ADDR="+addr, "RELAIS_REDIS_PREFIX=control-process:"+filepath.Base(filepath.Dir(dir))+":", "RELAIS_SESSIONSTORE_KEY="+strings.Repeat("00", 32))
	start := func(name string, args ...string) (*clusterprocess.Child, clusterprocess.Ready) {
		child, err := manager.Start(dir, name, env, args...)
		require.NoError(t, err)
		readyCtx, stop := context.WithTimeout(ctx, 10*time.Second)
		defer stop()
		ready, err := child.Ready(readyCtx)
		require.NoError(t, err)
		return child, ready
	}
	_, relay := start("relay", filepath.Join(dir, "relais-relay"), "-media", "127.0.0.1:0", "-leg", "127.0.0.1:0", "-http", "127.0.0.1:0")
	controlAddr, err := clusterprocess.FreeTCP()
	require.NoError(t, err)
	controlURL := "http://" + controlAddr
	executable, err := os.Executable()
	require.NoError(t, err)
	env = append(env, "RELAIS_CONTROL_WORKER_HELPER=1")
	_, source := start("source", executable, "-test.run=^TestControlWorkerProcess$", "--", "-name", "0", "-control", controlURL, "-media", "127.0.0.1:0", "-http", "127.0.0.1:0", "-relay-leg", relay.Leg, "-relay-media", relay.Media, "-frame-cache-off")
	_, target := start("target", executable, "-test.run=^TestControlWorkerProcess$", "--", "-name", "1", "-control", controlURL, "-media", "127.0.0.1:0", "-http", "127.0.0.1:0", "-relay-leg", relay.Leg, "-relay-media", relay.Media, "-frame-cache-off")
	_, _ = start("control", filepath.Join(dir, "relais-control"), "-http", controlAddr, "-relay", relay.HTTP, "-worker", "0="+source.HTTP, "-worker", "1="+target.HTTP)
	signalAddr, err := clusterprocess.FreeTCP()
	require.NoError(t, err)
	certFile := filepath.Join(dir, "signal.pem")
	signal, err := manager.Start(dir, "signal", append(env, "RELAIS_SIGNAL_TOKEN="+testToken), filepath.Join(dir, "relais-signal"), "-listen", signalAddr, "-control", controlURL, "-advertise-ips", "127.0.0.1", "-cert-out", certFile)
	require.NoError(t, err)
	readyCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	require.NoError(t, signal.WaitLog(readyCtx, "certificate_sha256="))
	pem, err := os.ReadFile(certFile)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(pem))
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: authenticatedTransport{base: transport}, Timeout: 5 * time.Second}
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: "https://" + signalAddr + "/calls", HTTPClient: client, RelayAddr: netip.MustParseAddrPort(relay.Media)}})
	require.NoError(t, err)
	defer h.Close()
	call, err := h.Dial(ctx, callharness.CallOptions{})
	require.NoError(t, err)
	message := agent.ControlMessage{ID: "press", Sequence: 1, Name: "ptt.start"}
	endpoint := "https://" + signalAddr + "/calls/" + call.SessionID() + "/agent/control"
	var receipt agent.ControlReceipt
	require.NoError(t, privateapi.Do(ctx, client, "https://"+signalAddr, http.MethodPost, "/calls/"+call.SessionID()+"/agent/control", message, &receipt, controlplane.RemoteErrors))
	require.NotEmpty(t, receipt.EventID)
	original := receipt.EventID
	var moved controlplane.MoveResult
	require.NoError(t, privateapi.Do(ctx, http.DefaultClient, controlURL, http.MethodPost, "/calls/"+call.SessionID()+"/move?to=1", nil, &moved, controlplane.RemoteErrors))
	require.Eventually(t, func() bool {
		return privateapi.Do(ctx, client, endpoint, http.MethodPost, "", message, &receipt, controlplane.RemoteErrors) == nil && receipt.Checkpointed
	}, 3*time.Second, 10*time.Millisecond)
	require.Equal(t, original, receipt.EventID)
	var status mediaworker.WorkerStatus
	require.NoError(t, privateapi.Do(ctx, http.DefaultClient, target.HTTP, http.MethodGet, "/status?agent-session="+call.SessionID(), nil, &status, mediaworker.RemoteErrors))
	require.NotNil(t, status.AgentSession)
	var ids []agent.EventID
	require.NoError(t, json.Unmarshal(status.AgentSession.App, &ids))
	require.Equal(t, []agent.EventID{original}, ids)
	_, err = call.Hangup(ctx)
	require.NoError(t, err)
}
