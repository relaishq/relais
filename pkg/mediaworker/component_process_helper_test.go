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

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

type splitProcessAgent struct{ value []byte }

func (a *splitProcessAgent) Process(_ context.Context, in agent.Input) (agent.Output, error) {
	return agent.Output{Audio: in.Payload}, nil
}
func (a *splitProcessAgent) Save(context.Context) (agent.State, error) {
	return agent.State{Version: 1, Bytes: bytes.Clone(a.value)}, nil
}
func (a *splitProcessAgent) Restore(_ context.Context, state agent.State) error {
	if state.Version != 1 {
		return agent.ErrVersion
	}
	a.value = bytes.Clone(state.Bytes)
	data, _ := json.Marshal(map[string]any{"restored_working": a.value})
	fmt.Println(string(data))
	return nil
}
func (*splitProcessAgent) Resume(context.Context, agent.ResumeNotice) error { return nil }

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
// DTLS code. Only the bounded history fixture is supplied by this test, ahead
// of the separate SDK History slice. The target has no history fixture source.
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
	w, err := New(Config{SplitCheckpoints: os.Getenv("RELAIS_SPLIT_PROCESS_ADMIT") == "1", DisableFrameCache: true, Agent: AgentConfig{Factory: func(string) agent.Agent {
		return &splitProcessAgent{value: []byte(os.Getenv("RELAIS_SPLIT_PROCESS_WORKING"))}
	}}, Relay: &RelayConfig{Owners: store, Addr: leg, PublicAddr: public}})
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
	if history := os.Getenv("RELAIS_SPLIT_PROCESS_HISTORY"); history != "" {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			w.mu.Lock()
			var target *session
			for _, s := range w.sessions {
				target = s
				break
			}
			w.mu.Unlock()
			if target == nil {
				continue
			}
			target.mu.Lock()
			target.componentHistory = []byte(history)
			target.mu.Unlock()
			target.wantSnapshot()
			break
		}
	}
	// Parent owns SIGKILL and reap. Do not turn a subprocess fixture into a
	// normal test that can accidentally exit before the production takeover.
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	<-timer.C
}
