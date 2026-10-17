package callharness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/relaishq/relais/examples/voice-agent/voice"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

type voiceLaunchAuth struct{ token, launchID string }

func (a voiceLaunchAuth) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	clone.Header.Set("X-Relais-Demo-Token", a.token)
	clone.Header.Set("X-Relais-Demo-Launch-ID", a.launchID)
	return http.DefaultTransport.RoundTrip(clone)
}

// This separate opt-in proves the shipped binary's real OS SIGKILL path. It
// uses the launcher-owned pool, private ports and no human browser/profile.
func TestVoiceAgentLiveProcessKill(t *testing.T) {
	if os.Getenv("RELAIS_VOICE_LIVE_PROCESS") != "1" || os.Getenv("CI") != "" {
		t.Skip("opt-in local real-process provider cut")
	}
	ledger := os.Getenv("RELAIS_VOICE_LEDGER")
	require.NotEmpty(t, ledger)
	key := os.Getenv("OPENAI_API_KEY")
	if file := os.Getenv("RELAIS_VOICE_LIVE_ENV"); file != "" {
		b, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "OPENAI_API_KEY=") {
				key = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "OPENAI_API_KEY=")), "\"'")
				break
			}
		}
	}
	require.NotEmpty(t, key, "worker key unavailable")
	budget := &liveVoiceProvider{ledger: ledger}
	require.NoError(t, budget.reserve("gpt-transcribe / gpt-6-luna / gpt-4o-mini-tts", "real SIGKILL cut; conservative whole-turn envelope", 0.05))
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.Command(filepath.Join(root, "bin/relais-demo"), "-bin", filepath.Join(root, "bin"), "-http", "127.0.0.1:0", "-worker-bin", filepath.Join(root, "bin/voice-agent"), "-agent", voice.Name)
	cmd.Dir = root
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "OPENAI_API_KEY=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "OPENAI_API_KEY="+key)
	output, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = io.Discard
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	type launch struct {
		base, token string
		relay       netip.AddrPort
	}
	ready := make(chan launch, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		var relayAddr netip.AddrPort
		for scanner.Scan() {
			line := scanner.Text()
			var value struct {
				Media netip.AddrPort `json:"media"`
			}
			if json.Unmarshal(scanner.Bytes(), &value) == nil && value.Media.IsValid() {
				relayAddr = value.Media
			}
			if strings.HasPrefix(line, "cluster demo: ") {
				u, e := url.Parse(strings.TrimPrefix(line, "cluster demo: "))
				if e == nil {
					fragment, _ := url.ParseQuery(u.Fragment)
					token := fragment.Get("token")
					u.Fragment = ""
					u.RawQuery = ""
					ready <- launch{strings.TrimRight(u.String(), "/"), token, relayAddr}
				}
			}
		}
	}()
	var launched launch
	select {
	case launched = <-ready:
	case <-ctx.Done():
		t.Fatal("process demo readiness unavailable")
	}
	auth := voiceLaunchAuth{token: launched.token}
	client := &http.Client{Transport: auth, Timeout: 15 * time.Second}
	response, err := client.Get(launched.base + "/demo/status")
	require.NoError(t, err)
	var status struct {
		LaunchID string `json:"launchId"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&status))
	require.NoError(t, response.Body.Close())
	auth.launchID = status.LaunchID
	client.Transport = auth
	h, err := Start(Options{External: &ExternalTopology{SignalingURL: launched.base + "/calls", RelayAddr: launched.relay, HTTPClient: client}})
	require.NoError(t, err)
	defer h.Close()
	call, err := h.Dial(ctx, CallOptions{})
	require.NoError(t, err)
	id := call.SessionID()
	control := func(sequence uint64, name string) {
		message := agent.ControlMessage{ID: fmt.Sprintf("%s-%d", name, sequence), Sequence: sequence, Name: name}
		body, err := json.Marshal(message)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			r, e := client.Post(launched.base+"/calls/"+id+"/agent/control", "application/json", strings.NewReader(string(body)))
			if e != nil {
				return false
			}
			_ = r.Body.Close()
			return r.StatusCode == http.StatusOK
		}, 3*time.Second, 20*time.Millisecond)
	}
	read := func() voice.State {
		r, e := client.Get(launched.base + "/demo/voice/" + id)
		if e != nil {
			return voice.State{}
		}
		defer r.Body.Close()
		var s voice.State
		_ = json.NewDecoder(r.Body).Decode(&s)
		return s
	}
	control(1, "ptt.start")
	packets := pacerFixtures(t, 80)
	packets = append(packets, packets...)
	for _, packet := range packets {
		require.NoError(t, call.audio.WriteSample(media.Sample{Data: packet, Duration: 20 * time.Millisecond}))
		time.Sleep(20 * time.Millisecond)
	}
	released := time.Now()
	control(2, "ptt.stop")
	require.Eventually(t, func() bool { return read().Phase == "transcribing" }, time.Second, 5*time.Millisecond)
	response, err = client.Post(launched.base+"/demo/kill", "application/json", strings.NewReader(`{"id":"`+id+`"}`))
	require.NoError(t, err)
	var action struct {
		PID   int    `json:"pid"`
		Error string `json:"error"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&action))
	require.NoError(t, response.Body.Close())
	require.Empty(t, action.Error)
	require.Positive(t, action.PID)
	require.Eventually(t, func() bool {
		s := read()
		return s.Phase == "idle" && s.Outcome == agent.OutcomeUnknown && s.Error == "turn_interrupted"
	}, 5*time.Second, 20*time.Millisecond)
	t.Logf("LIVE real SIGKILL release_to_notice_and_replacement=%s", time.Since(released))
	control(3, "ptt.start")
	require.Equal(t, "recording", read().Phase)
	_, err = call.Hangup(ctx)
	require.NoError(t, err)
}
