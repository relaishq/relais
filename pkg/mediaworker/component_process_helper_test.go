//go:build !windows

package mediaworker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/privateapi"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

type splitProcessAgent struct{ value, history []byte }

func (a *splitProcessAgent) Handle(context.Context, agent.Event) (agent.Effects, error) {
	return agent.Effects{}, nil
}
func (a *splitProcessAgent) Save(context.Context) (agent.State, error) {
	return agent.State{Version: 1, Bytes: bytes.Clone(a.value), History: bytes.Clone(a.history)}, nil
}
func (a *splitProcessAgent) Restore(_ context.Context, state agent.State) error {
	if state.Version != 1 {
		return agent.ErrVersion
	}
	a.value, a.history = bytes.Clone(state.Bytes), bytes.Clone(state.History)
	data, _ := json.Marshal(map[string]any{"restored_working": a.value, "restored_history": a.history})
	fmt.Println(string(data))
	return nil
}

type splitProcessHeartbeats struct{ url, name, token string }

func (h *splitProcessHeartbeats) Heartbeat(addr netip.AddrPort) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var reply struct {
		Token string `json:"token"`
	}
	err := privateapi.Do(ctx, http.DefaultClient, h.url, http.MethodPost, "/private/workers/"+h.name+"/heartbeat", struct {
		Address netip.AddrPort `json:"address"`
		Token   string         `json:"token"`
	}{addr, h.token}, &reply, nil)
	if err != nil {
		return err
	}
	h.token = reply.Token
	if h.token != "" {
		return ErrRejoinRequired
	}
	return nil
}

// This subprocess uses production Worker, private HTTP, ownership, writer and
// DTLS code. The agent supplies SDK History through Save. The target has no
// history source except its SDK Restore callback.
func TestSplitCheckpointWorkerProcess(t *testing.T) {
	if os.Getenv("RELAIS_SPLIT_PROCESS_HELPER") != "1" {
		t.Skip("subprocess fixture")
	}
	ctx := context.Background()
	cfg := storage.RedisConfig{Addr: os.Getenv("RELAIS_SPLIT_PROCESS_REDIS"), Prefix: os.Getenv("RELAIS_SPLIT_PROCESS_PREFIX")}
	if strings.Contains(cfg.Addr, ",") {
		cfg.Addrs = strings.Split(cfg.Addr, ",")
		cfg.Addr = ""
		cfg.Cluster = true
	}
	store, err := sessionstore.NewRedis(ctx, cfg, make([]byte, 32))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	leg, err := netip.ParseAddrPort(os.Getenv("RELAIS_SPLIT_PROCESS_LEG"))
	require.NoError(t, err)
	public, err := netip.ParseAddrPort(os.Getenv("RELAIS_SPLIT_PROCESS_PUBLIC"))
	require.NoError(t, err)
	w, err := New(Config{SplitCheckpoints: os.Getenv("RELAIS_SPLIT_PROCESS_ADMIT") == "1", DisableFrameCache: true, Agent: AgentConfig{Factory: agenthost.Factory(agenthost.Registration{Name: "history/v1", Factory: func(agent.FactoryArgs) agent.Agent {
		return &splitProcessAgent{value: []byte(os.Getenv("RELAIS_SPLIT_PROCESS_WORKING")), history: []byte(os.Getenv("RELAIS_SPLIT_PROCESS_HISTORY"))}
	}})}, Relay: &RelayConfig{Owners: store, Addr: leg, PublicAddr: public}})
	require.NoError(t, err)
	defer func() { _ = w.Close() }()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := http.Server{Handler: w.PrivateHandler(), ReadHeaderTimeout: time.Second}
	defer func() { _ = server.Close() }()
	go func() { _ = server.Serve(listener) }()
	ready, _ := json.Marshal(map[string]string{"http": "http://" + listener.Addr().String(), "media": w.LocalAddr().String()})
	fmt.Println(string(ready))
	w.StartHeartbeats(&splitProcessHeartbeats{url: os.Getenv("RELAIS_SPLIT_PROCESS_CONTROL"), name: os.Getenv("RELAIS_SPLIT_PROCESS_NAME")})
	// Parent owns SIGKILL and reap. Do not turn a subprocess fixture into a
	// normal test that can accidentally exit before the production takeover.
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	<-timer.C
}
