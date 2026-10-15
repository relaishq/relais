package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// advertise rewrites every ICE candidate in an SDP answer to addr.
func advertise(answer string, addr netip.AddrPort) string {
	lines := strings.Split(answer, "\r\n")
	for i, line := range lines {
		fields := strings.Fields(line)
		if strings.HasPrefix(line, "a=candidate:") && len(fields) > 5 {
			fields[4], fields[5] = addr.Addr().String(), strconv.Itoa(int(addr.Port()))
			lines[i] = strings.Join(fields, " ")
		}
	}

	return strings.Join(lines, "\r\n")
}

// TestRunChildReportsEveryDial (#122): two of three answers advertise a
// media address that never answers a connectivity check. Those dials stop
// checking after about 10 s and fail at the 20 s dial deadline as paths that
// never opened, each with a full record. The child reports every dial's
// outcome, the connected one included, and fails naming both failures.
func TestRunChildReportsEveryDial(t *testing.T) {
	system, err := callharness.Start(callharness.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = system.Close() })

	silent, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = silent.Close() })
	silentAddr := netip.MustParseAddrPort(silent.LocalAddr().String())
	var checks atomic.Int64
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := silent.ReadFrom(buf)
			if err != nil {
				return
			}
			if n >= 20 && buf[0] == 0 && buf[1] == 1 {
				checks.Add(1)
			}
		}
	}()

	target, err := url.Parse(strings.TrimSuffix(system.SignalingURL(), "/calls"))
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var answers atomic.Int64
	proxy.ModifyResponse = func(r *http.Response) error {
		if r.Request.Method != http.MethodPost || r.StatusCode != http.StatusCreated || answers.Add(1) == 1 {
			return nil
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		_ = r.Body.Close()
		rewritten := advertise(string(body), silentAddr)
		r.Body = io.NopCloser(strings.NewReader(rewritten))
		r.ContentLength = int64(len(rewritten))
		r.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
		return nil
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)

	output := filepath.Join(t.TempDir(), "caller.jsonl")
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout+20*time.Second)
	defer cancel()
	started := time.Now()
	err = RunChild(ctx, ChildOptions{
		Topology: callharness.ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: silentAddr},
		Callers:  3, Duration: time.Second, SampleEvery: time.Second, Output: output, ParentPID: os.Getppid(),
	})
	elapsed := time.Since(started)
	require.ErrorContains(t, err, "2 of 3 dials failed")
	assert.Equal(t, 2, strings.Count(err.Error(), "("+callharness.DialFailurePathNeverOpened+")"), err.Error())
	assert.GreaterOrEqual(t, elapsed, dialTimeout)
	assert.Less(t, elapsed, dialTimeout+5*time.Second, "the dial deadline still bounds a server that never answers")
	assert.GreaterOrEqual(t, checks.Load(), int64(2*45), "the checks reached the silent address")

	data, err := os.ReadFile(output)
	require.NoError(t, err)
	var connected, failed int
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var m Message
		require.NoError(t, json.Unmarshal(line, &m))
		require.Equal(t, "dial", m.Type, "only dial records before readiness")
		require.NotNil(t, m.Dial)
		assert.NotEmpty(t, m.Session)
		assert.Equal(t, m.Session, m.Dial.Session)
		require.Len(t, m.Dial.CandidatePairs, 1)
		pair := m.Dial.CandidatePairs[0]
		if m.Dial.Outcome == callharness.DialConnected {
			connected++
			assert.Empty(t, m.Error)
			assert.Positive(t, pair.ResponsesReceived)
			continue
		}
		failed++
		assert.Equal(t, callharness.DialFailed, m.Dial.Outcome)
		assert.Equal(t, callharness.DialFailurePathNeverOpened, m.Dial.Failure)
		assert.Contains(t, m.Error, "waiting for connected: context deadline exceeded")
		// The report's clock starts after the dial deadline is set (CI saw 19.9994 s).
		assert.GreaterOrEqual(t, m.Dial.Elapsed, dialTimeout-100*time.Millisecond)
		assert.Equal(t, "checking", m.Dial.ICEState)
		assert.Equal(t, "new", m.Dial.DTLSState)
		assert.Equal(t, "connecting", m.Dial.ConnectionState)
		assert.NotEmpty(t, m.Dial.ICEStates)
		assert.NotEmpty(t, m.Dial.ConnectionStates)
		assert.Equal(t, silentAddr.String(), pair.Remote)
		assert.EqualValues(t, 50, pair.ChecksSent, "about 10 s of checks at 200 ms")
		assert.Zero(t, pair.ResponsesReceived)
		assert.GreaterOrEqual(t, pair.LastCheck, 9*time.Second)
		assert.Less(t, pair.LastCheck, dialTimeout-5*time.Second, "checking ended well before the dial deadline")
	}
	assert.Equal(t, 1, connected)
	assert.Equal(t, 2, failed)

	f, err := os.Open(output)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	process, err := ReadProcess(f, os.Getpid(), 3, DefaultLimits())
	require.NoError(t, err)
	assert.Len(t, process.Dials, 3)
	assert.False(t, process.Complete)
	var dialCauses int
	for _, cause := range process.Causes {
		if strings.Contains(cause, ": "+callharness.DialFailurePathNeverOpened+": ") {
			dialCauses++
		}
	}
	assert.Equal(t, 2, dialCauses, "%v", process.Causes)
}
