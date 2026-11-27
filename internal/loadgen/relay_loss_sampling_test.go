package loadgen

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func TestRunChildRelayLossSamplerHelper(t *testing.T) {
	if os.Getenv("RELAIS_SAMPLER_HELPER") != "1" {
		return
	}
	err := RunChild(t.Context(), ChildOptions{
		RelayLoss: true, Callers: 4, Video: true, Duration: time.Second, SampleEvery: time.Second,
		ParentPID: os.Getppid(), Output: os.Getenv("RELAIS_SAMPLER_OUTPUT"), Desync: Desync{Seed: 41},
		Topology: callharness.ExternalTopology{SignalingURL: os.Getenv("RELAIS_SAMPLER_SIGNAL"), RelayAddr: netip.MustParseAddrPort("127.0.0.1:24780")},
	})
	require.NoError(t, err)
}

func TestRunChildRelayLossEmitsPartialFirstSample(t *testing.T) {
	system, err := callharness.Start(callharness.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, system.Close()) })
	target, err := url.Parse(strings.TrimSuffix(system.SignalingURL(), "/calls"))
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		// This test exercises caller sampling, not relay recovery. The local
		// media endpoint accepts real SDP/ICE, with no named relay topology.
		r.Out.Header.Del("Relais-Relay-Change")
		r.Out.URL.RawQuery = ""
	}}
	server := httptest.NewServer(proxy)
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunChildRelayLossSamplerHelper$")
	cmd.Env = append(os.Environ(), "RELAIS_SAMPLER_HELPER=1", "GOMAXPROCS=2", "RELAIS_SAMPLER_SIGNAL="+server.URL+"/calls", "RELAIS_SAMPLER_OUTPUT="+filepath.Join(t.TempDir(), "caller.jsonl"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	defer func() { cancel(); _ = cmd.Wait() }()
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 4096), 16<<20)
	var first *Stats
	started := false
	for scan.Scan() {
		var m struct {
			Type, HTTP string
			Stats      *Stats
		}
		if json.Unmarshal(scan.Bytes(), &m) != nil {
			continue
		}
		if m.Type == "ready" {
			raw, _ := json.Marshal(StartRequest{At: time.Now()})
			req, e := http.NewRequestWithContext(ctx, http.MethodPost, m.HTTP+"/start", bytes.NewReader(raw))
			require.NoError(t, e)
			resp, e := http.DefaultClient.Do(req)
			require.NoError(t, e)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusOK, resp.StatusCode)
			started = true
		}
		if m.Type == "stats" && first == nil {
			first = m.Stats
		}
	}
	require.NoError(t, scan.Err())
	require.NoError(t, cmd.Wait(), stderr.String())
	require.True(t, started)
	require.NotNil(t, first)
	// This is the real RunChild stream, not FirstSampleDelay's return value.
	// Seed 41's phase is ~203ms; the old wiring emits first after ~1203ms.
	require.GreaterOrEqual(t, first.Interval, 202626587*time.Nanosecond)
	require.Less(t, first.Interval, 750*time.Millisecond)
	t.Logf("firstSampleIntervalNs=%d sampleEveryNs=%d", first.Interval, time.Second)
}
