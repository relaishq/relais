package callharness

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agentbridge"
	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/workerprobe"
	legacy "github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Opt-in pinned Python/PyAV integration; the default Go test proves the move
// without codec dependencies. Full mode retains the 60 s consent threshold.
func TestPythonBridgeMoveLoopback(t *testing.T) {
	python := os.Getenv("RELAIS_TEST_PYTHON")
	if python == "" {
		t.Skip("set RELAIS_TEST_PYTHON to the pinned development venv")
	}
	python, err := filepath.Abs(python)
	require.NoError(t, err)
	disable := workerprobe.Enable()
	defer disable()
	budget := 90 * time.Second
	if testing.Short() {
		budget = 12 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	store := sessionstore.NewMemory()
	r, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	plane := controlplane.New(r, store)
	workers := make([]*mediaworker.Worker, 2)
	endpoints := make([]string, 2)
	for i := range workers {
		bridge, e := agentbridge.New(agentbridge.Config{URL: "ws://127.0.0.1:9102/bridge", Token: "isolated-loopback-token"})
		require.NoError(t, e)
		factory := agenthost.Factory(bridge.Registration())
		w, e := mediaworker.New(mediaworker.Config{SplitCheckpoints: true, Agent: mediaworker.AgentConfig{Factory: factory, Registrations: map[string]legacy.Factory{"bridge": factory}}, Relay: &mediaworker.RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}})
		require.NoError(t, e)
		defer func() { require.NoError(t, w.Close()) }()
		bridge.SetWorker(w)
		r.AddWorker(w.LocalAddr())
		require.NoError(t, plane.Register(strconv.Itoa(i), w.LocalAddr(), w))
		workerServer := httptest.NewServer(bridge)
		defer workerServer.Close()
		endpoints[i] = "ws" + strings.TrimPrefix(workerServer.URL, "http") + "/bridge"
		workers[i] = w
	}
	w := workers[0]
	cmd := exec.CommandContext(ctx, python, "-m", "relais_agent", "--packets", "150", "--worker", endpoints[0], "--worker", endpoints[1])
	cmd.Env = append(os.Environ(), "PYTHONPATH=../../python/relais-agent", "RELAIS_BRIDGE_DEV_TOKEN=isolated-loopback-token")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { cancel(); _ = cmd.Wait(); t.Log(stderr.String()) })
	scanner := bufio.NewScanner(stdout)
	require.True(t, scanner.Scan())
	var ready struct {
		Ready bool `json:"ready"`
	}
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &ready))
	require.True(t, ready.Ready)
	type sample struct {
		Sequence     uint64          `json:"seq"`
		Ready        int64           `json:"ready_ns"`
		Digest       string          `json:"digest"`
		Playback     json.RawMessage `json:"playback"`
		InputSamples int             `json:"input_samples"`
		Checkpointed json.RawMessage `json:"checkpointed"`
	}
	samples := make(chan sample, 256)
	go func() {
		defer close(samples)
		for scanner.Scan() {
			var s sample
			if json.Unmarshal(scanner.Bytes(), &s) == nil {
				samples <- s
			}
		}
	}()
	planeCtx, stopPlane := context.WithCancel(ctx)
	planeDone := make(chan struct{})
	go func() { defer close(planeDone); _ = plane.Run(planeCtx) }()
	defer func() { stopPlane(); <-planeDone }()
	server := httptest.NewServer(plane.Handler())
	defer server.Close()
	h, err := Start(Options{External: &ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: r.PublicAddr()}})
	require.NoError(t, err)
	defer func() { require.NoError(t, h.Close()) }()
	call, err := h.Dial(ctx, CallOptions{})
	require.NoError(t, err)
	received := receiveVoice(t, call)
	sending := make(chan error, 1)
	duration := 65 * time.Second
	if testing.Short() {
		duration = 5 * time.Second
	}
	go func() { sending <- call.SendMedia(ctx, duration) }()
	packets := make([]pacedPacket, 0, 150)
	var moveGaps []time.Duration
	for n := 0; n < 150; n++ {
		select {
		case p := <-received:
			packets = append(packets, p)
			if n == 59 || n == 109 {
				target := "1"
				if n == 109 {
					target = "0"
				}
				_, e := plane.Move(ctx, call.SessionID(), target)
				require.NoError(t, e)
			}
			if n == 60 || n == 110 {
				moveGaps = append(moveGaps, p.at.Sub(packets[n-1].at))
			}
		case <-ctx.Done():
			t.Fatalf("received %d/150: %s", len(packets), stderr.String())
		}
	}
	require.NoError(t, <-sending)
	observed := make(map[string][]int64)
	var savedSamples []sample
	for n := 0; n < 150; {
		select {
		case s, ok := <-samples:
			require.True(t, ok, "Python output closed")
			if s.Ready > 0 {
				observed[s.Digest] = append(observed[s.Digest], s.Ready)
				n++
			} else {
				savedSamples = append(savedSamples, s)
			}
		case <-ctx.Done():
			t.Fatal("missing Python production evidence")
		}
	}
	var delays []time.Duration
	var encoded bytes.Buffer
	for i, p := range packets {
		hash := sha256.Sum256(p.packet.Payload)
		key := hex.EncodeToString(hash[:])
		times := observed[key]
		require.NotEmpty(t, times)
		delays = append(delays, p.at.Sub(time.Unix(0, times[0])))
		observed[key] = times[1:]
		if i > 0 {
			require.Equal(t, packets[i-1].packet.SequenceNumber+1, p.packet.SequenceNumber)
			require.GreaterOrEqual(t, p.packet.Timestamp-packets[i-1].packet.Timestamp, uint32(960), "starvation may advance RTP time")
			require.LessOrEqual(t, p.at.Sub(packets[i-1].at), 500*time.Millisecond)
		}
		require.NoError(t, binary.Write(&encoded, binary.BigEndian, uint32(len(p.packet.Payload))))
		encoded.Write(p.packet.Payload)
	}
	sort.Slice(delays, func(i, j int) bool { return delays[i] < delays[j] })
	require.LessOrEqual(t, delays[142], 150*time.Millisecond, "raw Python-ready to caller delivery p95")
	decode := exec.CommandContext(ctx, python, "-c", `import sys,struct,av,math
from relais_agent.codec import decoder
b=sys.stdin.buffer.read(); c=decoder(); samples=[]
while b:
 n=struct.unpack('>I',b[:4])[0]; p=b[4:4+n]; b=b[4+n:]
 for f in c.decode(av.Packet(p)):
  channels=1 if f.format.is_planar else f.layout.nb_channels
  values=struct.unpack('<%df'%(f.samples*channels),bytes(f.planes[0])[:f.samples*channels*4])
  samples.extend(values[::channels])
crossings=sum(a<0<=b for a,b in zip(samples,samples[1:])); freq=crossings*48000/len(samples)
assert len(samples)==144000,(len(samples),freq)
assert 650<freq<670,freq
print('decoded_samples=%d frequency_hz=%.1f'%(len(samples),freq))`)
	decode.Env = cmd.Env
	decode.Stdin = &encoded
	output, err := decode.CombinedOutput()
	require.NoError(t, err, string(output))
	require.Eventually(t, func() bool {
		s, _, err := w.SessionAgent(call.SessionID())
		return err == nil && bytes.Contains(s.Bytes, []byte("dG9uZSBjb21wbGV0ZQ=="))
	}, 2*time.Second, 10*time.Millisecond)
	var playback, acknowledged bool
	inspect := func(s sample) {
		if len(s.Playback) > 0 {
			require.Contains(t, string(s.Playback), `"complete"`)
			require.Positive(t, s.InputSamples)
			playback = true
		}
		if len(s.Checkpointed) > 0 {
			var ack struct {
				Sequence uint64 `json:"seq"`
			}
			require.NoError(t, json.Unmarshal(s.Checkpointed, &ack))
			require.EqualValues(t, 1, ack.Sequence)
			acknowledged = true
		}
	}
	for _, s := range savedSamples {
		inspect(s)
	}
	for !playback || !acknowledged {
		select {
		case s, ok := <-samples:
			require.True(t, ok, "Python output closed")
			inspect(s)
		case <-ctx.Done():
			t.Fatal("missing idle playback or exact checkpoint acknowledgement")
		}
	}
	// Preserve the slice-3 real-adapter exact-export regression after the
	// two-worker move: a corrupt length must not consume the valid frozen cut.
	lease, err := store.Get(ctx, call.SessionID())
	require.NoError(t, err)
	exported, err := w.ExportSession(call.SessionID())
	require.NoError(t, err)
	var image map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(exported, &image))
	var move map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(image["AgentMoveOnly"], &move))
	var outbox []byte
	require.NoError(t, json.Unmarshal(move["Bytes"], &outbox))
	require.NotEmpty(t, outbox)
	move["Bytes"], err = json.Marshal(outbox[:len(outbox)-1])
	require.NoError(t, err)
	image["AgentMoveOnly"], err = json.Marshal(move)
	require.NoError(t, err)
	bad, err := json.Marshal(image)
	require.NoError(t, err)
	_, err = w.ResumeSession(bad, mediaworker.ResumeOptions{Lease: lease, Kind: legacy.PlannedMove})
	require.ErrorContains(t, err, "move-only length mismatch")
	resumed, err := w.ResumeSession(exported, mediaworker.ResumeOptions{Lease: lease, Kind: legacy.PlannedMove})
	require.NoError(t, err)
	require.Equal(t, call.SessionID(), resumed)
	t.Logf("local_export_bytes=%d adapter_outbox_bytes=%d corrupt_length_refused=true original_image_resumed=true", len(exported), len(outbox))
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.Zero(t, report.DecryptionFailures.Total())
	if !testing.Short() {
		require.GreaterOrEqual(t, report.Consent.ObservedFor, 60*time.Second)
		require.GreaterOrEqual(t, report.Consent.ResponsesAfter, uint64((60*time.Second)/(4*time.Second)))
		require.Less(t, report.Consent.LongestWithoutResponse, 4500*time.Millisecond)
	}
	for _, worker := range workers {
		metrics := httptest.NewRecorder()
		worker.PrivateHandler().ServeHTTP(metrics, httptest.NewRequest("GET", "/metrics", nil))
		require.Contains(t, metrics.Body.String(), "relais_worker_decryption_failures_total 0")
	}
	sort.Slice(moveGaps, func(i, j int) bool { return moveGaps[i] < moveGaps[j] })
	t.Logf("moves=2 move_gap_p95=%s (local, two samples)", moveGaps[1])
	t.Logf("packets=150 raw_p95=%s raw_max=%s %s", delays[142], delays[149], output)
	t.Logf("consent_observed=%s responses=%d max_silence=%s", report.Consent.ObservedFor, report.Consent.ResponsesAfter, report.Consent.LongestWithoutResponse)
}
